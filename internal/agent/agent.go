// Package agent is the program that runs inside a desk.
//
// It is small on purpose: it owns nothing and remembers nothing. It grabs the
// screen, moves the pointer, presses keys, runs programs, moves files, and
// records video — over plain HTTP on a port only the host can reach. The host
// forwards a client's request to it nearly verbatim, so the shapes here are
// the ones in package api.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/justin06lee/hollow/api"
)

// Server answers the host.
type Server struct {
	disp    *display
	home    string
	rec     recorder
	started time.Time
	version string
}

// New makes a server for the named display, such as ":0".
func New(displayName, version string) *Server {
	home, _ := os.UserHomeDir()
	return &Server{
		disp:    &display{name: displayName},
		home:    home,
		started: time.Now(),
		version: version,
	}
}

// Handler is the agent's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /screenshot", s.handleScreenshot)
	mux.HandleFunc("POST /input", s.handleInput)
	mux.HandleFunc("POST /exec", s.handleExec)
	mux.HandleFunc("PUT /files", s.handlePutFile)
	mux.HandleFunc("GET /files", s.handleGetFile)
	mux.HandleFunc("POST /record/start", s.handleRecordStart)
	mux.HandleFunc("POST /record/stop", s.handleRecordStop)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	width, height, err := s.disp.Size()
	if err != nil {
		// Not an error in the agent — the display is not up yet. 503 tells
		// the host to keep waiting rather than give up.
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("display %s: %w", s.disp.name, err))
		return
	}
	writeJSON(w, http.StatusOK, api.Health{
		Display: s.disp.name,
		Width:   width,
		Height:  height,
		Uptime:  time.Since(s.started).Seconds(),
		Agent:   s.version,
	})
}

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	img, err := s.disp.Capture()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	switch r.URL.Query().Get("format") {
	case "jpeg", "jpg":
		q := 80
		if v, err := strconv.Atoi(r.URL.Query().Get("quality")); err == nil && v > 0 && v <= 100 {
			q = v
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_ = jpeg.Encode(w, img, &jpeg.Options{Quality: q})
	default:
		w.Header().Set("Content-Type", "image/png")
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		_ = enc.Encode(w, img)
	}
}

func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	var in api.Input
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.input(in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req api.Exec
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.exec(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// resolve turns a client's path into one on this machine: absolute paths as
// they are, anything else under the desk user's home.
func (s *Server) resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.home, p)
	}
	return filepath.Clean(p), nil
}

func (s *Server) handlePutFile(w http.ResponseWriter, r *http.Request) {
	p, err := s.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	mode := os.FileMode(0o644)
	if m, err := strconv.ParseUint(r.URL.Query().Get("mode"), 8, 32); err == nil {
		mode = os.FileMode(m)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	n, err := io.Copy(f, r.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "bytes": n})
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	p, err := s.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			code = http.StatusNotFound
		}
		writeError(w, code, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if st.IsDir() {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s is a directory", p))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	_, _ = io.Copy(w, f)
}

func (s *Server) handleRecordStart(w http.ResponseWriter, r *http.Request) {
	var req api.Record
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	width, height, err := s.disp.Size()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	if err := s.rec.start(s.disp.name, width, height, req.FPS); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"recording": true})
}

func (s *Server) handleRecordStop(w http.ResponseWriter, r *http.Request) {
	path, err := s.rec.stop()
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	defer os.Remove(path)
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "video/mp4")
	if st != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	_, _ = io.Copy(w, f)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, api.Error{Error: err.Error()})
}
