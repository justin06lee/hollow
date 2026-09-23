package agent

import (
	"strings"
	"testing"

	"github.com/justin06lee/hollow/api"
)

func TestSiteAllows(t *testing.T) {
	sites := []string{"github.com", "http://intranet.local"}
	for url, want := range map[string]bool{
		"https://github.com/login":            true,
		"https://www.github.com/":             true,
		"https://gist.github.com/x":           true,
		"http://github.com/login":             false, // not over TLS
		"https://github.com.evil.io/login":    false,
		"https://evilgithub.com/":             false,
		"https://notgithub.com/":              false,
		"http://intranet.local/sso":           true,
		"https://intranet.local/sso":          true,
		"about:blank":                         false,
		"data:text/html,<form>":               false,
		"https://github.com@evil.io/":         false,
		"https://GitHub.COM./session?next=/x": true,
	} {
		if got := siteAllows(url, sites); got != want {
			t.Errorf("%s: got %v want %v", url, got, want)
		}
	}
}

func TestCheckGuards(t *testing.T) {
	g := []api.Guard{{Secret: "bank", Sites: []string{"bank.com"}}}
	if err := checkGuards("https://login.bank.com/", g); err != nil {
		t.Fatal(err)
	}
	err := checkGuards("https://bank-login.io/", g)
	if err == nil || !strings.Contains(err.Error(), "{{bank}} is only for bank.com") || !strings.Contains(err.Error(), "https://bank-login.io") {
		t.Fatalf("%v", err)
	}
}
