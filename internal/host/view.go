package host

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/justin06lee/hollow/api"
)

// The live view is a page this host serves to a person's browser: every
// desk, and any one of them as a live screen they can take over with their
// own mouse and keyboard.
//
// A browser has no bearer token, and a token in a page would be a token in
// a browser's history. So a client with the token asks for a link, which
// works once and not for long, and opening it trades it for a session
// cookie that only this host's /view/ pages are sent. The pages carry a
// strict CSP and refuse to be framed; anything that changes a desk also
// wants a header no form can send, and the stream wants the host's own
// origin, which together keep other sites from acting through the session.

//go:embed view
var viewFiles embed.FS

const (
	viewLinkTTL    = 15 * time.Minute
	viewSessionTTL = 12 * time.Hour
	viewCookie     = "hollow_view"
)

type views struct {
	mu       sync.Mutex
	links    map[string]time.Time // one-time link tokens
	sessions map[string]time.Time
}

func newViews() *views {
	return &views{links: map[string]time.Time{}, sessions: map[string]time.Time{}}
}

func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system has no randomness; nothing is safe to do
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (v *views) sweep(now time.Time) {
	for t, exp := range v.links {
		if now.After(exp) {
			delete(v.links, t)
		}
	}
	for t, exp := range v.sessions {
		if now.After(exp) {
			delete(v.sessions, t)
		}
	}
}

func (v *views) newLink() (string, time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	v.sweep(now)
	t, exp := randomToken(), now.Add(viewLinkTTL)
	v.links[t] = exp
	return t, exp
}

// redeem spends a link and starts a session for it.
func (v *views) redeem(link string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	v.sweep(now)
	for t := range v.links {
		if subtle.ConstantTimeCompare([]byte(t), []byte(link)) == 1 {
			delete(v.links, t)
			sess := randomToken()
			v.sessions[sess] = now.Add(viewSessionTTL)
			return sess, true
		}
	}
	return "", false
}

func (v *views) valid(r *http.Request) bool {
	c, err := r.Cookie(viewCookie)
	if err != nil || c.Value == "" {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for t, exp := range v.sessions {
		if subtle.ConstantTimeCompare([]byte(t), []byte(c.Value)) == 1 {
			return time.Now().Before(exp)
		}
	}
	return false
}

// handleViewLink mints a link to the live view, of one desk or of them all.
func (s *Server) handleViewLink(w http.ResponseWriter, r *http.Request) {
	var req api.ViewRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	path := "/view/"
	if req.Desk != "" {
		d, err := s.desks.Get(req.Desk)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		path = "/view/d/" + url.PathEscape(d.ID)
	}
	tok, exp := s.views.newLink()
	writeJSON(w, http.StatusOK, api.ViewLink{Path: path + "?t=" + tok, Expires: exp})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	var p api.Pause
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	d, err := s.desks.SetPaused(r.PathValue("id"), p.Paused)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// proxyStream joins a WebSocket to a desk's stream.
func (s *Server) proxyStream(w http.ResponseWriter, r *http.Request, ref string) {
	base, key, err := s.desks.Agent(ref)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	target, err := url.Parse(base)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	q := r.URL.Query()
	q.Del("t")
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path, pr.Out.URL.RawPath = "/stream", ""
			pr.Out.URL.RawQuery = q.Encode()
			pr.Out.Header.Set("Authorization", "Bearer "+key)
			pr.Out.Header.Del("Cookie")
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, http.StatusBadGateway, fmt.Errorf("desk %s: stream: %w", ref, err))
		},
	}
	release := s.desks.Watch(ref)
	defer release()
	rp.ServeHTTP(w, r)
}

// serveView is everything under /view/.
func (s *Server) serveView(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", fmt.Sprintf("default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob: data:; "+
		"connect-src 'self' ws://%[1]s wss://%[1]s; frame-ancestors 'none'; base-uri 'none'; form-action 'none'", r.Host))
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")

	if link := r.URL.Query().Get("t"); link != "" {
		sess, ok := s.views.redeem(link)
		if !ok {
			viewDenied(w, "This link has been used already, or it is too old.")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: viewCookie, Value: sess, Path: "/view/",
			MaxAge: int(viewSessionTTL / time.Second), HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
		// The link is spent; take it out of the address bar and history.
		q := r.URL.Query()
		q.Del("t")
		to := r.URL.Path
		if len(q) > 0 {
			to += "?" + q.Encode()
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/view/static/") {
		s.viewMux.ServeHTTP(w, r) // nothing secret: the pages' own code
		return
	}
	if !s.views.valid(r) {
		viewDenied(w, "This page needs a fresh link.")
		return
	}
	if r.Method != http.MethodGet && r.Header.Get("X-Hollow-View") != "1" {
		writeError(w, http.StatusForbidden, errors.New("missing X-Hollow-View"))
		return
	}
	s.viewMux.ServeHTTP(w, r)
}

func viewDenied(w http.ResponseWriter, why string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>hollow</title><link rel="stylesheet" href="/view/static/view.css">
<main class="denied"><h1>hollow</h1><p>%s</p><p>Ask for a new one:</p><pre>bangboo view</pre>
<p class="muted">or <code>hollow view</code> on the host. A link opens once, within fifteen minutes.</p></main>`, html.EscapeString(why))
}

func (s *Server) viewRoutes() *http.ServeMux {
	m := http.NewServeMux()
	static, _ := fs.Sub(viewFiles, "view")
	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			data, err := fs.ReadFile(static, name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)
		}
	}
	m.HandleFunc("GET /view/{$}", page("index.html"))
	m.HandleFunc("GET /view/d/{id}", page("desk.html"))
	m.Handle("GET /view/static/", http.StripPrefix("/view/static/", http.FileServerFS(static)))
	m.HandleFunc("GET /view/api/status", func(w http.ResponseWriter, r *http.Request) {
		total, avail := meminfo()
		writeJSON(w, http.StatusOK, map[string]any{
			"name": Hostname(), "version": s.version, "mem_mb": total, "free_mb": avail,
			"cpus": runtime.NumCPU(), "desks": len(s.desks.List()),
		})
	})
	m.HandleFunc("GET /view/api/desks", s.handleDesks)
	m.HandleFunc("GET /view/api/desks/{id}", s.handleDesk)
	m.HandleFunc("POST /view/api/desks/{id}/pause", s.handlePause)
	m.HandleFunc("GET /view/api/desks/{id}/screenshot", func(w http.ResponseWriter, r *http.Request) {
		s.proxy(w, r, r.PathValue("id"), "screenshot")
	})
	m.HandleFunc("GET /view/api/desks/{id}/clipboard", func(w http.ResponseWriter, r *http.Request) {
		s.proxy(w, r, r.PathValue("id"), "clipboard")
	})
	m.HandleFunc("GET /view/api/desks/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
		// The one route a cookie alone would open from any site, since a
		// WebSocket is not bound by the same-origin rules fetch is. It
		// has to come from this host's own pages.
		if o, err := url.Parse(r.Header.Get("Origin")); err != nil || o.Host != r.Host {
			writeError(w, http.StatusForbidden, errors.New("the stream is only for this host's own pages"))
			return
		}
		s.proxyStream(w, r, r.PathValue("id"))
	})
	return m
}
