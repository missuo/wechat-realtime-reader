// Package snapshot turns an encrypted WeChat database (main file + WAL) into
// a published plaintext SQLite snapshot.
//
// Each refresh:
//  1. records the main/WAL metadata, reads the WAL and the needed main pages,
//     and re-checks the metadata — a change in between means retry, never a
//     mixed snapshot;
//  2. overlays the last committed WAL transaction on the main file;
//  3. compares every candidate page's IV+HMAC tag with the snapshot's tag and
//     authenticates + decrypts only the pages that changed;
//  4. clones the current snapshot (APFS clonefile), patches the changed pages
//     into the clone and atomically renames it over "current".
//
// Any authentication failure fails the whole refresh; the previous snapshot
// stays in place and is reported as stale.
package snapshot

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/missuo/wechat-realtime-reader/internal/crypto"
	"github.com/missuo/wechat-realtime-reader/internal/wal"

	_ "modernc.org/sqlite"
)

const ps = crypto.PageSize

var (
	errUnstable   = errors.New("source changed during read")
	ErrKeyInvalid = errors.New("key does not authenticate page 1 (salt/key rotated?)")
)

type fileSig struct {
	Exists bool
	Ino    uint64
	Size   int64
	Mtime  time.Time
	Ctime  time.Time
}

func stat(p string) (fileSig, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fileSig{}, nil
		}
		return fileSig{}, err
	}
	return fileSig{
		Exists: true,
		Ino:    st.Ino,
		Size:   st.Size,
		Mtime:  time.Unix(st.Mtimespec.Unix()),
		Ctime:  time.Unix(st.Ctimespec.Unix()),
	}, nil
}

func (s fileSig) equal(o fileSig) bool {
	return s.Exists == o.Exists && s.Ino == o.Ino && s.Size == o.Size &&
		s.Mtime.Equal(o.Mtime) && s.Ctime.Equal(o.Ctime)
}

// Result reports one refresh.
type Result struct {
	Name         string
	Pages        int
	ChangedPages int
	WALFrames    int
	Full         bool
	Published    bool
	SourceTime   time.Time
	Duration     time.Duration
}

// DB is one source database and its published snapshot.
type DB struct {
	Name string // relative path, e.g. "message/message_0.db"
	src  string
	dst  string
	key  []byte

	mu      sync.Mutex
	cipher  *crypto.Cipher
	salt    []byte
	tags    []byte // per page IV+HMAC of the published snapshot
	fromWAL map[uint32]bool
	mainSig fileSig
	walSig  fileSig
	loaded  bool
}

// NewDB prepares a database; nothing is read until Refresh.
func NewDB(name, srcBase, dstBase string, key []byte) *DB {
	return &DB{
		Name: name,
		src:  filepath.Join(srcBase, name),
		dst:  filepath.Join(dstBase, name),
		key:  key,
	}
}

// SnapshotPath is the published plaintext file.
func (d *DB) SnapshotPath() string { return d.dst }

// SourcePaths returns the files whose changes should trigger a refresh.
func (d *DB) SourcePaths() []string { return []string{d.src, d.src + "-wal"} }

// SourceSigs returns the current metadata of the source files.
func (d *DB) sourceSigs() (fileSig, fileSig, error) {
	m, err := stat(d.src)
	if err != nil {
		return m, fileSig{}, err
	}
	w, err := stat(d.src + "-wal")
	return m, w, err
}

// Changed reports whether the source differs from the last published state.
func (d *DB) Changed() bool {
	m, w, err := d.sourceSigs()
	if err != nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.loaded || !m.equal(d.mainSig) || !w.equal(d.walSig)
}

// Refresh brings the snapshot up to date. It is safe to call concurrently;
// calls are serialized per database.
func (d *DB) Refresh(ctx context.Context) (*Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var last error
	for attempt := 0; attempt < 8; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res, err := d.refreshOnce()
		if !errors.Is(err, errUnstable) {
			return res, err
		}
		last = err
		time.Sleep(time.Duration(30*(attempt+1)) * time.Millisecond)
	}
	return nil, last
}

func (d *DB) refreshOnce() (*Result, error) {
	start := time.Now()
	m0, w0, err := d.sourceSigs()
	if err != nil {
		return nil, err
	}
	if !m0.Exists {
		return nil, fmt.Errorf("%s: source missing", d.Name)
	}
	if j, _ := stat(d.src + "-journal"); j.Exists && j.Size > 0 {
		// A hot rollback journal means a transaction is in flight.
		return nil, errUnstable
	}

	f, err := os.Open(d.src)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	page1 := make([]byte, ps)
	if _, err := f.ReadAt(page1, 0); err != nil {
		return nil, fmt.Errorf("%s: read page 1: %w", d.Name, err)
	}
	if d.cipher == nil || !bytes.Equal(d.salt, crypto.Salt(page1)) {
		c, _, err := crypto.Open(d.key, page1)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Name, ErrKeyInvalid)
		}
		d.cipher, d.salt = c, append([]byte(nil), crypto.Salt(page1)...)
		d.tags = nil // different salt ⇒ every page differs
	}

	var walBuf []byte
	if w0.Exists && w0.Size > 0 {
		walBuf, err = os.ReadFile(d.src + "-wal")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	wr, err := wal.Parse(walBuf, ps)
	if err != nil {
		// A torn header during WAL reset; retry.
		return nil, errUnstable
	}

	n := int(m0.Size / ps)
	if wr.DBPages > 0 {
		n = int(wr.DBPages)
	}
	if n == 0 {
		return nil, fmt.Errorf("%s: empty database", d.Name)
	}

	if !d.loaded {
		d.loadTags()
	}
	full := d.tags == nil
	res := &Result{Name: d.Name, Pages: n, WALFrames: wr.Frames, SourceTime: latest(m0.Mtime, w0.Mtime)}

	var changes map[uint32][]byte
	if !full {
		changes, err = d.diff(f, walBuf, wr, n, !m0.equal(d.mainSig))
		if err != nil {
			return nil, err
		}
		if len(changes) > n/2 {
			full = true
			changes = nil
		}
	}

	var newTags []byte
	var tmp string
	if full {
		tmp, newTags, err = d.buildFull(f, walBuf, wr, n)
	} else if len(changes) > 0 || len(d.tags) != n*crypto.TagSize {
		tmp, newTags, err = d.buildPatched(changes, n)
	}
	if err != nil {
		return nil, err
	}

	// Stability check: anything moved while we were reading ⇒ discard.
	m1, w1, err := d.sourceSigs()
	if err != nil || !m0.equal(m1) || !w0.equal(w1) {
		if tmp != "" {
			os.Remove(tmp)
		}
		return nil, errUnstable
	}

	if tmp != "" {
		if full {
			if err := quickCheck(tmp); err != nil {
				os.Remove(tmp)
				return nil, fmt.Errorf("%s: quick_check: %w", d.Name, err)
			}
		}
		if err := os.Rename(tmp, d.dst); err != nil {
			os.Remove(tmp)
			return nil, err
		}
		d.tags = newTags
		res.Published = true
	}
	d.fromWAL = make(map[uint32]bool, len(wr.Pages))
	for p := range wr.Pages {
		d.fromWAL[p] = true
	}
	d.mainSig, d.walSig, d.loaded = m0, w0, true
	res.Full = full
	res.ChangedPages = len(changes)
	if full {
		res.ChangedPages = n
	}
	res.Duration = time.Since(start)
	return res, nil
}

// loadTags reads the per-page tags of an existing snapshot so a restart
// does not need to re-decrypt unchanged pages.
func (d *DB) loadTags() {
	d.loaded = true
	f, err := os.Open(d.dst)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size()%ps != 0 || st.Size() == 0 {
		return
	}
	n := int(st.Size() / ps)
	tags := make([]byte, n*crypto.TagSize)
	buf := make([]byte, 256*ps)
	for p := 0; p < n; {
		k, err := f.ReadAt(buf, int64(p)*ps)
		if k < ps && err != nil {
			return
		}
		for i := 0; i+ps <= k && p < n; i, p = i+ps, p+1 {
			copy(tags[p*crypto.TagSize:], crypto.Tag(buf[i:i+ps]))
		}
	}
	d.tags = tags
	d.mainSig = fileSig{} // force a full compare against the source
}

func (d *DB) tagAt(pgno uint32) []byte {
	i := int(pgno-1) * crypto.TagSize
	if i+crypto.TagSize > len(d.tags) {
		return nil
	}
	return d.tags[i : i+crypto.TagSize]
}

// diff returns decrypted pages whose ciphertext differs from the snapshot.
func (d *DB) diff(f *os.File, walBuf []byte, wr *wal.Result, n int, mainChanged bool) (map[uint32][]byte, error) {
	changes := map[uint32][]byte{}
	dec := d.cipher.Decryptor()
	consider := func(pgno uint32, enc []byte) error {
		if t := d.tagAt(pgno); t != nil && bytes.Equal(t, crypto.Tag(enc)) {
			// Same IV+HMAC as the snapshot; still authenticate, so a page
			// whose ciphertext was altered under an old tag fails loudly.
			if err := dec.Verify(enc, pgno); err != nil {
				return fmt.Errorf("%s: %w", d.Name, err)
			}
			return nil
		}
		out := make([]byte, ps)
		if err := dec.DecryptPage(out, enc, pgno); err != nil {
			return fmt.Errorf("%s: %w", d.Name, err)
		}
		changes[pgno] = out
		return nil
	}

	// WAL pages always win.
	for pgno, off := range wr.Pages {
		if int(pgno) > n {
			continue
		}
		if err := consider(pgno, walBuf[off:off+ps]); err != nil {
			return nil, err
		}
	}

	if mainChanged {
		buf := make([]byte, 256*ps)
		for p := 0; p < n; {
			k, err := f.ReadAt(buf, int64(p)*ps)
			if k < ps {
				if err == io.EOF {
					break
				}
				return nil, err
			}
			for i := 0; i+ps <= k && p < n; i, p = i+ps, p+1 {
				pgno := uint32(p + 1)
				if _, ok := wr.Pages[pgno]; ok {
					continue
				}
				if err := consider(pgno, buf[i:i+ps]); err != nil {
					return nil, err
				}
			}
		}
		return changes, nil
	}

	// Main unchanged: only pages that left the WAL, or grew past the old
	// size without being in the WAL, need a main read.
	page := make([]byte, ps)
	oldN := uint32(len(d.tags) / crypto.TagSize)
	check := func(pgno uint32) error {
		if int(pgno) > n {
			return nil
		}
		if _, ok := wr.Pages[pgno]; ok {
			return nil
		}
		if _, err := f.ReadAt(page, int64(pgno-1)*ps); err != nil {
			return errUnstable
		}
		return consider(pgno, page)
	}
	for pgno := range d.fromWAL {
		if err := check(pgno); err != nil {
			return nil, err
		}
	}
	for pgno := oldN + 1; int(pgno) <= n; pgno++ {
		if err := check(pgno); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

func (d *DB) tmpPath() string {
	return fmt.Sprintf("%s.tmp-%d-%d", d.dst, os.Getpid(), time.Now().UnixNano())
}

// buildFull decrypts every page into a fresh temp file using all cores.
func (d *DB) buildFull(f *os.File, walBuf []byte, wr *wal.Result, n int) (string, []byte, error) {
	if err := os.MkdirAll(filepath.Dir(d.dst), 0o700); err != nil {
		return "", nil, err
	}
	tmp := d.tmpPath()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, err
	}
	fail := func(err error) (string, []byte, error) {
		out.Close()
		os.Remove(tmp)
		return "", nil, err
	}
	if err := out.Truncate(int64(n) * ps); err != nil {
		return fail(err)
	}
	tags := make([]byte, n*crypto.TagSize)

	const chunk = 256
	type job struct{ first int }
	jobs := make(chan job)
	errc := make(chan error, 1)
	var wg sync.WaitGroup
	workers := runtime.GOMAXPROCS(0)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dec := d.cipher.Decryptor()
			in := make([]byte, chunk*ps)
			dst := make([]byte, chunk*ps)
			for j := range jobs {
				cnt := min(chunk, n-j.first)
				k, err := f.ReadAt(in[:cnt*ps], int64(j.first)*ps)
				if k < cnt*ps && !(err == io.EOF && wr.DBPages > 0) {
					select {
					case errc <- errUnstable:
					default:
					}
					continue
				}
				for i := 0; i < cnt; i++ {
					pgno := uint32(j.first + i + 1)
					enc := in[i*ps : (i+1)*ps]
					if off, ok := wr.Pages[pgno]; ok {
						enc = walBuf[off : off+ps]
					} else if (i+1)*ps > k {
						select {
						case errc <- errUnstable: // page beyond main, not in WAL
						default:
						}
						break
					}
					if err := dec.DecryptPage(dst[i*ps:(i+1)*ps], enc, pgno); err != nil {
						select {
						case errc <- fmt.Errorf("%s: %w", d.Name, err):
						default:
						}
						break
					}
					copy(tags[int(pgno-1)*crypto.TagSize:], crypto.Tag(enc))
				}
				if _, err := out.WriteAt(dst[:cnt*ps], int64(j.first)*ps); err != nil {
					select {
					case errc <- err:
					default:
					}
				}
			}
		}()
	}
	for p := 0; p < n; p += chunk {
		select {
		case err := <-errc:
			close(jobs)
			wg.Wait()
			return fail(err)
		case jobs <- job{p}:
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errc:
		return fail(err)
	default:
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", nil, err
	}
	return tmp, tags, nil
}

// buildPatched clones the published snapshot and writes only changed pages.
func (d *DB) buildPatched(changes map[uint32][]byte, n int) (string, []byte, error) {
	tmp := d.tmpPath()
	if err := unix.Clonefile(d.dst, tmp, unix.CLONE_NOFOLLOW); err != nil {
		if err := copyFile(d.dst, tmp); err != nil {
			return "", nil, err
		}
	}
	out, err := os.OpenFile(tmp, os.O_WRONLY, 0o600)
	if err != nil {
		os.Remove(tmp)
		return "", nil, err
	}
	fail := func(err error) (string, []byte, error) {
		out.Close()
		os.Remove(tmp)
		return "", nil, err
	}
	if err := out.Truncate(int64(n) * ps); err != nil {
		return fail(err)
	}
	tags := make([]byte, n*crypto.TagSize)
	copy(tags, d.tags)
	for pgno, page := range changes {
		if _, err := out.WriteAt(page, int64(pgno-1)*ps); err != nil {
			return fail(err)
		}
		copy(tags[int(pgno-1)*crypto.TagSize:], crypto.Tag(page))
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", nil, err
	}
	return tmp, tags, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// ReadOnlyDSN opens a snapshot immutably; the file is never modified in
// place, only replaced by rename.
func ReadOnlyDSN(path string) string {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
	return u.String()
}

func quickCheck(path string) error {
	db, err := sql.Open("sqlite", ReadOnlyDSN(path))
	if err != nil {
		return err
	}
	defer db.Close()
	var s string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&s); err != nil {
		return err
	}
	if s != "ok" {
		return errors.New("not ok")
	}
	return nil
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
