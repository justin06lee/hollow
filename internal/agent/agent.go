// Package agent is the program that runs inside a desk.
//
// It is small on purpose: it owns nothing and remembers nothing. It grabs the
// screen, moves the pointer, presses keys, runs programs, moves files, reads
// and drives the browser, and records video — over plain HTTP on a port only
// the host is meant to reach. The host forwards a client's request to it
// nearly verbatim, so the shapes here are the ones in package api.
//
// Every request carries the desk's key. The port is forwarded to the host's
// loopback, and a host on a mesh may publish its loopback ports to every
// other machine on it; the key is what keeps "only the host" true anyway.
package agent

import (
	"crypto/subtle"
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
	key     string
	rec     recorder
	browser *browser
	started time.Time
	version string
}

// New makes a server for the named display, such as ":0", that answers only
// requests carrying key.
func New(displayName, version, key string) *Server {
	home, _ := os.UserHomeDir()
	s := &Server{
		disp:    &display{name: displayName},
		home:    home,
		key:     key,
		started: time.Now(),
		version: version,
	}
	s.browser = &browser{s: s}
	return s
}

// Handler is the agent's routes, behind the key.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /screenshot", s.handleScreenshot)
	mux.HandleFunc("GET /cursor", s.handleCursor)
	mux.HandleFunc("POST /input", s.handleInput)
	mux.HandleFunc("POST /exec", s.handleExec)
	mux.HandleFunc("PUT /files", s.handlePutFile)
	mux.HandleFunc("GET /files", s.handleGetFile)
	mux.HandleFunc("POST /record/start", s.handleRecordStart)
	mux.HandleFunc("POST /record/stop", s.handleRecordStop)
	mux.HandleFunc("GET /windows", s.handleWindows)
	mux.HandleFunc("POST /windows", s.handleWindowAction)
	mux.HandleFunc("GET /clipboard", s.handleClipboardGet)
	mux.HandleFunc("PUT /clipboard", s.handleClipboardSet)
	mux.HandleFunc("POST /browser/open", s.handleBrowserOpen)
	mux.HandleFunc("POST /browser/read", s.handleBrowserRead)
	mux.HandleFunc("POST /browser/click", s.handleBrowserClick)
	mux.HandleFunc("POST /browser/type", s.handleBrowserType)
	mux.HandleFunc("POST /browser/eval", s.handleBrowserEval)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.key == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.key)) != 1 {
			writeError(w, http.StatusUnauthorized, errors.New("wrong or missing desk key"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	width, height, err := s.disp.Size()
	if err != nil {
		// Not an error in the agent — the display is not up yet. 503 tells
		// the host to keep waiting rather than give up.
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("display %s: %w", s.disp.name, err))
		return
	}
	features := []string{"browser", "windows"}
	if _, err := lookPath("xclip"); err == nil {
		features = append(features, "clipboard")
	}
	writeJSON(w, http.StatusOK, api.Health{
		Display:  s.disp.name,
		Width:    width,
		Height:   height,
		Uptime:   time.Since(s.started).Seconds(),
		Agent:    s.version,
		Features: features,
	})
}

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	opts, err := parseShot(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	img, screen, err := s.shoot(opts)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	b := img.Bounds()
	w.Header().Set("X-Screen-Width", strconv.Itoa(screen.X))
	w.Header().Set("X-Screen-Height", strconv.Itoa(screen.Y))
	w.Header().Set("X-Image-Width", strconv.Itoa(b.Dx()))
	w.Header().Set("X-Image-Height", strconv.Itoa(b.Dy()))
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

func (s *Server) handleCursor(w http.ResponseWriter, r *http.Request) {
	x, y, err := s.disp.Pointer()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, api.Cursor{X: x, Y: y})
}

func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	var in api.Input
	if !decode(w, r, &in) {
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
	if !decode(w, r, &req) {
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
	if strings.HasPrefix(p, "~/") {
		p = p[2:]
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
	w.Header().Set("X-Path", p)
	_, _ = io.Copy(w, f)
}

func (s *Server) handleRecordStart(w http.ResponseWriter, r *http.Request) {
	var req api.Record
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
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

func (s *Server) handleWindows(w http.ResponseWriter, r *http.Request) {
	list, err := s.disp.Windows()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleWindowAction(w http.ResponseWriter, r *http.Request) {
	var req api.WindowAction
	if !decode(w, r, &req) {
		return
	}
	if err := s.disp.WindowAction(req.ID, req.Action); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleClipboardGet(w http.ResponseWriter, r *http.Request) {
	text, err := s.clipboardGet()
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, api.Clipboard{Text: text})
}

func (s *Server) handleClipboardSet(w http.ResponseWriter, r *http.Request) {
	var c api.Clipboard
	if !decode(w, r, &c) {
		return
	}
	if err := s.clipboardSet(c.Text); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleBrowserOpen(w http.ResponseWriter, r *http.Request) {
	var req api.BrowserOpen
	if !decode(w, r, &req) {
		return
	}
	res, err := s.browser.open(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleBrowserRead(w http.ResponseWriter, r *http.Request) {
	var req api.BrowserRead
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	st, err := s.browser.read(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleBrowserClick(w http.ResponseWriter, r *http.Request) {
	var req api.BrowserClick
	if !decode(w, r, &req) {
		return
	}
	res, err := s.browser.click(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleBrowserType(w http.ResponseWriter, r *http.Request) {
	var req api.BrowserType
	if !decode(w, r, &req) {
		return
	}
	res, err := s.browser.typeInto(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleBrowserEval(w http.ResponseWriter, r *http.Request) {
	var req api.BrowserEval
	if !decode(w, r, &req) {
		return
	}
	res, err := s.browser.eval(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, api.Error{Error: err.Error()})
}
