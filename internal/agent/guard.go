package agent

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/justin06lee/hollow/api"
)

// Guards are how a secret bound to a site stays on it. The host fills the
// secret in and says where it may go; here, the one place that can see
// where the text is really about to land, that is checked just before it is
// typed. A page that talked an agent into typing {{bank}} into its own form
// gets a refusal instead.

// siteAllows reports whether a page at rawURL is one of sites: the host
// itself or under it, over HTTPS unless the site was stored as http://.
func siteAllows(rawURL string, sites []string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return false
	}
	for _, s := range sites {
		plain := strings.HasPrefix(s, "http://")
		s = strings.TrimPrefix(s, "http://")
		if u.Scheme != "https" && !(plain && u.Scheme == "http") {
			continue
		}
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

func checkGuards(pageURL string, guards []api.Guard) error {
	for _, g := range guards {
		if !siteAllows(pageURL, g.Sites) {
			return fmt.Errorf("refused: {{%s}} is only for %s, and this page is %s. If this really is the right site, the user can add it: bangboo secret set %s --merge --site <host>",
				g.Secret, strings.Join(g.Sites, ", "), pageOrigin(pageURL), g.Secret)
		}
	}
	return nil
}

func pageOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host
}

// focusScript says where the keyboard is inside a tab: whether the page has
// it at all (not the address bar, not a dialog), and, when it is in a frame,
// the frame's address rather than the page's.
const focusScript = `(() => {
  let doc = document, href = location.href;
  if (!doc.hasFocus()) return {focus: false, href};
  let el = doc.activeElement;
  while (el && el.tagName === 'IFRAME') {
    let inner;
    try { inner = el.contentDocument; } catch (e) { inner = null; }
    if (!inner) return {focus: true, href, frame: el.src || 'a frame', crossOrigin: true};
    doc = inner; href = inner.location.href; el = inner.activeElement;
  }
  const editable = !!el && (el.isContentEditable || el.tagName === 'INPUT' || el.tagName === 'TEXTAREA');
  return {focus: true, href, editable};
})()`

type keyboardFocus struct {
	Focus       bool   `json:"focus"`
	Href        string `json:"href"`
	Frame       string `json:"frame"`
	CrossOrigin bool   `json:"crossOrigin"`
	Editable    bool   `json:"editable"`
}

var browserClasses = []string{"chromium", "chromium-browser", "google-chrome"}

// guardKeyboard checks, before secrets are typed on the keyboard, that the
// keyboard is in a page they may go into — not a terminal, not the address
// bar, not a frame from somewhere else.
func (s *Server) guardKeyboard(guards []api.Guard) error {
	if len(guards) == 0 {
		return nil
	}
	names := make([]string, len(guards))
	for i, g := range guards {
		names[i] = "{{" + g.Secret + "}}"
	}
	what := strings.Join(names, " and ")
	wins, err := s.disp.Windows()
	if err != nil {
		return fmt.Errorf("refused: cannot tell where %s would be typed: %v", what, err)
	}
	var active *api.Window
	for i := range wins {
		if wins[i].Active {
			active = &wins[i]
		}
	}
	if active == nil {
		return fmt.Errorf("refused: no window has the keyboard, so %s would go nowhere known; click into the field first", what)
	}
	isBrowser := false
	for _, c := range browserClasses {
		if strings.EqualFold(active.Class, c) {
			isBrowser = true
		}
	}
	if !isBrowser {
		return fmt.Errorf("refused: %s is only for %s, and the keyboard is in %q (%s), not a web page. Click into the field on the page first",
			what, strings.Join(guards[0].Sites, ", "), active.Title, active.Class)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	s.browser.mu.Lock()
	defer s.browser.mu.Unlock()
	pages, err := s.browser.targets()
	if err != nil {
		return fmt.Errorf("refused: cannot ask the browser where the keyboard is: %v", err)
	}
	for _, p := range pages {
		c, err := dialCDP(ctx, p)
		if err != nil {
			continue
		}
		var f keyboardFocus
		err = c.eval(ctx, focusScript, &f)
		c.close()
		if err != nil || !f.Focus {
			continue
		}
		if f.CrossOrigin {
			return fmt.Errorf("refused: the field with the keyboard is inside a frame from another site (%s), and %s cannot be checked there; open that frame's page on its own, or use browser_type", f.Frame, what)
		}
		if !f.Editable {
			return fmt.Errorf("refused: the keyboard is on %s but not in a text field; click into the field first", pageOrigin(f.Href))
		}
		return checkGuards(f.Href, guards)
	}
	return errors.New("refused: the keyboard is not in a web page (it may be in the address bar or a browser dialog); click into the field on the page first")
}
