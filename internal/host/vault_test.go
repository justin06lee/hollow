package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justin06lee/hollow/api"
)

func strp(s string) *string { return &s }

func testVault(t *testing.T) (*Vault, string) {
	t.Helper()
	dir := t.TempDir()
	v, err := OpenVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Set("GitHub", api.SecretSet{
		Username: strp("octo@example.com"),
		Fields:   map[string]string{"password": `p"a<ss>w\ord`, "totp": "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"},
		Sites:    []string{"https://www.github.com/login", "*.githubusercontent.com"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Set("aws", api.SecretSet{Fields: map[string]string{"token": "AKIAEXAMPLEKEY"}}); err != nil {
		t.Fatal(err)
	}
	return v, dir
}

func TestVaultPersistsSealed(t *testing.T) {
	v, dir := testVault(t)
	raw, err := os.ReadFile(filepath.Join(dir, "vault"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "AKIAEXAMPLEKEY") || strings.Contains(string(raw), "octo@") {
		t.Fatal("vault file holds plaintext")
	}
	st, _ := os.Stat(filepath.Join(dir, "vault.key"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode().Perm())
	}
	again, err := OpenVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(again.List()), len(v.List()); got != want {
		t.Fatalf("reopened vault has %d secrets, want %d", got, want)
	}
	gh := again.List()[1]
	if gh.Name != "github" || gh.Username != "octo@example.com" || strings.Join(gh.Sites, ",") != "github.com,githubusercontent.com" {
		t.Fatalf("listed %+v", gh)
	}
}

func TestFill(t *testing.T) {
	v, _ := testVault(t)
	got, guards, err := v.Fill("{{github.username}} / {{ github }} / \\{{github}}", fillTyped)
	if err != nil {
		t.Fatal(err)
	}
	if want := `octo@example.com / p"a<ss>w\ord / {{github}}`; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if len(guards) != 1 || guards[0].Secret != "github" {
		t.Fatalf("guards %+v", guards)
	}
	if got, guards, err = v.Fill("{{aws}}", fillExec); err != nil || got != "AKIAEXAMPLEKEY" || guards != nil {
		t.Fatalf("aws: %q %v %v", got, guards, err)
	}
	if _, _, err := v.Fill("echo {{github}}", fillExec); err == nil || !strings.Contains(err.Error(), "only for github.com") {
		t.Fatalf("a site-bound secret went into a command: %v", err)
	}
	if _, _, err := v.Fill("{{nope}}", fillTyped); err == nil || !strings.Contains(err.Error(), "there are: aws, github") {
		t.Fatalf("unknown: %v", err)
	}
	if _, _, err := v.Fill("{{github.pin}}", fillTyped); err == nil || !strings.Contains(err.Error(), "{{github.password}}") {
		t.Fatalf("unknown field: %v", err)
	}
	code, _, err := v.Fill("{{github.totp}}", fillTyped)
	if err != nil || len(code) != 6 {
		t.Fatalf("totp: %q %v", code, err)
	}
	if plain, _, _ := v.Fill("no braces here", fillTyped); plain != "no braces here" {
		t.Fatal(plain)
	}
}

func TestScrub(t *testing.T) {
	v, _ := testVault(t)
	if got := v.Scrub("key=AKIAEXAMPLEKEY user=octo@example.com"); got != "key={{aws.token}} user=octo@example.com" {
		t.Fatal(got)
	}
	// JSON escapes the quote, the backslash and (by default) the angle
	// brackets; the value must be found anyway.
	doc, _ := json.Marshal(map[string]any{"text": `pw: p"a<ss>w\ord`, "n": json.Number("12345678901234567890")})
	out := string(v.ScrubJSON(doc))
	if strings.Contains(out, "ss>w") || strings.Contains(out, `ss>w`) || !strings.Contains(out, "{{github}}") {
		t.Fatalf("scrubbed JSON: %s", out)
	}
	if !strings.Contains(out, "12345678901234567890") {
		t.Fatalf("numbers were changed: %s", out)
	}
	if err := v.Delete("aws"); err != nil {
		t.Fatal(err)
	}
	if got := v.Scrub("AKIAEXAMPLEKEY"); got != "AKIAEXAMPLEKEY" {
		t.Fatal("deleted secret still scrubbed")
	}
}

func TestMerge(t *testing.T) {
	v, _ := testVault(t)
	if _, err := v.Set("github", api.SecretSet{Merge: true, Fields: map[string]string{"totp": ""}}); err != nil {
		t.Fatal(err)
	}
	s := v.List()[1]
	if strings.Join(s.Fields, ",") != "password" || s.Username == "" || len(s.Sites) != 2 {
		t.Fatalf("merge lost things: %+v", s)
	}
	if _, err := v.Set("github", api.SecretSet{Merge: true, Anywhere: true}); err != nil {
		t.Fatal(err)
	}
	if s := v.List()[1]; len(s.Sites) != 0 {
		t.Fatalf("anywhere kept sites: %+v", s)
	}
	if _, err := v.Set("bad name", api.SecretSet{Fields: map[string]string{"password": "x"}}); err == nil {
		t.Fatal("bad name accepted")
	}
	if _, err := v.Set("x", api.SecretSet{Fields: map[string]string{"totp": "not base32!"}}); err == nil {
		t.Fatal("bad seed accepted")
	}
}

// RFC 6238, appendix B: the SHA1 seed "12345678901234567890", 8 digits.
func TestTOTPVectors(t *testing.T) {
	seed := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	for _, c := range []struct {
		at   int64
		want string
	}{{59, "94287082"}, {1111111109, "07081804"}, {1234567890, "89005924"}, {2000000000, "69279037"}} {
		got, err := totpCode("otpauth://totp/x?secret="+seed+"&digits=8", time.Unix(c.at, 0))
		if err != nil || got != c.want {
			t.Fatalf("at %d: got %s want %s (%v)", c.at, got, c.want, err)
		}
	}
	got, err := totpCode("gezd gnbv gy3t qojq gezd gnbv gy3t qojq", time.Unix(59, 0))
	if err != nil || got != "287082" {
		t.Fatalf("6 digits: %s %v", got, err)
	}
}

func TestCleanSites(t *testing.T) {
	got, err := cleanSites([]string{"https://www.Example.com:443/login?x", "*.foo.org", "http://intranet.local/", "bar.io/path", "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "example.com,foo.org,http://intranet.local,bar.io"; strings.Join(got, ",") != want {
		t.Fatalf("got %v", got)
	}
}
