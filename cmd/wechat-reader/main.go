// Command wechat-reader is a read-only, near-real-time reader for the local
// macOS WeChat 4.x chat databases, with a web viewer and an MCP server.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/missuo/wechat-realtime-reader/internal/config"
	"github.com/missuo/wechat-realtime-reader/internal/crypto"
	"github.com/missuo/wechat-realtime-reader/internal/mcpserver"
	"github.com/missuo/wechat-realtime-reader/internal/media"
	"github.com/missuo/wechat-realtime-reader/internal/snapshot"
	"github.com/missuo/wechat-realtime-reader/internal/store"
	"github.com/missuo/wechat-realtime-reader/internal/web"
)

const usage = `wechat-reader — read-only near-real-time WeChat (macOS 4.x) reader

Usage:
  wechat-reader serve  [-addr 127.0.0.1:5678] [-v]   web viewer + HTTP MCP (/mcp) + live sync
  wechat-reader mcp    [-v]                          MCP over stdio (syncs too unless another process does)
  wechat-reader sync   [-watch] [-v]                 build/refresh snapshots once, or keep watching
  wechat-reader status                               show snapshot freshness
  wechat-reader doctor                               verify keys against every database (prints no secrets)
  wechat-reader token  [-addr 127.0.0.1:5678]        print the web/MCP access token and URL

Config: ~/.config/wechat-realtime-reader (override with WECHAT_READER_CONFIG)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "mcp":
		err = cmdMCP(args)
	case "sync":
		err = cmdSync(args)
	case "status":
		err = cmdStatus(args)
	case "doctor":
		err = cmdDoctor(args)
	case "token":
		err = cmdToken(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newLogger(verbose bool) *slog.Logger {
	lvl := slog.LevelInfo
	if verbose {
		lvl = slog.LevelDebug
	}
	// Logs go to stderr: stdout is reserved for MCP stdio.
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// runtime ties together config, store and (when this process owns the sync
// lock) a live syncer. When another process syncs, it reads that process's
// manifest and keeps trying to take over the lock if it goes away.
type runtime struct {
	cfg *config.Config
	log *slog.Logger
	st  *store.Store
	lib *media.Library

	mu     sync.Mutex
	syncer *snapshot.Syncer
	ready  chan struct{} // closed after the first full refresh (owner only)
}

func newRuntime(log *slog.Logger) (*runtime, error) {
	cfg, err := config.Load("")
	if err != nil {
		return nil, err
	}
	st, err := store.New(cfg.SnapshotDir(), cfg.SelfWxid())
	if err != nil {
		return nil, err
	}
	rt := &runtime{cfg: cfg, log: log, st: st, ready: make(chan struct{})}
	rt.lib = media.NewLibrary(cfg.AccountDir(), cfg.ImageKeyPath(), cfg.CacheDir(), cfg.SelfWxid())
	st.Freshness = rt.status
	return rt, nil
}

// warmImageKey derives the image key in the background so the first image
// request does not wait for it.
func (rt *runtime) warmImageKey() {
	go func() {
		t := time.Now()
		if _, err := rt.lib.EnsureKeys(); err != nil {
			rt.log.Warn("image key unavailable; images will not display", "err", err)
			return
		}
		if d := time.Since(t); d > time.Second {
			rt.log.Info("image key derived", "took", d.Round(time.Millisecond))
		}
	}()
}

func (rt *runtime) status() map[string]snapshot.DBStatus {
	rt.mu.Lock()
	s := rt.syncer
	rt.mu.Unlock()
	if s != nil {
		return s.Status().DBs
	}
	m, err := snapshot.ReadManifest(rt.cfg)
	if err != nil {
		return nil
	}
	// The manifest is only trustworthy while its writer is alive.
	if _, held, _ := lockHeld(rt.cfg); !held {
		for k, v := range m.DBs {
			v.Status = "stale"
			v.Error = "no sync process running"
			m.DBs[k] = v
		}
	}
	return m.DBs
}

func lockHeld(cfg *config.Config) (*snapshot.Lock, bool, error) {
	l, ok, err := snapshot.TryLock(cfg)
	if ok {
		l.Release()
		return nil, false, err
	}
	return nil, err == nil, err
}

func (rt *runtime) subscribe() (<-chan snapshot.Event, func()) {
	rt.mu.Lock()
	s := rt.syncer
	rt.mu.Unlock()
	if s == nil {
		return nil, func() {}
	}
	return s.Subscribe()
}

// runSync owns the sync loop when possible; returns when ctx ends.
func (rt *runtime) runSync(ctx context.Context) {
	announced := false
	for {
		lock, ok, err := snapshot.TryLock(rt.cfg)
		if err != nil {
			rt.log.Error("sync lock", "err", err)
		}
		if ok {
			rt.ownSync(ctx, lock)
			return
		}
		if !announced {
			rt.log.Info("another process is syncing; serving its snapshots read-only")
			announced = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (rt *runtime) ownSync(ctx context.Context, lock *snapshot.Lock) {
	defer lock.Release()
	s, err := snapshot.NewSyncer(rt.cfg, nil, rt.log)
	if err != nil {
		rt.log.Error("syncer", "err", err)
		return
	}
	rt.mu.Lock()
	rt.syncer = s
	rt.mu.Unlock()
	t := time.Now()
	if err := s.RefreshAll(ctx); err != nil {
		rt.log.Warn("initial refresh had errors", "err", err)
	}
	rt.log.Info("snapshots ready", "dbs", len(s.DBs()), "took", time.Since(t).Round(time.Millisecond))
	close(rt.ready)
	if err := s.Run(ctx); err != nil {
		rt.log.Error("sync loop", "err", err)
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:5678", "listen address (loopback only)")
	verbose := fs.Bool("v", false, "debug logging")
	noSync := fs.Bool("no-sync", false, "never sync; only read existing snapshots")
	fs.Parse(args)

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("refusing to listen on non-loopback address %s", *addr)
	}

	log := newLogger(*verbose)
	rt, err := newRuntime(log)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	if !*noSync {
		go rt.runSync(ctx)
	}
	rt.warmImageKey()
	token, err := web.LoadOrCreateToken(rt.cfg.StateDir())
	if err != nil {
		return err
	}
	srv := &web.Server{
		Store:  rt.st,
		Media:  rt.lib,
		MCP:    mcpserver.New(rt.st, rt.lib, rt.status),
		Token:  token,
		Log:    log,
		Status: rt.status,
	}
	if !*noSync {
		// Returns a nil channel while another process owns the sync; the
		// web layer then polls the manifest instead.
		srv.Events = rt.subscribe
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		hs.Shutdown(sctx)
	}()
	url := fmt.Sprintf("http://%s/?token=%s", ln.Addr(), token)
	fmt.Fprintf(os.Stderr, "\n  Web 查看:  %s\n  MCP (HTTP): http://%s/mcp   (Authorization: Bearer <token>)\n\n", url, ln.Addr())
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	verbose := fs.Bool("v", false, "debug logging")
	noSync := fs.Bool("no-sync", false, "never sync; only read existing snapshots")
	fs.Parse(args)
	log := newLogger(*verbose)
	rt, err := newRuntime(log)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	if !*noSync {
		go rt.runSync(ctx)
	}
	rt.warmImageKey()
	return mcpserver.New(rt.st, rt.lib, rt.status).Run(ctx, &mcp.StdioTransport{})
}

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	watch := fs.Bool("watch", false, "keep watching for changes")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)
	log := newLogger(*verbose)
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	lock, ok, err := snapshot.TryLock(cfg)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("another wechat-reader process is already syncing")
	}
	defer lock.Release()
	s, err := snapshot.NewSyncer(cfg, nil, log)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	t := time.Now()
	err = s.RefreshAll(ctx)
	printStatus(s.Status().DBs)
	fmt.Fprintf(os.Stderr, "refreshed in %s\n", time.Since(t).Round(time.Millisecond))
	if !*watch {
		return err
	}
	log.Info("watching for changes; Ctrl-C to stop")
	ch, _ := s.Subscribe()
	go func() {
		for ev := range ch {
			st := s.Status().DBs[ev.DB]
			log.Info("published", "db", ev.DB, "changed_pages", st.LastChangedPages, "took_ms", st.LastDurationMS, "lag_ms", st.LagMS)
		}
	}()
	return s.Run(ctx)
}

func printStatus(dbs map[string]snapshot.DBStatus) {
	names := make([]string, 0, len(dbs))
	for k := range dbs {
		names = append(names, k)
	}
	sort.Strings(names)
	fmt.Printf("%-28s %-7s %8s %8s %-19s %s\n", "DATABASE", "STATUS", "PAGES", "LAG_MS", "SNAPSHOT", "ERROR")
	for _, n := range names {
		d := dbs[n]
		snap := "-"
		if !d.SnapshotTime.IsZero() {
			snap = d.SnapshotTime.Format("2006-01-02 15:04:05")
		}
		fmt.Printf("%-28s %-7s %8d %8d %-19s %s\n", n, d.Status, d.Pages, d.LagMS, snap, d.Error)
	}
}

func cmdStatus(args []string) error {
	rt, err := newRuntime(newLogger(false))
	if err != nil {
		return err
	}
	dbs := rt.status()
	if dbs == nil {
		return errors.New("no manifest yet; run `wechat-reader sync` or `serve` first")
	}
	if len(args) > 0 && args[0] == "-json" {
		return json.NewEncoder(os.Stdout).Encode(dbs)
	}
	printStatus(dbs)
	return nil
}

func cmdDoctor(args []string) error {
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	fmt.Printf("config dir   %s (private: ok)\n", cfg.Dir)
	fmt.Printf("snapshot dir %s\n", cfg.SnapshotDir())
	fmt.Printf("keys         %d\n\n", len(cfg.Keys))
	names := make([]string, 0, len(cfg.Keys))
	for k := range cfg.Keys {
		names = append(names, k)
	}
	sort.Strings(names)
	bad := 0
	for _, n := range names {
		p := filepath.Join(cfg.Source.DBBasePath, n)
		page := make([]byte, crypto.PageSize)
		f, err := os.Open(p)
		if err != nil {
			fmt.Printf("  %-36s missing\n", n)
			continue
		}
		_, err = f.ReadAt(page, 0)
		f.Close()
		if err != nil {
			fmt.Printf("  %-36s unreadable\n", n)
			bad++
			continue
		}
		key, err := crypto.ParseHexKey(cfg.Keys[n])
		if err != nil {
			fmt.Printf("  %-36s bad key format\n", n)
			bad++
			continue
		}
		if _, mode, err := crypto.Open(key, page); err != nil {
			fmt.Printf("  %-36s FAIL (key/salt mismatch)\n", n)
			bad++
		} else {
			fmt.Printf("  %-36s ok (%s key)\n", n, mode)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d database(s) failed verification", bad)
	}
	return nil
}

func cmdToken(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:5678", "address used by serve")
	fs.Parse(args)
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	tok, err := web.LoadOrCreateToken(cfg.StateDir())
	if err != nil {
		return err
	}
	fmt.Println(tok)
	fmt.Fprintf(os.Stderr, "web: http://%s/?token=%s\n", *addr, tok)
	return nil
}
