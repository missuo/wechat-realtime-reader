package snapshot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/missuo/wechat-realtime-reader/internal/crypto"
)

// plainDB creates a synthetic plaintext SQLite db (4096/80) with sqlite3.
func plainDB(t *testing.T, dir, name, script string) []byte {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	p := filepath.Join(dir, name)
	cmd := exec.Command("sqlite3", p)
	cmd.Stdin = strings.NewReader(".filectrl reserve_bytes 80\nPRAGMA page_size=4096;\nVACUUM;\n" + script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite3: %v %s", err, out)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fixture struct {
	c    *crypto.Cipher
	key  []byte
	salt []byte
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{key: make([]byte, 32), salt: make([]byte, 16)}
	rand.Read(f.key)
	rand.Read(f.salt)
	var err error
	if f.c, err = crypto.NewCipher(f.key, f.salt, crypto.KeyDerived); err != nil {
		t.Fatal(err)
	}
	return f
}

// encPage encrypts deterministically (IV from content) so unchanged pages
// keep identical ciphertext, like an untouched page on disk.
func (f *fixture) encPage(plain []byte, pgno uint32) []byte {
	sum := sha256.Sum256(append(binary.LittleEndian.AppendUint32(nil, pgno), plain...))
	out := make([]byte, ps)
	f.c.EncryptPage(out, plain, pgno, f.salt, sum[:16])
	return out
}

func (f *fixture) encDB(plain []byte) []byte {
	var out []byte
	for i := 0; i*ps < len(plain); i++ {
		out = append(out, f.encPage(plain[i*ps:(i+1)*ps], uint32(i+1))...)
	}
	return out
}

// walOf builds a WAL with one committed transaction containing all pages
// of plain that differ from base.
func (f *fixture) walOf(base, plain []byte) []byte {
	h := make([]byte, 32)
	binary.BigEndian.PutUint32(h[0:], 0x377f0682)
	binary.BigEndian.PutUint32(h[4:], 3007000)
	binary.BigEndian.PutUint32(h[8:], ps)
	binary.BigEndian.PutUint32(h[16:], 7)
	binary.BigEndian.PutUint32(h[20:], 9)
	s1, s2 := walSum(h[:24], 0, 0)
	binary.BigEndian.PutUint32(h[24:], s1)
	binary.BigEndian.PutUint32(h[28:], s2)
	var changed []int
	n := len(plain) / ps
	for i := 0; i < n; i++ {
		if (i+1)*ps > len(base) || string(base[i*ps:(i+1)*ps]) != string(plain[i*ps:(i+1)*ps]) {
			changed = append(changed, i)
		}
	}
	buf := h
	for k, i := range changed {
		fh := make([]byte, 24)
		binary.BigEndian.PutUint32(fh[0:], uint32(i+1))
		if k == len(changed)-1 {
			binary.BigEndian.PutUint32(fh[4:], uint32(n))
		}
		binary.BigEndian.PutUint32(fh[8:], 7)
		binary.BigEndian.PutUint32(fh[12:], 9)
		page := f.encPage(plain[i*ps:(i+1)*ps], uint32(i+1))
		s1, s2 = walSum(fh[:8], s1, s2)
		s1, s2 = walSum(page, s1, s2)
		binary.BigEndian.PutUint32(fh[16:], s1)
		binary.BigEndian.PutUint32(fh[20:], s2)
		buf = append(append(buf, fh...), page...)
	}
	return buf
}

func walSum(b []byte, s1, s2 uint32) (uint32, uint32) {
	for i := 0; i+8 <= len(b); i += 8 {
		s1 += binary.LittleEndian.Uint32(b[i:]) + s2
		s2 += binary.LittleEndian.Uint32(b[i+4:]) + s1
	}
	return s1, s2
}

func count(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", ReadOnlyDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRefreshFullIncrementalWALAndTamper(t *testing.T) {
	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "src"), filepath.Join(tmp, "dst")
	os.MkdirAll(filepath.Join(src, "x"), 0o700)
	f := newFixture(t)

	rows := "CREATE TABLE t(x TEXT);\nWITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i<500) INSERT INTO t SELECT printf('row %05d %s', i, hex(randomblob(40))) FROM c;\n"
	v1 := plainDB(t, tmp, "v1.db", rows)
	main := filepath.Join(src, "x", "a.db")
	os.WriteFile(main, f.encDB(v1), 0o600)

	d := NewDB("x/a.db", src, dst, f.key)
	ctx := context.Background()

	// 1. full build
	res, err := d.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Full || !res.Published {
		t.Fatalf("expected full publish, got %+v", res)
	}
	if n := count(t, d.SnapshotPath()); n != 500 {
		t.Fatalf("rows=%d", n)
	}

	// 2. no change → nothing published
	if res, err = d.Refresh(ctx); err != nil || res.Published {
		t.Fatalf("expected no-op, got %+v %v", res, err)
	}

	// 3. WAL with one more row → incremental publish of few pages
	v2path := filepath.Join(tmp, "v2.db")
	os.WriteFile(v2path, v1, 0o600)
	cmd := exec.Command("sqlite3", v2path, "INSERT INTO t VALUES('new row');")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	v2, _ := os.ReadFile(v2path)
	os.WriteFile(main+"-wal", f.walOf(v1, v2), 0o600)
	res, err = d.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Full || !res.Published || res.ChangedPages == 0 || res.ChangedPages > 5 {
		t.Fatalf("expected small incremental publish, got %+v", res)
	}
	if n := count(t, d.SnapshotPath()); n != 501 {
		t.Fatalf("after WAL rows=%d", n)
	}

	// 4. checkpoint: main now holds v2, WAL emptied → content unchanged
	os.WriteFile(main, f.encDB(v2), 0o600)
	os.Truncate(main+"-wal", 0)
	if res, err = d.Refresh(ctx); err != nil || res.Published {
		t.Fatalf("checkpoint should not change snapshot: %+v %v", res, err)
	}

	// 5. tampered page → refresh fails, previous snapshot untouched
	before, _ := os.ReadFile(d.SnapshotPath())
	enc, _ := os.ReadFile(main)
	enc[3*ps+100] ^= 0xff
	os.WriteFile(main, enc, 0o600)
	if _, err = d.Refresh(ctx); err == nil {
		t.Fatal("tampered page must fail the refresh")
	}
	after, _ := os.ReadFile(d.SnapshotPath())
	if string(before) != string(after) {
		t.Fatal("failed refresh modified the published snapshot")
	}
	if left, _ := filepath.Glob(d.SnapshotPath() + ".tmp-*"); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}

	// 6. restart: a new DB object reuses the published snapshot's tags
	os.WriteFile(main, f.encDB(v2), 0o600)
	d2 := NewDB("x/a.db", src, dst, f.key)
	if res, err = d2.Refresh(ctx); err != nil || res.Published {
		t.Fatalf("restart should reuse snapshot: %+v %v", res, err)
	}
}
