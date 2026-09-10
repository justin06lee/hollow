package host

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"runtime"
	"strings"

	"github.com/justin06lee/hollow/api"
	"github.com/justin06lee/hollow/internal/backend"
	"github.com/justin06lee/hollow/internal/image"
)

// agents holds the guest agent binaries, one per guest platform, built by
// the Makefile before the host is. A guest fetches its own at every boot.
//
//go:embed agents
var agents embed.FS

// AgentBinary returns the embedded agent for a guest platform, or an error
// saying the host was built without it.
func AgentBinary(goos, goarch string) ([]byte, error) {
	data, err := fs.ReadFile(agents, "agents/hollow-agent-"+goos+"-"+goarch)
	if err != nil {
		return nil, fmt.Errorf("this hollow was built without the %s/%s guest agent; build it with make", goos, goarch)
	}
	return data, nil
}

// Server is the API.
type Server struct {
	version string
	token   string
	backend backend.Backend
	images  *image.Manager
	desks   *Desks
	agent   *http.Client
	mux     *http.ServeMux
}

// NewServer wires the API to its parts.
func NewServer(version, token string, b backend.Backend, images *image.Manager, desks *Desks) *Server {
	s := &Server{
		version: version,
		token:   token,
		backend: b,
		images:  images,
		desks:   desks,
		// No timeout: an exec may legitimately run for minutes and a
		// recording stop returns a whole file. The caller's context bounds it.
		agent: &http.Client{},
		mux:   http.NewServeMux(),
	}
	m := s.mux
	m.HandleFunc("GET /v1/status", s.handleStatus)
	m.HandleFunc("GET /v1/images", s.handleImages)
	m.HandleFunc("GET /v1/images/{os}", s.handleImage)
	m.HandleFunc("POST /v1/images/{os}/pull", s.handlePull)
	m.HandleFunc("GET /v1/desks", s.handleDesks)
	m.HandleFunc("POST /v1/desks", s.handleCreate)
	m.HandleFunc("GET /v1/desks/{id}", s.handleDesk)
	m.HandleFunc("DELETE /v1/desks/{id}", s.handleDelete)
	m.HandleFunc("GET /v1/desks/{id}/logs", s.handleLogs)
	m.HandleFunc("/v1/desks/{id}/{rest...}", s.handleAgent)
	m.HandleFunc("GET /v1/agent/hollow-agent", s.handleAgentBinary)
	return s
}

// ServeHTTP checks the token on everything except the agent download, which
// a guest fetches before it knows anything, and which is only reachable from
// this machine and its guests anyway.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/agent/") {
		s.mux.ServeHTTP(w, r)
		return
	}
	if !s.authed(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hollow"`)
		writeError(w, http.StatusUnauthorized, errors.New("missing or wrong token"))
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) authed(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.Status{
		Version: s.version,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Backend: s.backend.Name(),
		Desks:   len(s.desks.List()),
		Images:  s.images.List(),
	})
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.images.List())
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	img, err := s.images.Get(r.PathValue("os"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, img)
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	osName := r.PathValue("os")
	if err := s.images.Pull(osName); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	img, _ := s.images.Get(osName)
	writeJSON(w, http.StatusAccepted, img)
}

func (s *Server) handleDesks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.desks.List())
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var spec api.DeskSpec
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	d, err := s.desks.Create(spec)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) handleDesk(w http.ResponseWriter, r *http.Request) {
	d, err := s.desks.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.desks.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	out, err := s.desks.ConsoleLog(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, out)
}

// handleAgent forwards everything under a desk to the agent inside it:
// screenshot, input, exec, files, record/start, record/stop, health.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	base, err := s.desks.AgentURL(id)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	target := base + "/" + r.PathValue("rest")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.ContentLength = r.ContentLength
	for _, h := range []string{"Content-Type", "Accept"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := s.agent.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		writeError(w, http.StatusBadGateway, fmt.Errorf("desk %s: agent: %w", id, err))
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) handleAgentBinary(w http.ResponseWriter, r *http.Request) {
	data, err := AgentBinary("linux", "amd64")
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, api.Error{Error: err.Error()})
}
