// Package web serves the local viewer UI, a JSON API, server-sent events
// and the MCP streamable-HTTP endpoint. It binds to loopback only and
// requires a per-install token.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/missuo/wechat-realtime-reader/internal/media"
	"github.com/missuo/wechat-realtime-reader/internal/snapshot"
	"github.com/missuo/wechat-realtime-reader/internal/store"
)

//go:embed static
var staticFS embed.FS

const cookieName = "wrr_token"

// Server is the HTTP front end.
type Server struct {
	Store  *store.Store
	Media  *media.Library
	MCP    *mcp.Server
	Token  string
	Log    *slog.Logger
	Status func() map[string]snapshot.DBStatus
	// Events delivers publish notifications. A nil func or nil channel
	// means fall back to polling Status.
	Events func() (<-chan snapshot.Event, func())
}

// LoadOrCreateToken returns the persistent access token (0600 file).
func LoadOrCreateToken(stateDir string) (string, error) {
	p := filepath.Join(stateDir, "web-token")
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	return tok, os.WriteFile(p, []byte(tok+"\n"), 0o600)
}

// Handler builds the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(sub)
	mux.Handle("GET /{path...}", files)
	mux.HandleFunc("GET /api/sessions", s.sessions)
	mux.HandleFunc("GET /api/contacts", s.contacts)
	mux.HandleFunc("GET /api/messages", s.messages)
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("GET /api/members", s.members)
	mux.HandleFunc("GET /api/avatar/{user}", s.avatar)
	mux.HandleFunc("GET /api/image", s.image)
	mux.HandleFunc("GET /api/video", s.video)
	mux.HandleFunc("GET /api/voice", s.voice)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/events", s.events)
	if s.MCP != nil {
		h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.MCP }, nil)
		for _, m := range []string{"GET", "POST", "DELETE"} {
			mux.Handle(m+" /mcp", h)
		}
	}
	return s.guard(mux)
}

// guard enforces loopback Host (DNS-rebinding defence) and the token.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'")

		// ?token=… on a page load sets the cookie and strips the token.
		if t := r.URL.Query().Get("token"); t != "" && r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
			if s.tokenOK(t) {
				http.SetCookie(w, &http.Cookie{Name: cookieName, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400 * 365})
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
		}
		if !s.authorized(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp" {
				http.Error(w, "unauthorized: open the URL printed by `wechat-reader serve`", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>WeChat Reader</title><body style="font:15px system-ui;padding:40px;color:#444">需要访问令牌。请打开 <code>wechat-reader serve</code> 启动时输出的链接（含 <code>?token=</code>）。<br>令牌也可以用 <code>wechat-reader token</code> 查看。</body>`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1
}

func (s *Server) authorized(r *http.Request) bool {
	if c, err := r.Cookie(cookieName); err == nil && s.tokenOK(c.Value) {
		return true
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") && s.tokenOK(strings.TrimPrefix(a, "Bearer ")) {
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var nf *store.NotFoundError
	switch {
	case errors.As(err, &nf):
		code = http.StatusNotFound
	case errors.Is(err, store.ErrNotReady):
		code = http.StatusServiceUnavailable
	case errors.Is(err, errBadRequest):
		code = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

var errBadRequest = errors.New("bad request")

func intParam(r *http.Request, k string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(k)); err == nil {
		return v
	}
	return def
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ss, err := s.Store.Sessions(r.Context(), store.SessionQuery{Keyword: q.Get("keyword"), Kind: q.Get("kind"), Limit: intParam(r, "limit", 300)})
	if err != nil {
		writeErr(w, err)
		return
	}
	if ss == nil {
		ss = []store.Session{}
	}
	writeJSON(w, map[string]any{"sessions": ss, "freshness": s.Store.FreshnessFor("session/session.db")})
}

func (s *Server) contacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cs, err := s.Store.SearchContacts(r.Context(), store.ContactQuery{Keyword: q.Get("keyword"), Kind: q.Get("kind"), Limit: intParam(r, "limit", 30)})
	if err != nil {
		writeErr(w, err)
		return
	}
	if cs == nil {
		cs = []*store.Contact{}
	}
	writeJSON(w, map[string]any{"contacts": cs})
}

func (s *Server) parseQuery(r *http.Request) (store.MessageQuery, error) {
	q := r.URL.Query()
	mq := store.MessageQuery{
		Chat:    q.Get("chat"),
		Sender:  q.Get("sender"),
		Keyword: q.Get("keyword"),
		Limit:   intParam(r, "limit", 60),
		WithRaw: q.Get("raw") == "1",
	}
	var err error
	if mq.Since, err = store.ParseTime(q.Get("since")); err != nil {
		return mq, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	if mq.Until, err = store.ParseUntil(q.Get("until")); err != nil {
		return mq, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	mq.BeforeSeq, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	mq.AfterSeq, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	for _, t := range strings.Split(q.Get("types"), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			mq.Types = append(mq.Types, n)
		}
	}
	return mq, nil
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	mq, err := s.parseQuery(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if mq.Chat == "" {
		writeErr(w, fmt.Errorf("%w: chat is required", errBadRequest))
		return
	}
	mq.Asc = true
	page, err := s.Store.Messages(r.Context(), mq)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, page)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	mq, err := s.parseQuery(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(mq.Keyword) == "" && mq.Sender == "" {
		writeErr(w, fmt.Errorf("%w: keyword or sender is required", errBadRequest))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	page, err := s.Store.SearchMessages(ctx, mq)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, page)
}

func (s *Server) members(w http.ResponseWriter, r *http.Request) {
	chat := r.URL.Query().Get("chat")
	ms, err := s.Store.ChatMembers(r.Context(), chat)
	if err != nil {
		writeErr(w, err)
		return
	}
	if ms == nil {
		ms = []store.ChatMember{}
	}
	writeJSON(w, map[string]any{"members": ms})
}

func (s *Server) avatar(w http.ResponseWriter, r *http.Request) {
	b, err := s.Store.Avatar(r.Context(), r.PathValue("user"))
	if err != nil || len(b) == 0 {
		w.Header().Set("Cache-Control", "private, max-age=600")
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(b))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(b)
}

// image serves a decoded image attachment: ?chat=&id=&local_id=&t=&size=thumb|full
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	if s.Media == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	ts, _ := strconv.ParseInt(q.Get("t"), 10, 64)
	ids := []string{q.Get("id")}
	if lid := q.Get("local_id"); lid != "" {
		ids = append(ids, lid+"_"+q.Get("t"))
	}
	size := media.Thumb
	if q.Get("size") == "full" {
		size = media.Full
	}
	b, mime, thumbOnly, err := s.Media.Image(r.Context(), q.Get("chat"), ids, time.Unix(ts, 0), size)
	if err != nil {
		w.Header().Set("Cache-Control", "private, max-age=300")
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", mime)
	if thumbOnly {
		// The original may be downloaded later; do not cache the fallback long.
		w.Header().Set("X-Image-Variant", "thumb-only")
		w.Header().Set("Cache-Control", "private, max-age=300")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=86400")
	}
	w.Write(b)
}

// video serves a video attachment: ?id=&t=[&cover=1]. The mp4 is served
// with Range support so the browser can seek.
func (s *Server) video(w http.ResponseWriter, r *http.Request) {
	if s.Media == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	ts, _ := strconv.ParseInt(q.Get("t"), 10, 64)
	t := time.Unix(ts, 0)
	if q.Get("cover") != "" {
		b, mime, err := s.Media.VideoCover(q.Get("id"), t)
		if err != nil {
			w.Header().Set("Cache-Control", "private, max-age=300")
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Write(b)
		return
	}
	p, err := s.Media.VideoPath(q.Get("id"), t)
	if err != nil {
		w.Header().Set("Cache-Control", "private, max-age=300")
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// voice serves a voice message decoded to WAV: ?chat=&local_id=&t=
func (s *Server) voice(w http.ResponseWriter, r *http.Request) {
	if s.Media == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	ts, _ := strconv.ParseInt(q.Get("t"), 10, 64)
	lid, _ := strconv.ParseInt(q.Get("local_id"), 10, 64)
	silk, err := s.Store.VoiceSILK(r.Context(), q.Get("chat"), lid, time.Unix(ts, 0))
	if err != nil {
		w.Header().Set("Cache-Control", "private, max-age=60")
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	p, _, err := s.Media.VoiceWAV(silk)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	dbs := map[string]snapshot.DBStatus{}
	if s.Status != nil {
		dbs = s.Status()
	}
	writeJSON(w, map[string]any{"dbs": dbs, "freshness": s.Store.FreshnessFor(), "self": s.Store.Self()})
}

// events streams {"db":…, "version":…} whenever a snapshot is republished.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	fmt.Fprint(w, "retry: 2000\n\n")
	fl.Flush()

	send := func(ev snapshot.Event) {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: update\ndata: %s\n\n", b)
		fl.Flush()
	}
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	var ch <-chan snapshot.Event
	if s.Events != nil {
		var cancel func()
		ch, cancel = s.Events()
		defer cancel()
	}
	if ch != nil {
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				send(ev)
			case <-keepalive.C:
				fmt.Fprint(w, ": ping\n\n")
				fl.Flush()
			}
		}
	}

	// No in-process syncer: poll the manifest written by the sync owner.
	last := map[string]uint64{}
	if s.Status != nil {
		for k, v := range s.Status() {
			last[k] = v.Version
		}
	}
	poll := time.NewTicker(700 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
			if s.Status == nil {
				continue
			}
			for k, v := range s.Status() {
				if v.Version != last[k] {
					last[k] = v.Version
					send(snapshot.Event{DB: k, Version: v.Version, Time: v.SnapshotTime})
				}
			}
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}
