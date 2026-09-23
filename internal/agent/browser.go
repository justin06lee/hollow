package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/justin06lee/hollow/api"
)

// The browser is Chromium driven over the DevTools protocol, which is what
// makes a web page something an agent can read rather than squint at: the
// page's text and its clickable elements come back as data, and a click by
// element lands on the element whatever the window looks like.
//
// It is the same Chromium a person would see on the desk, so a screenshot
// and a read describe one page, and the agent can switch between them.

const (
	cdpPort         = 9222
	defaultMaxChars = 6000
	defaultMaxElems = 120
	navTimeout      = 25 * time.Second
)

type browser struct {
	s  *Server
	mu sync.Mutex
	// target is the tab the agent last worked in. Kept so that "the page"
	// means the same page from one call to the next, even when a click
	// opened another tab in the background.
	target string
}

type cdpTarget struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
	WS    string `json:"webSocketDebuggerUrl"`
}

var cdpHTTP = &http.Client{Timeout: 3 * time.Second}

func cdpURL(path string) string { return fmt.Sprintf("http://127.0.0.1:%d%s", cdpPort, path) }

func (b *browser) targets() ([]cdpTarget, error) {
	resp, err := cdpHTTP.Get(cdpURL("/json/list"))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var all []cdpTarget
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return nil, err
	}
	pages := all[:0]
	for _, t := range all {
		if t.Type == "page" && !strings.HasPrefix(t.URL, "devtools://") {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

// ensure starts Chromium if its debugging port is not answering.
func (b *browser) ensure() error {
	if resp, err := cdpHTTP.Get(cdpURL("/json/version")); err == nil {
		resp.Body.Close()
		return nil
	}
	bin := ""
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if p, err := exec.LookPath(name); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		return errors.New("no Chromium on this desk")
	}
	profile := filepath.Join(b.s.home, ".config", "hollow-browser")
	cmd := exec.Command(bin,
		fmt.Sprintf("--remote-debugging-port=%d", cdpPort),
		"--user-data-dir="+profile,
		"--no-first-run", "--no-default-browser-check",
		"--password-store=basic",
		"--start-maximized",
		"--hide-crash-restore-bubble",
		"--disable-features=Translate,MediaRouter",
		"about:blank")
	cmd.Env = b.s.env()
	cmd.Dir = b.s.home
	null, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start chromium: %w", err)
	}
	go func() { cmd.Wait(); null.Close() }()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if pages, err := b.targets(); err == nil && len(pages) > 0 {
			return nil
		}
	}
	return errors.New("chromium started but its debugging port never answered")
}

// page picks the tab to work in: the one on screen, so that a read and a
// screenshot always describe the same page — including after a tab was
// opened or switched with the keyboard, which the browser does not report.
// With several windows, each showing a tab, the one used last wins.
func (b *browser) page(ctx context.Context) (cdpTarget, int, error) {
	pages, err := b.targets()
	if err != nil {
		return cdpTarget{}, 0, fmt.Errorf("the browser is not running; open a page first (%v)", err)
	}
	if len(pages) == 0 {
		return cdpTarget{}, 0, errors.New("the browser has no tabs open")
	}
	if len(pages) == 1 {
		b.target = pages[0].ID
		return pages[0], 1, nil
	}
	var shown []cdpTarget
	for _, p := range pages {
		if tabVisible(ctx, p) {
			shown = append(shown, p)
		}
	}
	for _, p := range shown {
		if p.ID == b.target {
			return p, len(pages), nil
		}
	}
	if len(shown) > 0 {
		b.target = shown[0].ID
		return shown[0], len(pages), nil
	}
	for _, p := range pages {
		if p.ID == b.target {
			return p, len(pages), nil
		}
	}
	b.target = pages[0].ID
	return pages[0], len(pages), nil
}

func tabVisible(ctx context.Context, t cdpTarget) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, err := dialCDP(ctx, t)
	if err != nil {
		return false
	}
	defer c.close()
	var state string
	return c.eval(ctx, "document.visibilityState", &state) == nil && state == "visible"
}

// cdp is one DevTools session with one tab.
type cdp struct {
	conn *websocket.Conn
	id   int
}

func dialCDP(ctx context.Context, t cdpTarget) (*cdp, error) {
	c, _, err := websocket.Dial(ctx, t.WS, nil)
	if err != nil {
		return nil, fmt.Errorf("devtools: %w", err)
	}
	c.SetReadLimit(64 << 20)
	return &cdp{conn: c}, nil
}

func (c *cdp) close() { c.conn.Close(websocket.StatusNormalClosure, "") }

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// call sends one command and waits for its answer, skipping the events
// that arrive in between.
func (c *cdp) call(ctx context.Context, method string, params any, out any) error {
	c.id++
	id := c.id
	msg, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, msg); err != nil {
		return err
	}
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return err
		}
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *cdpError       `json:"error"`
		}
		if json.Unmarshal(data, &reply) != nil || reply.ID != id {
			continue
		}
		if reply.Error != nil {
			return fmt.Errorf("%s: %s", method, reply.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(reply.Result, out)
		}
		return nil
	}
}

// eval runs an expression in the page and decodes its value into out.
func (c *cdp) eval(ctx context.Context, expr string, out any) error {
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	err := c.call(ctx, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true, "userGesture": true,
	}, &r)
	if err != nil {
		return err
	}
	if r.Exception != nil {
		msg := r.Exception.Text
		if r.Exception.Exception != nil && r.Exception.Exception.Description != "" {
			msg = r.Exception.Exception.Description
		}
		return fmt.Errorf("javascript: %s", firstLine(msg))
	}
	if out == nil {
		return nil
	}
	if len(r.Result.Value) == 0 {
		return json.Unmarshal([]byte("null"), out)
	}
	return json.Unmarshal(r.Result.Value, out)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// waitLoaded waits for the document to finish loading, and a moment more
// for the scripts that run after it to put the page in its real shape.
func (c *cdp) waitLoaded(ctx context.Context, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var state string
		if err := c.eval(ctx, "document.readyState", &state); err == nil && state == "complete" {
			time.Sleep(350 * time.Millisecond)
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func (b *browser) session(ctx context.Context) (*cdp, cdpTarget, int, error) {
	t, n, err := b.page(ctx)
	if err != nil {
		return nil, t, 0, err
	}
	c, err := dialCDP(ctx, t)
	if err != nil {
		return nil, t, 0, err
	}
	return c, t, n, nil
}

// Every browser call has a deadline. A browser that stops answering is
// almost always a desk out of memory, and saying so in seconds is worth more
// than an agent waiting minutes for a page that is swapping.
const (
	openBudget = 45 * time.Second
	callBudget = 30 * time.Second
)

// budget bounds ctx and returns a function that turns a deadline into an
// explanation.
func budget(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc, func(error) error) {
	cctx, cancel := context.WithTimeout(ctx, d)
	explain := func(err error) error {
		if err == nil || cctx.Err() != context.DeadlineExceeded {
			return err
		}
		return fmt.Errorf("the browser did not answer within %s%s", d, memoryHint())
	}
	return cctx, cancel, explain
}

// memoryHint says how short of memory the desk is, when it is.
func memoryHint() string {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return ""
	}
	var total, avail, swapFree int
	for _, line := range strings.Split(string(data), "\n") {
		var n int
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			fmt.Sscanf(line[9:], "%d", &n)
			total = n / 1024
		case strings.HasPrefix(line, "MemAvailable:"):
			fmt.Sscanf(line[13:], "%d", &n)
			avail = n / 1024
		case strings.HasPrefix(line, "SwapFree:"):
			fmt.Sscanf(line[9:], "%d", &n)
			swapFree = n / 1024
		}
	}
	if avail > 0 && avail < total/8 {
		return fmt.Sprintf(" — this desk is nearly out of memory (%d MB of %d free, %d MB swap free): close tabs, or use a desk with more memory (mem_mb)", avail, total, swapFree)
	}
	return " — the page may still be loading; browser_read tries again"
}

func (b *browser) open(ctx context.Context, req api.BrowserOpen) (res api.BrowserResult, err error) {
	ctx, cancel, explain := budget(ctx, openBudget)
	defer cancel()
	defer func() { err = explain(err) }()
	b.mu.Lock()
	defer b.mu.Unlock()
	u := strings.TrimSpace(req.URL)
	if u == "" {
		return api.BrowserResult{}, errors.New("url is required")
	}
	if !strings.Contains(u, "://") && !strings.HasPrefix(u, "about:") {
		u = "https://" + u
	}
	if err := b.ensure(); err != nil {
		return api.BrowserResult{}, err
	}
	if req.NewTab {
		r, err := http.NewRequestWithContext(ctx, http.MethodPut, cdpURL("/json/new?"+url.QueryEscape(u)), nil)
		if err != nil {
			return api.BrowserResult{}, err
		}
		resp, err := cdpHTTP.Do(r)
		if err != nil {
			return api.BrowserResult{}, err
		}
		var t cdpTarget
		err = json.NewDecoder(resp.Body).Decode(&t)
		resp.Body.Close()
		if err != nil {
			return api.BrowserResult{}, fmt.Errorf("new tab: %w", err)
		}
		b.target = t.ID
		if resp, err := cdpHTTP.Get(cdpURL("/json/activate/" + t.ID)); err == nil {
			resp.Body.Close()
		}
		c, _, _, err := b.session(ctx)
		if err != nil {
			return api.BrowserResult{}, err
		}
		defer c.close()
		c.waitLoaded(ctx, navTimeout)
		var href string
		_ = c.eval(ctx, "location.href", &href)
		return api.BrowserResult{URL: href, Navigated: true}, nil
	}
	c, t, _, err := b.session(ctx)
	if err != nil {
		return api.BrowserResult{}, err
	}
	defer c.close()
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	if err := c.call(ctx, "Page.navigate", map[string]any{"url": u}, &nav); err != nil {
		return api.BrowserResult{}, err
	}
	if nav.ErrorText != "" {
		return api.BrowserResult{}, fmt.Errorf("could not load %s: %s", u, nav.ErrorText)
	}
	c.waitLoaded(ctx, navTimeout)
	// Bring the tab to the front, so a screenshot shows the same page.
	if resp, err := cdpHTTP.Get(cdpURL("/json/activate/" + t.ID)); err == nil {
		resp.Body.Close()
	}
	var href string
	_ = c.eval(ctx, "location.href", &href)
	return api.BrowserResult{URL: href, Navigated: true}, nil
}

func (b *browser) read(ctx context.Context, req api.BrowserRead) (st0 api.BrowserState, err error) {
	ctx, cancel, explain := budget(ctx, callBudget)
	defer cancel()
	defer func() { err = explain(err) }()
	b.mu.Lock()
	defer b.mu.Unlock()
	c, _, tabs, err := b.session(ctx)
	if err != nil {
		return api.BrowserState{}, err
	}
	defer c.close()
	if req.MaxChars <= 0 {
		req.MaxChars = defaultMaxChars
	}
	if req.MaxElements <= 0 {
		req.MaxElements = defaultMaxElems
	}
	var st api.BrowserState
	expr := fmt.Sprintf("(%s)(%d,%d,%d)", readScript, req.Offset, req.MaxChars, req.MaxElements)
	if err := c.eval(ctx, expr, &st); err != nil {
		return st, err
	}
	st.Tabs = tabs
	return st, nil
}

// clickable finds an element from the last read, scrolls it into view, and
// returns its centre in the page's viewport.
func (b *browser) locate(ctx context.Context, c *cdp, index int) (float64, float64, error) {
	var p struct {
		OK  bool    `json:"ok"`
		Err string  `json:"err"`
		X   float64 `json:"x"`
		Y   float64 `json:"y"`
	}
	expr := fmt.Sprintf(`(() => {
		const el = document.querySelector('[data-hollow-idx="%d"]');
		if (!el) return {ok:false, err:"no element %d on this page: the page has changed since it was read; read it again"};
		el.scrollIntoView({block:"center", inline:"center"});
		const r = el.getBoundingClientRect();
		if (r.width === 0 && r.height === 0) return {ok:false, err:"element %d is not visible"};
		return {ok:true, x:r.left + r.width/2, y:r.top + Math.min(r.height/2, 20)};
	})()`, index, index, index)
	if err := c.eval(ctx, expr, &p); err != nil {
		return 0, 0, err
	}
	if !p.OK {
		return 0, 0, errors.New(p.Err)
	}
	return p.X, p.Y, nil
}

func (c *cdp) mouseClick(ctx context.Context, x, y float64) error {
	for _, ev := range []map[string]any{
		{"type": "mouseMoved", "x": x, "y": y},
		{"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1},
		{"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1},
	} {
		if err := c.call(ctx, "Input.dispatchMouseEvent", ev, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *cdp) key(ctx context.Context, key, code string, vk int, text string, modifiers int, commands []string) error {
	down := map[string]any{"type": "keyDown", "key": key, "code": code, "windowsVirtualKeyCode": vk, "modifiers": modifiers}
	if text != "" {
		down["text"] = text
	}
	if len(commands) > 0 {
		down["commands"] = commands
	}
	if err := c.call(ctx, "Input.dispatchKeyEvent", down, nil); err != nil {
		return err
	}
	return c.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": key, "code": code, "windowsVirtualKeyCode": vk, "modifiers": modifiers}, nil)
}

// settleAfter waits for whatever an action set off: a navigation, or a page
// redrawing itself. It reports whether the address changed.
func (c *cdp) settleAfter(ctx context.Context, before string) (string, bool) {
	time.Sleep(400 * time.Millisecond)
	c.waitLoaded(ctx, 10*time.Second)
	var href string
	_ = c.eval(ctx, "location.href", &href)
	return href, href != before
}

func (b *browser) click(ctx context.Context, req api.BrowserClick) (res0 api.BrowserResult, err error) {
	ctx, cancel, explain := budget(ctx, callBudget)
	defer cancel()
	defer func() { err = explain(err) }()
	b.mu.Lock()
	defer b.mu.Unlock()
	c, _, before, err := b.sessionAt(ctx)
	if err != nil {
		return api.BrowserResult{}, err
	}
	defer c.close()
	x, y, err := b.locate(ctx, c, req.Index)
	if err != nil {
		return api.BrowserResult{}, err
	}
	if err := c.mouseClick(ctx, x, y); err != nil {
		return api.BrowserResult{}, err
	}
	href, moved := c.settleAfter(ctx, before)
	return api.BrowserResult{URL: href, Navigated: moved}, nil
}

func (b *browser) sessionAt(ctx context.Context) (*cdp, cdpTarget, string, error) {
	c, t, _, err := b.session(ctx)
	if err != nil {
		return nil, t, "", err
	}
	var href string
	_ = c.eval(ctx, "location.href", &href)
	return c, t, href, nil
}

func (b *browser) typeInto(ctx context.Context, req api.BrowserType) (res0 api.BrowserResult, err error) {
	ctx, cancel, explain := budget(ctx, callBudget)
	defer cancel()
	defer func() { err = explain(err) }()
	b.mu.Lock()
	defer b.mu.Unlock()
	c, _, before, err := b.sessionAt(ctx)
	if err != nil {
		return api.BrowserResult{}, err
	}
	defer c.close()
	x, y, err := b.locate(ctx, c, req.Index)
	if err != nil {
		return api.BrowserResult{}, err
	}
	// Click rather than focus(): a click is what a person does, and what
	// custom inputs built from divs are listening for.
	if err := c.mouseClick(ctx, x, y); err != nil {
		return api.BrowserResult{}, err
	}
	if req.Clear {
		// ctrl+a then Backspace, as keystrokes, so that frameworks which
		// track an input's value through its events see it emptied.
		if err := c.key(ctx, "a", "KeyA", 65, "", 2, []string{"selectAll"}); err != nil {
			return api.BrowserResult{}, err
		}
		if err := c.key(ctx, "Backspace", "Backspace", 8, "", 0, nil); err != nil {
			return api.BrowserResult{}, err
		}
	}
	if req.Text != "" {
		if err := c.call(ctx, "Input.insertText", map[string]any{"text": req.Text}, nil); err != nil {
			return api.BrowserResult{}, err
		}
	}
	if req.Submit {
		if err := c.key(ctx, "Enter", "Enter", 13, "\r", 0, nil); err != nil {
			return api.BrowserResult{}, err
		}
		href, moved := c.settleAfter(ctx, before)
		return api.BrowserResult{URL: href, Navigated: moved}, nil
	}
	var href string
	_ = c.eval(ctx, "location.href", &href)
	return api.BrowserResult{URL: href}, nil
}

func (b *browser) eval(ctx context.Context, req api.BrowserEval) (ev0 api.BrowserEvalResult, err error) {
	ctx, cancel, explain := budget(ctx, callBudget)
	defer cancel()
	defer func() { err = explain(err) }()
	b.mu.Lock()
	defer b.mu.Unlock()
	if strings.TrimSpace(req.JS) == "" {
		return api.BrowserEvalResult{}, errors.New("js is required")
	}
	c, _, _, err := b.session(ctx)
	if err != nil {
		return api.BrowserEvalResult{}, err
	}
	defer c.close()
	var v json.RawMessage
	if err := c.eval(ctx, req.JS, &v); err != nil {
		return api.BrowserEvalResult{}, err
	}
	if len(v) == 0 {
		v = json.RawMessage("null")
	}
	return api.BrowserEvalResult{Value: v}, nil
}

// readScript turns a page into text an agent can act on. It numbers every
// visible interactive element with a data attribute, so a later click finds
// the same element by its number rather than by coordinates that a scroll
// or a resize would have invalidated.
//
// Screen coordinates are worked out from the window's position and the
// height of the browser's own toolbar, so the numbers also work with a
// pointer click on the desk.
const readScript = `function(offset, maxChars, maxElems) {
  const doc = document;
  doc.querySelectorAll('[data-hollow-idx]').forEach(e => e.removeAttribute('data-hollow-idx'));
  const sel = 'a[href], button, input:not([type=hidden]), textarea, select, summary, label[for], ' +
    '[role=button], [role=link], [role=checkbox], [role=radio], [role=tab], [role=menuitem], ' +
    '[role=option], [role=switch], [role=textbox], [role=combobox], [role=searchbox], ' +
    '[contenteditable=""], [contenteditable=true], [onclick], [tabindex]:not([tabindex="-1"])';
  const dpr = window.devicePixelRatio || 1;
  const border = Math.max(0, (window.outerWidth - window.innerWidth) / 2);
  const left = window.screenX + border;
  const top = window.screenY + Math.max(0, window.outerHeight - window.innerHeight - border);
  const vw = window.innerWidth, vh = window.innerHeight;
  const clean = s => (s || '').replace(/\s+/g, ' ').trim();
  const cut = (s, n) => s.length > n ? s.slice(0, n - 1) + '…' : s;
  const kindOf = el => {
    const t = el.tagName.toLowerCase(), role = el.getAttribute('role');
    if (t === 'input') {
      const ty = (el.getAttribute('type') || 'text').toLowerCase();
      if (['checkbox','radio','submit','button','reset','file','range','color'].includes(ty)) return ty === 'submit' || ty === 'reset' ? 'button' : ty;
      return 'input';
    }
    if (t === 'a') return 'link';
    if (t === 'button' || t === 'summary') return 'button';
    if (t === 'textarea' || t === 'select') return t;
    if (role) return role;
    if (el.isContentEditable) return 'textbox';
    return 'clickable';
  };
  const labelOf = el => {
    let s = el.getAttribute('aria-label') || '';
    if (!s && el.labels && el.labels.length) s = el.labels[0].innerText;
    if (!s) s = el.innerText || '';
    if (!s) s = el.getAttribute('placeholder') || el.getAttribute('title') || el.getAttribute('alt') || '';
    if (!s) { const img = el.querySelector && el.querySelector('img[alt]'); if (img) s = img.getAttribute('alt'); }
    if (!s && el.tagName === 'INPUT') s = el.getAttribute('name') || '';
    return cut(clean(s), 80);
  };
  const seen = new Set();
  const inView = [], below = [];
  for (const el of doc.querySelectorAll(sel)) {
    if (seen.has(el)) continue;
    seen.add(el);
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    const cs = getComputedStyle(el);
    if (cs.visibility === 'hidden' || cs.display === 'none' || Number(cs.opacity) === 0) continue;
    const visible = r.bottom > 0 && r.right > 0 && r.top < vh && r.left < vw;
    (visible ? inView : below).push([el, r, visible]);
  }
  const picked = inView.concat(below).slice(0, maxElems);
  const elements = picked.map(([el, r, visible], i) => {
    el.setAttribute('data-hollow-idx', String(i));
    const kind = kindOf(el);
    const e = {index: i, kind, label: labelOf(el), visible,
      x: Math.round(left + (r.left + r.width / 2) * dpr),
      y: Math.round(top + (r.top + Math.min(r.height / 2, 20)) * dpr)};
    if (el.disabled) e.disabled = true;
    if (kind === 'link') e.href = cut(el.href || '', 120);
    if ('value' in el && kind !== 'button' && el.type !== 'password' && el.value) e.value = cut(String(el.value), 80);
    if (el.type === 'checkbox' || el.type === 'radio') e.value = el.checked ? 'checked' : 'unchecked';
    return e;
  });
  const text = (doc.body ? doc.body.innerText : '').replace(/\n{3,}/g, '\n\n');
  const start = Math.max(0, Math.min(offset, text.length));
  return {
    url: location.href, title: doc.title,
    text: text.slice(start, start + maxChars), offset: start, total_chars: text.length,
    elements, more_elements: Math.max(0, inView.length + below.length - picked.length),
    scroll_y: Math.round(window.scrollY),
    page_height: Math.round(Math.max(doc.documentElement.scrollHeight, doc.body ? doc.body.scrollHeight : 0)),
    view_height: vh,
  };
}`
