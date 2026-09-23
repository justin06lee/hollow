// Package client talks to a hollow host.
//
// It is what the hollow CLI uses, and what bangboo uses: one method per thing
// the API can do, taking and returning the types in package api, with no
// opinion about what to do with a screenshot once you have it.
//
// A host is usually reachable at more than one address — its makima name,
// its Tailscale address, loopback when you are on it. A client can be given
// all of them; it uses whichever answers, preferring them in the order given,
// and moves to another when the one it was using stops answering.
package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/justin06lee/hollow/api"
)

// UserAgent is sent with every request.
var UserAgent = "hollow"

// Client is a connection to one host.
type Client struct {
	URLs  []string
	Token string
	HTTP  *http.Client

	// Name, when set, is checked against what each address says it is, so
	// that an address which happens to reach a different hollow — loopback
	// on a machine running its own, say — is not mistaken for this one.
	Name string

	mu   sync.Mutex
	base string // the URL that answered last
}

// New makes a client for a host at one url with a bearer token.
func New(rawURL, token string) *Client {
	return NewMulti([]string{rawURL}, token)
}

// NewMulti makes a client for a host reachable at any of urls.
func NewMulti(urls []string, token string) *Client {
	clean := make([]string, 0, len(urls))
	for _, u := range urls {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			clean = append(clean, u)
		}
	}
	return &Client{URLs: clean, Token: token, HTTP: &http.Client{}}
}

// Connect is what a client needs to reach a host, as one string a person or
// a program can paste: the host's name, every address it answers at, and
// its token.
type Connect struct {
	URL   string   `json:"u,omitempty"` // the first address; all a v1 code had
	URLs  []string `json:"us,omitempty"`
	Token string   `json:"t"`
	Name  string   `json:"n,omitempty"`
}

const connectPrefix = "hollow1-"

// Addresses is every address in the code, preferred first.
func (c Connect) Addresses() []string {
	if len(c.URLs) > 0 {
		return c.URLs
	}
	if c.URL != "" {
		return []string{c.URL}
	}
	return nil
}

// Encode renders a connect code.
func (c Connect) Encode() string {
	if len(c.URLs) > 0 {
		c.URL = c.URLs[0]
		if len(c.URLs) == 1 {
			c.URLs = nil
		}
	}
	b, _ := json.Marshal(c)
	return connectPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParseConnect reads a connect code.
func ParseConnect(code string) (Connect, error) {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(code, connectPrefix) {
		return Connect{}, errors.New("not a hollow connect code (they start with " + connectPrefix + ")")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, connectPrefix))
	if err != nil {
		return Connect{}, fmt.Errorf("bad connect code: %w", err)
	}
	var c Connect
	if err := json.Unmarshal(raw, &c); err != nil {
		return Connect{}, fmt.Errorf("bad connect code: %w", err)
	}
	if len(c.Addresses()) == 0 || c.Token == "" {
		return Connect{}, errors.New("bad connect code: missing address or token")
	}
	return c, nil
}

// FromConnect makes a client from a connect code.
func FromConnect(code string) (*Client, error) {
	c, err := ParseConnect(code)
	if err != nil {
		return nil, err
	}
	cl := NewMulti(c.Addresses(), c.Token)
	cl.Name = c.Name
	return cl, nil
}

// Hello asks one address whether a hollow answers there. It needs no token.
func Hello(ctx context.Context, base string) (api.Hello, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/hello", nil)
	if err != nil {
		return api.Hello{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return api.Hello{}, err
	}
	defer resp.Body.Close()
	var h api.Hello
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h) != nil || h.Hollow == "" {
		return api.Hello{}, fmt.Errorf("%s is not a hollow", base)
	}
	return h, nil
}

// Base is the address in use, working one out if there is none yet: every
// address is asked at once, and the earliest in the list that answers wins.
func (c *Client) Base(ctx context.Context) (string, error) {
	c.mu.Lock()
	base := c.base
	c.mu.Unlock()
	if base != "" {
		return base, nil
	}
	if len(c.URLs) == 0 {
		return "", errors.New("no address for this host")
	}
	if len(c.URLs) == 1 {
		c.setBase(c.URLs[0])
		return c.URLs[0], nil
	}
	// Two rounds: a mesh path that is changing under us (relay to direct,
	// a laptop waking) can drop the first probe and answer the second.
	for round := 0; round < 2; round++ {
		if b, ok := c.probe(ctx); ok {
			return b, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		time.Sleep(700 * time.Millisecond)
	}
	return "", fmt.Errorf("host unreachable at %s", strings.Join(c.URLs, ", "))
}

// probe asks every address at once; the earliest in the list that answers,
// as the right host, wins, and it stops as soon as every address ahead of
// that one has been heard from.
func (c *Client) probe(ctx context.Context) (string, bool) {
	type answer struct {
		i   int
		err error
	}
	ch := make(chan answer, len(c.URLs))
	for i, u := range c.URLs {
		go func(i int, u string) {
			h, err := Hello(ctx, u)
			if err == nil && c.Name != "" && h.Name != c.Name {
				err = fmt.Errorf("%s is %s, not %s", u, h.Name, c.Name)
			}
			ch <- answer{i, err}
		}(i, u)
	}
	ok := make([]bool, len(c.URLs))
	done := make([]bool, len(c.URLs))
	for range c.URLs {
		a := <-ch
		done[a.i], ok[a.i] = true, a.err == nil
		for i := range c.URLs {
			if ok[i] {
				c.setBase(c.URLs[i])
				return c.URLs[i], true
			}
			if !done[i] {
				break
			}
		}
	}
	return "", false
}

func (c *Client) setBase(b string) {
	c.mu.Lock()
	c.base = b
	c.mu.Unlock()
}

// Reachable reports which address answered, working it out if need be.
func (c *Client) Reachable(ctx context.Context) (string, error) { return c.Base(ctx) }

func isNetErr(err error) bool {
	var ne net.Error
	var oe *net.OpError
	return errors.As(err, &oe) || (errors.As(err, &ne) && ne.Timeout()) || errors.Is(err, io.EOF)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		base, err := c.Base(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("User-Agent", UserAgent)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			// The address stopped answering: forget it, and try again once —
			// at another address if there is one — when the request can be
			// sent again.
			if isNetErr(err) && attempt == 0 && ctx.Err() == nil && (body == nil || req.GetBody != nil) && (len(c.URLs) > 1 || method == http.MethodGet) {
				c.setBase("")
				if req.GetBody != nil {
					if body, err = req.GetBody(); err != nil {
						return nil, err
					}
				}
				continue
			}
			return nil, err
		}
		if resp.StatusCode >= 400 {
			defer resp.Body.Close()
			var e api.Error
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			if json.Unmarshal(data, &e) == nil && e.Error != "" {
				return nil, &Error{Status: resp.StatusCode, Message: e.Error}
			}
			return nil, &Error{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
		}
		return resp, nil
	}
}

func jsonBody(in any) (io.Reader, string, error) {
	if in == nil {
		return nil, "", nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(b), "application/json", nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	body, ct, err := jsonBody(in)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, method, path, body, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) doBytes(ctx context.Context, method, path string, in any) ([]byte, http.Header, error) {
	body, ct, err := jsonBody(in)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.do(ctx, method, path, body, ct)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.Header, err
}

// Error is a non-2xx answer from the host.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("hollow: HTTP %d", e.Status)
	}
	return e.Message
}

// IsStatus reports whether err is an answer from the host with that status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

func deskPath(id string) string { return "/v1/desks/" + url.PathEscape(id) }

// Status asks the host about itself.
func (c *Client) Status(ctx context.Context) (api.Status, error) {
	var s api.Status
	return s, c.doJSON(ctx, http.MethodGet, "/v1/status", nil, &s)
}

// Images lists the golden images.
func (c *Client) Images(ctx context.Context) ([]api.Image, error) {
	var out []api.Image
	return out, c.doJSON(ctx, http.MethodGet, "/v1/images", nil, &out)
}

// Image is one golden image's state.
func (c *Client) Image(ctx context.Context, osName string) (api.Image, error) {
	var out api.Image
	return out, c.doJSON(ctx, http.MethodGet, "/v1/images/"+url.PathEscape(osName), nil, &out)
}

// Pull starts building an image. It returns at once; poll Image.
func (c *Client) Pull(ctx context.Context, osName string) (api.Image, error) {
	var out api.Image
	return out, c.doJSON(ctx, http.MethodPost, "/v1/images/"+url.PathEscape(osName)+"/pull", nil, &out)
}

// PullAndWait builds an image and blocks until it is ready or has failed,
// reporting progress on the way. An image already being built is waited for.
func (c *Client) PullAndWait(ctx context.Context, osName string, progress func(api.Image)) (api.Image, error) {
	img, err := c.Pull(ctx, osName)
	if err != nil && !(IsStatus(err, http.StatusConflict) && strings.Contains(err.Error(), "already")) {
		return img, err
	}
	last := ""
	for {
		img, err = c.Image(ctx, osName)
		if err != nil {
			return img, err
		}
		if line := img.State + " " + img.Progress; line != last && progress != nil {
			last = line
			progress(img)
		}
		switch img.State {
		case api.ImageReady:
			return img, nil
		case api.ImageFailed:
			return img, fmt.Errorf("%s image: %s", osName, img.Error)
		}
		select {
		case <-ctx.Done():
			return img, ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

// Desks lists the running desks.
func (c *Client) Desks(ctx context.Context) ([]api.Desk, error) {
	var out []api.Desk
	return out, c.doJSON(ctx, http.MethodGet, "/v1/desks", nil, &out)
}

// Desk is one desk, by id or name.
func (c *Client) Desk(ctx context.Context, id string) (api.Desk, error) {
	var out api.Desk
	return out, c.doJSON(ctx, http.MethodGet, deskPath(id), nil, &out)
}

// Create boots a desk. It returns as soon as the VM is running; use Wait
// for the moment it can be driven. A name already in use is a 409.
func (c *Client) Create(ctx context.Context, spec api.DeskSpec) (api.Desk, error) {
	var out api.Desk
	return out, c.doJSON(ctx, http.MethodPost, "/v1/desks", spec, &out)
}

// Wait blocks until a desk is ready, or has stopped or failed.
func (c *Client) Wait(ctx context.Context, id string) (api.Desk, error) {
	for {
		d, err := c.Desk(ctx, id)
		if err != nil {
			return d, err
		}
		switch d.State {
		case api.DeskReady:
			return d, nil
		case api.DeskStopped, api.DeskFailed:
			return d, fmt.Errorf("desk %s %s: %s", id, d.State, d.Error)
		}
		select {
		case <-ctx.Done():
			return d, ctx.Err()
		case <-time.After(700 * time.Millisecond):
		}
	}
}

// Delete stops and removes a desk.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, deskPath(id), nil, nil)
}

// Health is what the desk's agent says about its display.
func (c *Client) Health(ctx context.Context, id string) (api.Health, error) {
	var out api.Health
	return out, c.doJSON(ctx, http.MethodGet, deskPath(id)+"/health", nil, &out)
}

// Logs is the desk's serial console so far.
func (c *Client) Logs(ctx context.Context, id string) (string, error) {
	b, _, err := c.doBytes(ctx, http.MethodGet, deskPath(id)+"/logs", nil)
	return string(b), err
}

// ShotOptions shape a screenshot. The zero value is the whole screen as PNG,
// with the pointer, straight away.
type ShotOptions struct {
	X, Y, W, H int    // a region, in screen pixels; W and H zero for all of it
	FitW, FitH int    // scale to fit inside this box; zero to leave the size alone
	SettleMS   int    // wait up to this long for the screen to stop changing
	NoCursor   bool   // leave the pointer out
	Format     string // png (default) or jpeg
	Quality    int    // jpeg quality
}

// Shot is a screenshot and the geometry needed to use it.
type Shot struct {
	Data             []byte
	MIME             string
	ScreenW, ScreenH int // the whole screen
	ImageW, ImageH   int // this image
}

// Shoot takes a screenshot as asked.
func (c *Client) Shoot(ctx context.Context, id string, o ShotOptions) (Shot, error) {
	q := url.Values{}
	if o.W > 0 && o.H > 0 {
		q.Set("x", strconv.Itoa(o.X))
		q.Set("y", strconv.Itoa(o.Y))
		q.Set("w", strconv.Itoa(o.W))
		q.Set("h", strconv.Itoa(o.H))
	}
	if o.FitW > 0 && o.FitH > 0 {
		q.Set("fit", fmt.Sprintf("%dx%d", o.FitW, o.FitH))
	}
	if o.SettleMS > 0 {
		q.Set("settle", strconv.Itoa(o.SettleMS))
	}
	if o.NoCursor {
		q.Set("cursor", "0")
	}
	if o.Format != "" {
		q.Set("format", o.Format)
	}
	if o.Quality > 0 {
		q.Set("quality", strconv.Itoa(o.Quality))
	}
	path := deskPath(id) + "/screenshot"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	data, h, err := c.doBytes(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Shot{}, err
	}
	num := func(k string) int { n, _ := strconv.Atoi(h.Get(k)); return n }
	return Shot{
		Data: data, MIME: h.Get("Content-Type"),
		ScreenW: num("X-Screen-Width"), ScreenH: num("X-Screen-Height"),
		ImageW: num("X-Image-Width"), ImageH: num("X-Image-Height"),
	}, nil
}

// Screenshot is the desk's screen right now, as PNG.
func (c *Client) Screenshot(ctx context.Context, id string) ([]byte, error) {
	s, err := c.Shoot(ctx, id, ShotOptions{})
	return s.Data, err
}

// ScreenshotJPEG is the same, smaller, at the given quality (1–100).
func (c *Client) ScreenshotJPEG(ctx context.Context, id string, quality int) ([]byte, error) {
	s, err := c.Shoot(ctx, id, ShotOptions{Format: "jpeg", Quality: quality})
	return s.Data, err
}

// Cursor is where the desk's pointer is.
func (c *Client) Cursor(ctx context.Context, id string) (api.Cursor, error) {
	var out api.Cursor
	return out, c.doJSON(ctx, http.MethodGet, deskPath(id)+"/cursor", nil, &out)
}

// Input does one thing to the desk's pointer or keyboard.
func (c *Client) Input(ctx context.Context, id string, in api.Input) error {
	return c.doJSON(ctx, http.MethodPost, deskPath(id)+"/input", in, nil)
}

// Exec runs a program on the desk.
func (c *Client) Exec(ctx context.Context, id string, req api.Exec) (api.ExecResult, error) {
	var out api.ExecResult
	return out, c.doJSON(ctx, http.MethodPost, deskPath(id)+"/exec", req, &out)
}

// PutFile writes a file on the desk. Relative paths are under the desk
// user's home.
func (c *Client) PutFile(ctx context.Context, id, path string, r io.Reader) error {
	resp, err := c.do(ctx, http.MethodPut, deskPath(id)+"/files?path="+url.QueryEscape(path), r, "application/octet-stream")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// GetFile reads a file from the desk. Close the reader when done.
func (c *Client) GetFile(ctx context.Context, id, path string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, deskPath(id)+"/files?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// RecordStart begins recording the desk's screen.
func (c *Client) RecordStart(ctx context.Context, id string, fps int) error {
	return c.doJSON(ctx, http.MethodPost, deskPath(id)+"/record/start", api.Record{FPS: fps}, nil)
}

// RecordStop ends the recording and returns the MP4.
func (c *Client) RecordStop(ctx context.Context, id string) ([]byte, error) {
	b, _, err := c.doBytes(ctx, http.MethodPost, deskPath(id)+"/record/stop", nil)
	return b, err
}

// Windows lists the desk's windows, bottom of the stack first.
func (c *Client) Windows(ctx context.Context, id string) ([]api.Window, error) {
	var out []api.Window
	return out, c.doJSON(ctx, http.MethodGet, deskPath(id)+"/windows", nil, &out)
}

// WindowAction activates or closes one window.
func (c *Client) WindowAction(ctx context.Context, id string, a api.WindowAction) error {
	return c.doJSON(ctx, http.MethodPost, deskPath(id)+"/windows", a, nil)
}

// Clipboard reads the desk's clipboard.
func (c *Client) Clipboard(ctx context.Context, id string) (string, error) {
	var out api.Clipboard
	return out.Text, c.doJSON(ctx, http.MethodGet, deskPath(id)+"/clipboard", nil, &out)
}

// SetClipboard writes the desk's clipboard.
func (c *Client) SetClipboard(ctx context.Context, id, text string) error {
	return c.doJSON(ctx, http.MethodPut, deskPath(id)+"/clipboard", api.Clipboard{Text: text}, nil)
}

// BrowserOpen navigates the desk's browser, starting it if need be.
func (c *Client) BrowserOpen(ctx context.Context, id string, req api.BrowserOpen) (api.BrowserResult, error) {
	var out api.BrowserResult
	return out, c.doJSON(ctx, http.MethodPost, deskPath(id)+"/browser/open", req, &out)
}

// BrowserRead is the current page as text and numbered elements.
func (c *Client) BrowserRead(ctx context.Context, id string, req api.BrowserRead) (api.BrowserState, error) {
	var out api.BrowserState
	return out, c.doJSON(ctx, http.MethodPost, deskPath(id)+"/browser/read", req, &out)
}

// BrowserClick clicks an element from the last read.
func (c *Client) BrowserClick(ctx context.Context, id string, req api.BrowserClick) (api.BrowserResult, error) {
	var out api.BrowserResult
	return out, c.doJSON(ctx, http.MethodPost, deskPath(id)+"/browser/click", req, &out)
}

// BrowserType types into an element from the last read.
func (c *Client) BrowserType(ctx context.Context, id string, req api.BrowserType) (api.BrowserResult, error) {
	var out api.BrowserResult
	return out, c.doJSON(ctx, http.MethodPost, deskPath(id)+"/browser/type", req, &out)
}

// BrowserEval runs JavaScript in the page and returns its value as JSON.
func (c *Client) BrowserEval(ctx context.Context, id string, js string) (json.RawMessage, error) {
	var out api.BrowserEvalResult
	return out.Value, c.doJSON(ctx, http.MethodPost, deskPath(id)+"/browser/eval", api.BrowserEval{JS: js}, &out)
}

// Secrets lists the host's vault: names, accounts, fields and sites. Values
// never leave the host.
func (c *Client) Secrets(ctx context.Context) ([]api.Secret, error) {
	var out []api.Secret
	return out, c.doJSON(ctx, http.MethodGet, "/v1/secrets", nil, &out)
}

// SetSecret stores a secret in the host's vault.
func (c *Client) SetSecret(ctx context.Context, name string, in api.SecretSet) (api.Secret, error) {
	var out api.Secret
	return out, c.doJSON(ctx, http.MethodPut, "/v1/secrets/"+url.PathEscape(name), in, &out)
}

// DeleteSecret removes a secret from the host's vault.
func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/secrets/"+url.PathEscape(name), nil, nil)
}
