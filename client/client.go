// Package client talks to a hollow host.
//
// It is what the hollow CLI uses, and what bangboo is expected to use: one
// method per thing the API can do, taking and returning the types in package
// api, with no opinion about what to do with a screenshot once you have it.
package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/justin06lee/hollow/api"
)

// UserAgent is sent with every request.
var UserAgent = "hollow"

// Client is a connection to one host.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// New makes a client for a host at url with a bearer token.
func New(rawURL, token string) *Client {
	return &Client{URL: strings.TrimRight(rawURL, "/"), Token: token, HTTP: &http.Client{}}
}

// Connect is the two things a client needs, as one string a person can paste.
type Connect struct {
	URL   string `json:"u"`
	Token string `json:"t"`
}

const connectPrefix = "hollow1-"

// Encode renders a connect code.
func (c Connect) Encode() string {
	b, _ := json.Marshal(c)
	return connectPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParseConnect reads a connect code.
func ParseConnect(code string) (Connect, error) {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(code, connectPrefix) {
		return Connect{}, errors.New("not a hollow connect code")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, connectPrefix))
	if err != nil {
		return Connect{}, fmt.Errorf("bad connect code: %w", err)
	}
	var c Connect
	if err := json.Unmarshal(raw, &c); err != nil {
		return Connect{}, fmt.Errorf("bad connect code: %w", err)
	}
	if c.URL == "" || c.Token == "" {
		return Connect{}, errors.New("bad connect code: missing url or token")
	}
	return c, nil
}

// FromConnect makes a client from a connect code.
func FromConnect(code string) (*Client, error) {
	c, err := ParseConnect(code)
	if err != nil {
		return nil, err
	}
	return New(c.URL, c.Token), nil
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
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

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
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

func (c *Client) doBytes(ctx context.Context, method, path string, in any) ([]byte, error) {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, method, path, body, ct)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
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
// reporting progress lines on the way.
func (c *Client) PullAndWait(ctx context.Context, osName string, progress func(api.Image)) (api.Image, error) {
	img, err := c.Pull(ctx, osName)
	if err != nil {
		var e *Error
		if !errors.As(err, &e) || e.Status != http.StatusConflict || !strings.Contains(e.Message, "already") {
			return img, err
		}
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

// Desk is one desk.
func (c *Client) Desk(ctx context.Context, id string) (api.Desk, error) {
	var out api.Desk
	return out, c.doJSON(ctx, http.MethodGet, "/v1/desks/"+url.PathEscape(id), nil, &out)
}

// Create boots a desk. It returns as soon as the VM is running; use Wait
// for the moment it can be driven.
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
	return c.doJSON(ctx, http.MethodDelete, "/v1/desks/"+url.PathEscape(id), nil, nil)
}

// Health is what the desk's agent says about its display.
func (c *Client) Health(ctx context.Context, id string) (api.Health, error) {
	var out api.Health
	return out, c.doJSON(ctx, http.MethodGet, "/v1/desks/"+url.PathEscape(id)+"/health", nil, &out)
}

// Logs is the desk's serial console so far.
func (c *Client) Logs(ctx context.Context, id string) (string, error) {
	b, err := c.doBytes(ctx, http.MethodGet, "/v1/desks/"+url.PathEscape(id)+"/logs", nil)
	return string(b), err
}

// Screenshot is the desk's screen right now, as PNG.
func (c *Client) Screenshot(ctx context.Context, id string) ([]byte, error) {
	return c.doBytes(ctx, http.MethodGet, "/v1/desks/"+url.PathEscape(id)+"/screenshot", nil)
}

// ScreenshotJPEG is the same, smaller, at the given quality (1–100).
func (c *Client) ScreenshotJPEG(ctx context.Context, id string, quality int) ([]byte, error) {
	return c.doBytes(ctx, http.MethodGet, fmt.Sprintf("/v1/desks/%s/screenshot?format=jpeg&quality=%d", url.PathEscape(id), quality), nil)
}

// Input does one thing to the desk's pointer or keyboard.
func (c *Client) Input(ctx context.Context, id string, in api.Input) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/desks/"+url.PathEscape(id)+"/input", in, nil)
}

// Exec runs a program on the desk.
func (c *Client) Exec(ctx context.Context, id string, req api.Exec) (api.ExecResult, error) {
	var out api.ExecResult
	return out, c.doJSON(ctx, http.MethodPost, "/v1/desks/"+url.PathEscape(id)+"/exec", req, &out)
}

// PutFile writes a file on the desk.
func (c *Client) PutFile(ctx context.Context, id, path string, r io.Reader) error {
	resp, err := c.do(ctx, http.MethodPut, "/v1/desks/"+url.PathEscape(id)+"/files?path="+url.QueryEscape(path), r, "application/octet-stream")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// GetFile reads a file from the desk. Close the reader when done.
func (c *Client) GetFile(ctx context.Context, id, path string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/desks/"+url.PathEscape(id)+"/files?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// RecordStart begins recording the desk's screen.
func (c *Client) RecordStart(ctx context.Context, id string, fps int) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/desks/"+url.PathEscape(id)+"/record/start", api.Record{FPS: fps}, nil)
}

// RecordStop ends the recording and returns the MP4.
func (c *Client) RecordStop(ctx context.Context, id string) ([]byte, error) {
	return c.doBytes(ctx, http.MethodPost, "/v1/desks/"+url.PathEscape(id)+"/record/stop", nil)
}
