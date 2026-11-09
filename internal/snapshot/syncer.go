package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"

	"github.com/missuo/wechat-realtime-reader/internal/config"
	"github.com/missuo/wechat-realtime-reader/internal/crypto"
)

// DefaultDBs are the databases needed for chat reading. head_image is
// optional and used for avatars only; media holds voice audio.
var DefaultDBs = []string{
	"contact/contact.db",
	"session/session.db",
	"message/message_[0-9]*.db",
	"message/biz_message_[0-9]*.db",
	"message/media_[0-9]*.db",
	"head_image/head_image.db",
}

// DBStatus is the freshness record for one database. It never contains
// keys, salts, wxids or message content.
type DBStatus struct {
	Name             string    `json:"name"`
	Status           string    `json:"status"` // fresh | stale | error | pending
	SourceCommitTime time.Time `json:"source_commit_time"`
	SnapshotTime     time.Time `json:"snapshot_time"`
	LagMS            int64     `json:"lag_ms"`
	Pages            int       `json:"pages"`
	LastChangedPages int       `json:"last_changed_pages"`
	LastDurationMS   int64     `json:"last_duration_ms"`
	Version          uint64    `json:"version"`
	Error            string    `json:"error,omitempty"`
}

// Manifest is persisted to state/manifest.json so read-only processes can
// report freshness.
type Manifest struct {
	UpdatedAt time.Time           `json:"updated_at"`
	SyncPID   int                 `json:"sync_pid"`
	DBs       map[string]DBStatus `json:"dbs"`
}

// Event is broadcast after a snapshot is republished.
type Event struct {
	DB      string    `json:"db"`
	Version uint64    `json:"version"`
	Time    time.Time `json:"time"`
}

// Syncer keeps a set of databases fresh.
type Syncer struct {
	cfg   *config.Config
	dbs   []*DB
	byAbs map[string]*DB
	log   *slog.Logger

	mu       sync.Mutex
	status   map[string]DBStatus
	subs     map[chan Event]struct{}
	pending  map[*DB]bool
	running  map[*DB]bool
	debounce time.Duration
	started  time.Time
}

// NewSyncer resolves the database list (glob patterns relative to
// db_storage) and pairs each with its key. Databases without a key are
// skipped with a warning.
func NewSyncer(cfg *config.Config, patterns []string, log *slog.Logger) (*Syncer, error) {
	if len(patterns) == 0 {
		patterns = DefaultDBs
	}
	s := &Syncer{
		cfg:      cfg,
		byAbs:    map[string]*DB{},
		log:      log,
		status:   map[string]DBStatus{},
		subs:     map[chan Event]struct{}{},
		pending:  map[*DB]bool{},
		running:  map[*DB]bool{},
		debounce: 300 * time.Millisecond,
		started:  time.Now(),
	}
	seen := map[string]bool{}
	for _, pat := range patterns {
		matches, _ := filepath.Glob(filepath.Join(cfg.Source.DBBasePath, pat))
		sort.Strings(matches)
		for _, m := range matches {
			rel, _ := filepath.Rel(cfg.Source.DBBasePath, m)
			if seen[rel] || strings.Contains(rel, "_fts") {
				continue
			}
			seen[rel] = true
			hexKey, ok := cfg.Keys[rel]
			if !ok {
				log.Warn("no key for database, skipping", "db", rel)
				continue
			}
			key, err := crypto.ParseHexKey(hexKey)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			d := NewDB(rel, cfg.Source.DBBasePath, cfg.SnapshotDir(), key)
			s.dbs = append(s.dbs, d)
			for _, p := range d.SourcePaths() {
				s.byAbs[p] = d
			}
			s.status[rel] = DBStatus{Name: rel, Status: "pending"}
		}
	}
	if len(s.dbs) == 0 {
		return nil, errors.New("no databases matched with keys")
	}
	// Remove temp files left by a crashed run.
	if old, _ := filepath.Glob(filepath.Join(cfg.SnapshotDir(), "*", "*.tmp-*")); len(old) > 0 {
		for _, f := range old {
			os.Remove(f)
		}
	}
	if err := os.MkdirAll(cfg.SnapshotDir(), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir(), 0o700); err != nil {
		return nil, err
	}
	return s, nil
}

// DBs returns the managed databases.
func (s *Syncer) DBs() []*DB { return s.dbs }

// Subscribe returns a channel receiving publish events.
func (s *Syncer) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// Status returns a copy of the freshness table.
func (s *Syncer) Status() Manifest {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := Manifest{UpdatedAt: time.Now(), SyncPID: os.Getpid(), DBs: map[string]DBStatus{}}
	for k, v := range s.status {
		m.DBs[k] = v
	}
	return m
}

// RefreshAll refreshes every database, in parallel across databases.
func (s *Syncer) RefreshAll(ctx context.Context) error {
	var wg sync.WaitGroup
	errs := make([]error, len(s.dbs))
	sem := make(chan struct{}, 3) // bound memory/IO for first builds
	for i, d := range s.dbs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			errs[i] = s.refresh(ctx, d)
		}()
	}
	wg.Wait()
	s.writeManifest()
	return errors.Join(errs...)
}

func (s *Syncer) refresh(ctx context.Context, d *DB) error {
	res, err := d.Refresh(ctx)
	s.mu.Lock()
	st := s.status[d.Name]
	if err != nil {
		st.Status = "error"
		if st.Version > 0 {
			st.Status = "stale"
		}
		st.Error = err.Error()
		s.status[d.Name] = st
		s.mu.Unlock()
		s.log.Error("refresh failed", "db", d.Name, "err", err)
		return err
	}
	now := time.Now()
	st.Status, st.Error = "fresh", ""
	st.SourceCommitTime = res.SourceTime
	st.Pages = res.Pages
	if res.Published || st.Version == 0 {
		st.Version++
		st.SnapshotTime = now
		st.LastChangedPages = res.ChangedPages
		st.LastDurationMS = res.Duration.Milliseconds()
	}
	// Lag = time from the source commit (or from when we started watching,
	// for commits that predate this process) to the snapshot covering it.
	st.LagMS = max(0, st.SnapshotTime.Sub(latest(st.SourceCommitTime, s.started)).Milliseconds())
	s.status[d.Name] = st
	var subs []chan Event
	if res.Published {
		for ch := range s.subs {
			subs = append(subs, ch)
		}
	}
	s.mu.Unlock()
	if res.Published {
		s.log.Debug("published", "db", d.Name, "changed", res.ChangedPages, "full", res.Full, "took", res.Duration)
		ev := Event{DB: d.Name, Version: st.Version, Time: now}
		for _, ch := range subs {
			select {
			case ch <- ev:
			default:
			}
		}
	}
	return nil
}

// Run watches the sources and refreshes on change until ctx ends. It uses
// kqueue (via fsnotify) with a slow stat poll as a safety net.
func (s *Syncer) Run(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	dirs := map[string]bool{}
	for _, d := range s.dbs {
		dirs[filepath.Dir(d.src)] = true
	}
	for dir := range dirs {
		if err := w.Add(dir); err != nil {
			s.log.Warn("watch failed, relying on polling", "dir", filepath.Base(dir), "err", err)
		}
	}

	poll := time.NewTicker(3 * time.Second)
	defer poll.Stop()
	manifestTick := time.NewTicker(5 * time.Second)
	defer manifestTick.Stop()

	for {
		select {
		case <-ctx.Done():
			s.writeManifest()
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return errors.New("watcher closed")
			}
			if d := s.byAbs[ev.Name]; d != nil {
				s.schedule(ctx, d)
			}
		case err, ok := <-w.Errors:
			if ok {
				s.log.Warn("watch error", "err", err)
			}
		case <-poll.C:
			for _, d := range s.dbs {
				if d.Changed() {
					s.schedule(ctx, d)
				}
			}
		case <-manifestTick.C:
			s.markStale()
			s.writeManifest()
		}
	}
}

// schedule debounces refreshes: at most one running per DB, and events
// during a run collapse into a single follow-up refresh.
func (s *Syncer) schedule(ctx context.Context, d *DB) {
	s.mu.Lock()
	if s.running[d] {
		s.pending[d] = true
		s.mu.Unlock()
		return
	}
	s.running[d] = true
	s.mu.Unlock()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.debounce):
			}
			_ = s.refresh(ctx, d)
			s.writeManifest()
			s.mu.Lock()
			if !s.pending[d] {
				s.running[d] = false
				s.mu.Unlock()
				return
			}
			s.pending[d] = false
			s.mu.Unlock()
		}
	}()
}

// markStale flags snapshots whose source moved on without a refresh.
func (s *Syncer) markStale() {
	for _, d := range s.dbs {
		if d.Changed() {
			s.mu.Lock()
			st := s.status[d.Name]
			if st.Status == "fresh" && !s.running[d] {
				st.Status = "stale"
				s.status[d.Name] = st
			}
			s.mu.Unlock()
		}
	}
}

func (s *Syncer) writeManifest() {
	m := s.Status()
	b, _ := json.MarshalIndent(m, "", "  ")
	p := filepath.Join(s.cfg.StateDir(), "manifest.json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		os.Rename(tmp, p)
	}
}

// ReadManifest loads the manifest written by whichever process syncs.
func ReadManifest(cfg *config.Config) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(cfg.StateDir(), "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Lock is an exclusive advisory lock making sure only one process writes
// snapshots.
type Lock struct{ f *os.File }

// TryLock acquires the sync lock without blocking. ok=false means another
// process is already syncing.
func TryLock(cfg *config.Config) (*Lock, bool, error) {
	if err := os.MkdirAll(cfg.StateDir(), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(cfg.StateDir(), "sync.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &Lock{f: f}, true, nil
}

func (l *Lock) Release() {
	if l != nil && l.f != nil {
		unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
		l.f.Close()
	}
}
