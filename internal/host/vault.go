package host

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/justin06lee/hollow/api"
)

// The vault is how an agent logs in without ever holding a password. The
// user stores credentials on the host once; an agent types {{github}} and the
// host puts the password in on the request's way to the desk. Nothing reads
// a value back out: there is no route for it, and everything coming back
// from a desk has the vault's values replaced by their placeholders.
//
// What it does not do is make a desk a safe place for a secret. Once typed,
// a password is in a page, and a program on the desk that goes looking can
// find it and encode it past the scrubbing. The vault keeps secrets out of
// prompts, transcripts and model context, and keeps a secret bound to its
// sites from being typed anywhere else — which is what stops a page that
// talks an agent into typing {{bank}} into its own form.
//
// On disk it is one AES-256-GCM sealed file with its key beside it, both
// readable by hollow alone. The key being on the same disk means this is
// protection for the file on its own — a backup, a copied directory — not
// against someone who has the machine.

var (
	secretNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	secretFieldRE = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	placeholderRE = regexp.MustCompile(`\\?\{\{\s*([A-Za-z0-9][A-Za-z0-9_-]*)(?:\.([A-Za-z0-9_]+))?\s*\}\}`)
)

// Fields with these names are the account, not a secret: shown, and typed
// from Username.
var usernameFields = map[string]bool{"username": true, "user": true, "login": true, "email": true}

// And these are the one-time code, worked out from the seed in "totp".
var codeFields = map[string]bool{"totp": true, "otp": true, "code": true, "2fa": true, "mfa": true}

// Values shorter than this are not scrubbed from what desks send back: a
// three-character "secret" would be found in every page.
const minScrub = 4

type secret struct {
	Username string            `json:"username,omitempty"`
	Fields   map[string]string `json:"fields"`
	Sites    []string          `json:"sites,omitempty"`
	Updated  time.Time         `json:"updated"`
}

// Vault is the host's credentials.
type Vault struct {
	path, keyPath string

	mu      sync.RWMutex
	secrets map[string]*secret
	scrub   *strings.Replacer // nil when there is nothing to scrub
}

// OpenVault loads the vault in dir, or starts an empty one there.
func OpenVault(dir string) (*Vault, error) {
	v := &Vault{
		path:    filepath.Join(dir, "vault"),
		keyPath: filepath.Join(dir, "vault.key"),
		secrets: map[string]*secret{},
	}
	sealed, err := os.ReadFile(v.path)
	if errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(v.keyPath)
	if err != nil {
		return nil, fmt.Errorf("the vault is there but its key is not (%v)", err)
	}
	plain, err := open(key, sealed)
	if err != nil {
		return nil, fmt.Errorf("the vault does not open with its key: %w", err)
	}
	if err := json.Unmarshal(plain, &v.secrets); err != nil {
		return nil, fmt.Errorf("the vault is damaged: %w", err)
	}
	v.rebuild()
	return v, nil
}

func (v *Vault) key() ([]byte, error) {
	if k, err := os.ReadFile(v.keyPath); err == nil && len(k) == 32 {
		return k, nil
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.WriteFile(v.keyPath, k, 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

// save writes the vault. Called with mu held.
func (v *Vault) save() error {
	key, err := v.key()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(v.secrets)
	if err != nil {
		return err
	}
	sealed, err := seal(key, plain)
	if err != nil {
		return err
	}
	tmp := v.path + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, v.path)
}

func seal(key, plain []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, []byte("hollow vault 1")), nil
}

func open(key, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("too short")
	}
	n := gcm.NonceSize()
	return gcm.Open(nil, sealed[:n], sealed[n:], []byte("hollow vault 1"))
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// rebuild remakes the scrubber from the vault's values. Called with mu held.
// Longest first, so that a value containing another is replaced whole.
func (v *Vault) rebuild() {
	type pair struct{ value, placeholder string }
	var pairs []pair
	for name, s := range v.secrets {
		for f, val := range s.Fields {
			if len(val) < minScrub {
				continue
			}
			ph := "{{" + name + "." + f + "}}"
			if f == "password" {
				ph = "{{" + name + "}}"
			}
			pairs = append(pairs, pair{val, ph})
		}
	}
	if len(pairs) == 0 {
		v.scrub = nil
		return
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].value) > len(pairs[j].value) })
	args := make([]string, 0, 2*len(pairs))
	for _, p := range pairs {
		args = append(args, p.value, p.placeholder)
	}
	v.scrub = strings.NewReplacer(args...)
}

// List is every secret, without its values, by name.
func (v *Vault) List() []api.Secret {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]api.Secret, 0, len(v.secrets))
	for name, s := range v.secrets {
		out = append(out, describe(name, s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func describe(name string, s *secret) api.Secret {
	fields := make([]string, 0, len(s.Fields))
	for f := range s.Fields {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	return api.Secret{Name: name, Username: s.Username, Fields: fields, Sites: s.Sites, Updated: s.Updated}
}

// Set stores a secret, as SecretSet describes.
func (v *Vault) Set(name string, in api.SecretSet) (api.Secret, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !secretNameRE.MatchString(name) {
		return api.Secret{}, fmt.Errorf("secret name %q: lowercase letters, digits, dash and underscore, up to 63", name)
	}
	sites, err := cleanSites(in.Sites)
	if err != nil {
		return api.Secret{}, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	s := &secret{Fields: map[string]string{}}
	if old, ok := v.secrets[name]; ok && in.Merge {
		s.Username, s.Sites = old.Username, old.Sites
		for f, val := range old.Fields {
			s.Fields[f] = val
		}
	}
	if in.Username != nil {
		s.Username = strings.TrimSpace(*in.Username)
	}
	for f, val := range in.Fields {
		f = strings.ToLower(strings.TrimSpace(f))
		switch {
		case !secretFieldRE.MatchString(f):
			return api.Secret{}, fmt.Errorf("field %q: lowercase letters, digits and underscore", f)
		case usernameFields[f]:
			return api.Secret{}, fmt.Errorf("the account goes in username, not a field called %q", f)
		case codeFields[f] && f != "totp":
			return api.Secret{}, fmt.Errorf("a one-time code's seed goes in the field totp, not %q", f)
		}
		if val == "" {
			delete(s.Fields, f)
			continue
		}
		if f == "totp" {
			if _, err := totpCode(val, time.Now()); err != nil {
				return api.Secret{}, fmt.Errorf("totp: %w", err)
			}
		}
		s.Fields[f] = val
	}
	if len(sites) > 0 {
		s.Sites = sites
	} else if in.Anywhere || !in.Merge {
		s.Sites = nil
	}
	if len(s.Fields) == 0 && s.Username == "" {
		return api.Secret{}, errors.New("a secret needs at least a field or a username")
	}
	s.Updated = time.Now().UTC()
	prev, had := v.secrets[name]
	v.secrets[name] = s
	if err := v.save(); err != nil {
		if had {
			v.secrets[name] = prev
		} else {
			delete(v.secrets, name)
		}
		return api.Secret{}, err
	}
	v.rebuild()
	return describe(name, s), nil
}

// Delete removes a secret.
func (v *Vault) Delete(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.secrets[name]
	if !ok {
		return fmt.Errorf("no secret %q", name)
	}
	delete(v.secrets, name)
	if err := v.save(); err != nil {
		v.secrets[name] = s
		return err
	}
	v.rebuild()
	return nil
}

// cleanSites turns what a person types — a URL, a host, *.host — into the
// hosts guards compare against. An explicit http:// is kept, as the one way
// to allow a secret onto a page without TLS.
func cleanSites(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		s := strings.ToLower(strings.TrimSpace(raw))
		if s == "" {
			continue
		}
		plain := strings.HasPrefix(s, "http://")
		if strings.Contains(s, "://") {
			u, err := url.Parse(s)
			if err != nil || u.Hostname() == "" {
				return nil, fmt.Errorf("site %q is not a host or URL", raw)
			}
			s = u.Hostname()
		} else {
			if i := strings.IndexAny(s, "/?#"); i >= 0 {
				s = s[:i]
			}
			if h, _, ok := strings.Cut(s, ":"); ok {
				s = h
			}
		}
		s = strings.TrimPrefix(s, "*.")
		s = strings.TrimPrefix(s, "www.")
		s = strings.TrimSuffix(s, ".")
		if s == "" || strings.ContainsAny(s, " *") {
			return nil, fmt.Errorf("site %q is not a host or URL", raw)
		}
		if plain {
			s = "http://" + s
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

// Fill mode: where the text is going.
const (
	fillTyped = iota // into a page or a window, where a guard can check
	fillExec         // into a command, where nothing can
)

// Fill replaces the placeholders in text with their values. It returns the
// guards for every site-bound secret it filled in, for the guest to check
// before it types. A secret bound to sites cannot go into a command at all.
func (v *Vault) Fill(text string, mode int) (string, []api.Guard, error) {
	if !strings.Contains(text, "{{") {
		return text, nil, nil
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	var guards []api.Guard
	guarded := map[string]bool{}
	var firstErr error
	out := placeholderRE.ReplaceAllStringFunc(text, func(m string) string {
		if strings.HasPrefix(m, `\`) {
			return m[1:]
		}
		if firstErr != nil {
			return m
		}
		sub := placeholderRE.FindStringSubmatch(m)
		name, field := strings.ToLower(sub[1]), strings.ToLower(sub[2])
		s, ok := v.secrets[name]
		if !ok {
			firstErr = v.unknown(name)
			return m
		}
		if len(s.Sites) > 0 {
			if mode == fillExec {
				firstErr = fmt.Errorf("{{%s}} is only for %s, and a command is not a page on it — to let it into commands, store it for anywhere (bangboo secret set %s --merge --anywhere)",
					name, strings.Join(s.Sites, ", "), name)
				return m
			}
			if !guarded[name] {
				guarded[name] = true
				guards = append(guards, api.Guard{Secret: name, Sites: s.Sites})
			}
		}
		val, err := s.value(name, field)
		if err != nil {
			firstErr = err
			return m
		}
		return val
	})
	if firstErr != nil {
		return "", nil, firstErr
	}
	return out, guards, nil
}

// unknown explains a placeholder that names no secret. Called with mu held.
func (v *Vault) unknown(name string) error {
	if len(v.secrets) == 0 {
		return fmt.Errorf("no secret %q: this host's vault is empty. The user stores secrets with `bangboo secret set NAME`. To type the braces themselves, write \\{{%s}}", name, name)
	}
	names := make([]string, 0, len(v.secrets))
	for n := range v.secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Errorf("no secret %q; there are: %s. To type the braces themselves, write \\{{%s}}", name, strings.Join(names, ", "), name)
}

func (s *secret) value(name, field string) (string, error) {
	switch {
	case field == "":
		if val, ok := s.Fields["password"]; ok {
			return val, nil
		}
		var hidden []string
		for f := range s.Fields {
			if f != "totp" {
				hidden = append(hidden, f)
			}
		}
		if len(hidden) == 1 {
			return s.Fields[hidden[0]], nil
		}
		sort.Strings(hidden)
		return "", fmt.Errorf("{{%s}} has no password; say which field: %s", name, fieldList(name, hidden))
	case usernameFields[field]:
		if s.Username == "" {
			return "", fmt.Errorf("{{%s}} has no username stored", name)
		}
		return s.Username, nil
	case codeFields[field]:
		seed, ok := s.Fields["totp"]
		if !ok {
			return "", fmt.Errorf("{{%s}} has no TOTP seed, so there is no code to type; ask the user for it", name)
		}
		return totpCode(seed, time.Now())
	}
	val, ok := s.Fields[field]
	if !ok {
		var have []string
		for f := range s.Fields {
			have = append(have, f)
		}
		sort.Strings(have)
		return "", fmt.Errorf("{{%s}} has no field %q; it has: %s", name, field, fieldList(name, have))
	}
	return val, nil
}

func fieldList(name string, fields []string) string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = "{{" + name + "." + f + "}}"
	}
	return strings.Join(out, ", ")
}

// Scrub replaces every secret value in s with its placeholder.
func (v *Vault) Scrub(s string) string {
	v.mu.RLock()
	r := v.scrub
	v.mu.RUnlock()
	if r == nil {
		return s
	}
	return r.Replace(s)
}

// Active reports whether there is anything to scrub.
func (v *Vault) Active() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.scrub != nil
}

// ScrubJSON scrubs every string in a JSON document — keys, values, and
// strings inside strings — by decoding it rather than searching its bytes,
// since JSON escaping means a value is not always spelled the same way.
// A body that is not JSON is scrubbed as text.
func (v *Vault) ScrubJSON(data []byte) []byte {
	if !v.Active() {
		return data
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return []byte(v.Scrub(string(data)))
	}
	doc = v.walk(doc)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return []byte(v.Scrub(string(data)))
	}
	return buf.Bytes()
}

func (v *Vault) walk(x any) any {
	switch t := x.(type) {
	case string:
		return v.Scrub(t)
	case []any:
		for i := range t {
			t[i] = v.walk(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[v.Scrub(k)] = v.walk(val)
		}
		return out
	}
	return x
}

// totpCode is RFC 6238: the one-time code for seed at t. seed is the base32
// secret an authenticator app is given, or the whole otpauth:// URI.
func totpCode(seed string, t time.Time) (string, error) {
	secret, digits, period, algo := seed, 6, 30, "SHA1"
	if strings.HasPrefix(strings.ToLower(seed), "otpauth://") {
		u, err := url.Parse(seed)
		if err != nil {
			return "", fmt.Errorf("not an otpauth URI: %w", err)
		}
		q := u.Query()
		secret = q.Get("secret")
		if d, err := strconv.Atoi(q.Get("digits")); err == nil {
			digits = d
		}
		if p, err := strconv.Atoi(q.Get("period")); err == nil {
			period = p
		}
		if a := q.Get("algorithm"); a != "" {
			algo = strings.ToUpper(a)
		}
	}
	secret = strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(secret)))
	secret = strings.TrimRight(secret, "=")
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil || len(key) == 0 {
		return "", errors.New("the seed is not base32 (the text an authenticator app is given, or an otpauth:// URI)")
	}
	if digits < 6 || digits > 8 || period < 1 {
		return "", fmt.Errorf("%d digits every %ds is not a TOTP", digits, period)
	}
	var h func() hash.Hash
	switch algo {
	case "SHA1":
		h = sha1.New
	case "SHA256":
		h = sha256.New
	case "SHA512":
		h = sha512.New
	default:
		return "", fmt.Errorf("unknown TOTP algorithm %s", algo)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(t.Unix()/int64(period)))
	mac := hmac.New(h, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, code%mod), nil
}
