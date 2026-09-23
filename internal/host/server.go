package host

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

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

// Hostname is what this host calls itself.
func Hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "hollow"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// Server is the API.
type Server struct {
	version string
	token   string
	backend backend.Backend
	images  *image.Manager
	desks   *Desks
	vault   *Vault
	urls    func() []string
	agent   *http.Client
	mux     *http.ServeMux
}

// NewServer wires the API to its parts. urls says where clients can reach
// this host, for status.
func NewServer(version, token string, b backend.Backend, images *image.Manager, desks *Desks, vault *Vault, urls func() []string) *Server {
	s := &Server{
		version: version,
		token:   token,
		backend: b,
		images:  images,
		desks:   desks,
		vault:   vault,
		urls:    urls,
		// No timeout: an exec may legitimately run for minutes and a
		// recording stop returns a whole file. The caller's context bounds it.
		agent: &http.Client{},
		mux:   http.NewServeMux(),
	}
	m := s.mux
	m.HandleFunc("GET /v1/hello", s.handleHello)
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
	m.HandleFunc("GET /v1/secrets", s.handleSecrets)
	m.HandleFunc("PUT /v1/secrets/{name}", s.handleSecretSet)
	m.HandleFunc("DELETE /v1/secrets/{name}", s.handleSecretDelete)
	return s
}

// ServeHTTP checks the token on everything except hello and the agent
// download. Hello says only that a hollow is here; the agent is fetched by
// guests, which have no token, before they know anything.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/hello" || strings.HasPrefix(r.URL.Path, "/v1/agent/") {
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

func (s *Server) handleHello(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.Hello{Hollow: s.version, Name: Hostname()})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	total, avail := meminfo()
	st := api.Status{
		Version: s.version,
		Name:    Hostname(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Backend: s.backend.Name(),
		Ready:   true,
		CPUs:    runtime.NumCPU(),
		MemMB:   total,
		FreeMB:  avail,
		URLs:    s.urls(),
		Desks:   len(s.desks.List()),
		Images:  s.images.List(),
	}
	if err := s.backend.Check(); err != nil {
		st.Ready, st.Problem = false, err.Error()
	}
	writeJSON(w, http.StatusOK, st)
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
	_, _ = io.WriteString(w, s.vault.Scrub(out))
}

// handleAgent forwards everything under a desk to the agent inside it,
// with the desk's own key. The client's token is not passed on: a guest runs
// whatever a bot asked it to, and has no business holding the host's key.
//
// On the way in, placeholders for secrets are filled in; on the way out,
// secret values are replaced by their placeholders.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	rest := r.PathValue("rest")
	base, key, err := s.desks.Agent(ref)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	target := base + "/" + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	var body io.Reader = r.Body
	length := r.ContentLength
	if r.Method == http.MethodPost && fillable[rest] {
		data, err := io.ReadAll(io.LimitReader(r.Body, maxFillBody))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if data, err = s.fill(rest, data); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		body, length = bytes.NewReader(data), int64(len(data))
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.ContentLength = length
	req.Header.Set("Authorization", "Bearer "+key)
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
		writeError(w, http.StatusBadGateway, fmt.Errorf("desk %s: agent: %w", ref, err))
		return
	}
	defer resp.Body.Close()
	copyHeaders := func(skipLength bool) {
		for k, vs := range resp.Header {
			if k == "Content-Type" || (k == "Content-Length" && !skipLength) || strings.HasPrefix(k, "X-") {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
		}
	}
	if ct := resp.Header.Get("Content-Type"); s.vault.Active() && scrubbable(ct, resp.ContentLength) {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxScrubBody+1))
		if err == nil && len(data) <= maxScrubBody {
			if strings.HasPrefix(ct, "application/json") {
				data = s.vault.ScrubJSON(data)
			} else if utf8.Valid(data) {
				data = []byte(s.vault.Scrub(string(data)))
			}
			copyHeaders(true)
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(data)
			return
		}
		// Too big to be text anyone reads whole: send what was read, then
		// the rest, as it is.
		copyHeaders(true)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	copyHeaders(false)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// fillable are the desk routes whose bodies may hold secret placeholders.
var fillable = map[string]bool{"input": true, "browser/type": true, "exec": true}

const (
	maxFillBody  = 64 << 20
	maxScrubBody = 16 << 20
)

// scrubbable says whether a response from a desk is text that could carry a
// secret back: JSON, text, and files, which are usually text when they are
// small. Pictures and video go through untouched.
func scrubbable(ct string, length int64) bool {
	if length > maxScrubBody {
		return false
	}
	for _, p := range []string{"application/json", "text/", "application/octet-stream"} {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

// fill puts secrets into a request for a desk, and the guards that say
// where they may go. Guards a client sent are thrown away: they are the
// host's to set.
func (s *Server) fill(rest string, data []byte) ([]byte, error) {
	if !bytes.Contains(data, []byte("{{")) && !bytes.Contains(data, []byte(`"guards"`)) {
		return data, nil
	}
	switch rest {
	case "input":
		var in api.Input
		if json.Unmarshal(data, &in) != nil {
			return data, nil // the agent says what is wrong with it
		}
		in.Guards = nil
		if in.Action == api.InputType {
			text, guards, err := s.vault.Fill(in.Text, fillTyped)
			if err != nil {
				return nil, err
			}
			in.Text, in.Guards = text, guards
		}
		return json.Marshal(in)
	case "browser/type":
		var in api.BrowserType
		if json.Unmarshal(data, &in) != nil {
			return data, nil
		}
		text, guards, err := s.vault.Fill(in.Text, fillTyped)
		if err != nil {
			return nil, err
		}
		in.Text, in.Guards = text, guards
		return json.Marshal(in)
	case "exec":
		var in api.Exec
		if json.Unmarshal(data, &in) != nil {
			return data, nil
		}
		var err error
		fill := func(p *string) {
			if err == nil {
				*p, _, err = s.vault.Fill(*p, fillExec)
			}
		}
		fill(&in.Cmd)
		fill(&in.Stdin)
		for i := range in.Args {
			fill(&in.Args[i])
		}
		for i := range in.Env {
			fill(&in.Env[i])
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(in)
	}
	return data, nil
}

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.vault.List())
}

func (s *Server) handleSecretSet(w http.ResponseWriter, r *http.Request) {
	var in api.SecretSet
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sec, err := s.vault.Set(r.PathValue("name"), in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, sec)
}

func (s *Server) handleSecretDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.vault.Delete(r.PathValue("name")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
