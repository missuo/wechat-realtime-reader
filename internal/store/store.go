// Package store is the read-only query layer over published plaintext
// snapshots. Snapshots are opened with immutable=1 and reopened whenever the
// syncer atomically replaces them.
package store

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/missuo/wechat-realtime-reader/internal/snapshot"
)

// Store answers queries against the snapshot directory.
type Store struct {
	dir  string
	self string

	// Freshness reports per-db sync status; may be nil.
	Freshness func() map[string]snapshot.DBStatus

	mu       sync.Mutex
	handles  map[string]*handle
	contacts *contactSet
	dec      *zstd.Decoder
}

type handle struct {
	db     *sql.DB
	sig    sigT
	names  map[int64]string // Name2Id rowid → username (message dbs)
	ids    map[string]int64
	tables map[string]bool
}

type sigT struct {
	ino   uint64
	size  int64
	mtime int64
}

func fileSig(p string) (sigT, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return sigT{}, err
	}
	return sigT{st.Ino, st.Size, st.Mtimespec.Nano()}, nil
}

// New opens a store over dir (the "current" snapshot directory).
func New(dir, selfWxid string) (*Store, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(64<<20))
	if err != nil {
		return nil, err
	}
	return &Store{dir: dir, self: selfWxid, handles: map[string]*handle{}, dec: dec}, nil
}

// Self returns the account's own username.
func (s *Store) Self() string { return s.self }

// ErrNotReady means the snapshot has not been built yet.
var ErrNotReady = errors.New("snapshot not available yet; is the sync running?")

// get returns an up-to-date handle for a snapshot database.
func (s *Store) get(rel string) (*handle, error) {
	p := filepath.Join(s.dir, rel)
	sig, err := fileSig(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", rel, ErrNotReady)
		}
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.handles[rel]; h != nil && h.sig == sig {
		return h, nil
	}
	db, err := sql.Open("sqlite", snapshot.ReadOnlyDSN(p))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxIdleTime(time.Minute)
	h := &handle{db: db, sig: sig}
	if strings.HasPrefix(rel, "message/") {
		if err := h.loadMessageMeta(); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
	}
	if old := s.handles[rel]; old != nil {
		// In-flight queries may still use the old handle; close it later.
		time.AfterFunc(time.Minute, func() { old.db.Close() })
	}
	s.handles[rel] = h
	return h, nil
}

func (h *handle) loadMessageMeta() error {
	h.names, h.ids, h.tables = map[int64]string{}, map[string]int64{}, map[string]bool{}
	rows, err := h.db.Query(`SELECT rowid, user_name FROM Name2Id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var n string
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return err
		}
		h.names[id], h.ids[n] = n, id
	}
	rows.Close()
	rows, err = h.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Msg_%'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		h.tables[n] = true
	}
	return rows.Err()
}

// TableName is the per-chat message table: Msg_<md5(username)>.
func TableName(username string) string {
	sum := md5.Sum([]byte(username))
	return "Msg_" + hex.EncodeToString(sum[:])
}

var msgDBRe = regexp.MustCompile(`^(biz_)?message_(\d+)\.db$`)

// messageDBs lists message snapshot databases, newest shard first.
func (s *Store) messageDBs() []string {
	ents, _ := os.ReadDir(filepath.Join(s.dir, "message"))
	type item struct {
		rel string
		n   int
		biz bool
	}
	var items []item
	for _, e := range ents {
		m := msgDBRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		items = append(items, item{"message/" + e.Name(), n, m[1] != ""})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].biz != items[j].biz {
			return !items[i].biz
		}
		return items[i].n > items[j].n
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.rel
	}
	return out
}

// Freshness summary attached to query results.
type Freshness struct {
	Status           string    `json:"status"` // fresh | stale | error | unknown
	SourceCommitTime time.Time `json:"source_commit_time,omitzero"`
	SnapshotTime     time.Time `json:"snapshot_time,omitzero"`
	LagMS            int64     `json:"lag_ms"`
	StaleDBs         []string  `json:"stale_dbs,omitempty"`
}

// FreshnessFor summarizes the given databases (all message dbs when nil).
func (s *Store) FreshnessFor(dbs ...string) Freshness {
	f := Freshness{Status: "unknown"}
	if s.Freshness == nil {
		return f
	}
	all := s.Freshness()
	if len(all) == 0 {
		return f
	}
	if len(dbs) == 0 {
		for k := range all {
			dbs = append(dbs, k)
		}
	}
	f.Status = "fresh"
	for _, name := range dbs {
		st, ok := all[name]
		if !ok {
			continue
		}
		if st.SourceCommitTime.After(f.SourceCommitTime) {
			f.SourceCommitTime = st.SourceCommitTime
		}
		if st.SnapshotTime.After(f.SnapshotTime) {
			f.SnapshotTime = st.SnapshotTime
		}
		f.LagMS = max(f.LagMS, st.LagMS)
		if st.Status != "fresh" {
			f.StaleDBs = append(f.StaleDBs, name)
			if st.Status == "error" || f.Status == "fresh" {
				f.Status = st.Status
			}
		}
	}
	return f
}

func (s *Store) decompress(b []byte, ct sql.NullInt64) string {
	if len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd {
		out, err := s.dec.DecodeAll(b, nil)
		if err == nil {
			return string(out)
		}
	}
	return string(b)
}

// Close releases database handles.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.handles {
		h.db.Close()
	}
	s.handles = map[string]*handle{}
	s.dec.Close()
}

func ctxOr(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
