// Package secrets is the command a person uses to fill a host's vault:
// set, ls, rm and import. It is shared by the hollow CLI, which talks to one
// host, and bangboo, which talks to every host it knows, so the two work
// alike.
//
// Values are asked for without echo, or read from stdin for a password
// manager's CLI to pipe in. None is ever printed, and none can be read back.
package secrets

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/justin06lee/hollow/api"
	"github.com/justin06lee/hollow/client"
	"golang.org/x/term"
)

// Host is one vault to work on.
type Host struct {
	Name   string
	Client *client.Client
}

// Env is what the calling program provides.
type Env struct {
	Prog string // "bangboo" or "hollow", for usage text

	// Hosts resolves --host: "" means the default (every host, for bangboo).
	Hosts func(ctx context.Context, name string) ([]Host, error)

	// HostFlag says whether there is a --host flag to offer.
	HostFlag bool
}

// Usage is the secret command's help.
func Usage(prog string) string {
	return fmt.Sprintf(`usage: %[1]s secret <command>

Secrets live in a host's vault. An agent types {{name}} and the host fills in
the password on its way to the desk; nothing can read a value back out, and
secret values are replaced by their placeholders in everything a desk sends
back. {{name.username}}, {{name.totp}} (the current code) and {{name.FIELD}}
work too.

  set NAME      store a secret; asks for the password without echo
                  --username U   the account (not secret; shown to agents)
                  --site HOST    only type it into pages on HOST (repeatable)
                  --field F      ask for field F instead of a password (repeatable)
                  --totp         also ask for a TOTP seed, for {{NAME.totp}}
                  --stdin        read the values from stdin, one per line
                  --merge        change only what is given, keep the rest
                  --anywhere     with --merge: drop the sites
  ls            list secrets: names, accounts, fields and sites, never values
  rm NAME       delete a secret
  import FILE   import a CSV export from Chrome, Firefox, Safari, Bitwarden or
                1Password; each login is bound to its site
                  --match TEXT   only logins whose name or site contains TEXT
                  --dry-run      show what would be stored, store nothing

A secret with no --site can be typed anywhere, and used in shell commands.
Give web logins their site: it is what keeps a page from talking an agent
into typing your password into the wrong one.
`, prog)
}

// Main runs "secret ARGS".
func Main(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(os.Stderr, Usage(env.Prog))
		if len(args) == 0 {
			return flag.ErrHelp
		}
		return nil
	}
	switch args[0] {
	case "set", "add", "put":
		return cmdSet(ctx, env, args[1:])
	case "ls", "list":
		return cmdList(ctx, env, args[1:])
	case "rm", "remove", "delete":
		return cmdRemove(ctx, env, args[1:])
	case "import":
		return cmdImport(ctx, env, args[1:])
	}
	return fmt.Errorf("unknown secret command %q\n\n%s", args[0], Usage(env.Prog))
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func flags(env Env, name string, host *string) *flag.FlagSet {
	fs := flag.NewFlagSet("secret "+name, flag.ContinueOnError)
	if env.HostFlag {
		fs.StringVar(host, "host", "", "only this host (default: every host)")
	}
	fs.Usage = func() { fmt.Fprint(os.Stderr, Usage(env.Prog)) }
	return fs
}

// parse reads flags wherever they are among the arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func cmdSet(ctx context.Context, env Env, args []string) error {
	var host, username string
	var sites, fields multi
	fs := flags(env, "set", &host)
	fs.StringVar(&username, "username", "", "the account")
	fs.StringVar(&username, "user", "", "the account")
	fs.Var(&sites, "site", "only type it into pages on this host (repeatable)")
	fs.Var(&fields, "field", "ask for this field instead of a password (repeatable)")
	totp := fs.Bool("totp", false, "also ask for a TOTP seed")
	stdin := fs.Bool("stdin", false, "read values from stdin, one per line")
	merge := fs.Bool("merge", false, "change only what is given")
	anywhere := fs.Bool("anywhere", false, "with --merge: drop the sites")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: %s secret set NAME [--username U] [--site HOST]... [--field F]... [--totp]", env.Prog)
	}
	name := pos[0]
	userSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "username" || f.Name == "user" {
			userSet = true
		}
	})

	var ask []string
	if len(fields) > 0 {
		ask = append(ask, fields...)
	} else if !*merge || (!*totp && !userSet && len(sites) == 0 && !*anywhere) {
		ask = append(ask, "password")
	}
	if *totp {
		ask = append(ask, "totp")
	}
	in := api.SecretSet{Fields: map[string]string{}, Sites: sites, Merge: *merge, Anywhere: *anywhere}
	if userSet {
		in.Username = &username
	}
	var lines *bufio.Scanner
	if *stdin {
		lines = bufio.NewScanner(os.Stdin)
		lines.Buffer(make([]byte, 64<<10), 1<<20)
	}
	for _, f := range ask {
		var val string
		if lines != nil {
			if !lines.Scan() {
				return fmt.Errorf("stdin ended before the value for %s", f)
			}
			val = strings.TrimRight(lines.Text(), "\r")
		} else {
			prompt := f + " for " + name
			if f == "totp" {
				prompt = "TOTP seed for " + name + " (the text or otpauth:// link an authenticator app is given)"
			}
			if val, err = readHidden(prompt); err != nil {
				return err
			}
		}
		if val == "" && !*merge {
			return fmt.Errorf("no value given for %s", f)
		}
		if val != "" {
			in.Fields[f] = val
		}
	}
	hosts, err := env.Hosts(ctx, host)
	if err != nil {
		return err
	}
	var failed []string
	for _, h := range hosts {
		s, err := h.Client.SetSecret(ctx, name, in)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", h.Name, err)
			failed = append(failed, h.Name)
			continue
		}
		fmt.Printf("%s: stored %s\n", h.Name, describe(s))
	}
	if len(sites) == 0 && !*merge {
		fmt.Fprintf(os.Stderr, "note: with no --site, {{%s}} can be typed into any page and used in shell commands\n", strings.ToLower(name))
	}
	if len(failed) > 0 {
		return fmt.Errorf("not stored on %s", strings.Join(failed, ", "))
	}
	return nil
}

func describe(s api.Secret) string {
	var b strings.Builder
	b.WriteString("{{" + s.Name + "}}")
	if s.Username != "" {
		b.WriteString(" for " + s.Username)
	}
	b.WriteString(" (" + strings.Join(s.Fields, ", ") + ")")
	if len(s.Sites) > 0 {
		b.WriteString(" on " + strings.Join(s.Sites, ", "))
	} else {
		b.WriteString(" anywhere")
	}
	return b.String()
}

// readHidden asks for a value on the terminal without echoing it.
func readHidden(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("no terminal to ask on; pipe the value in with --stdin")
	}
	defer tty.Close()
	fmt.Fprintf(tty, "%s: ", prompt)
	b, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func cmdList(ctx context.Context, env Env, args []string) error {
	var host string
	fs := flags(env, "ls", &host)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	hosts, err := env.Hosts(ctx, host)
	if err != nil {
		return err
	}
	type row struct {
		Host   string       `json:"host"`
		Secret []api.Secret `json:"secrets"`
		Error  string       `json:"error,omitempty"`
	}
	var rows []row
	for _, h := range hosts {
		list, err := h.Client.Secrets(ctx)
		r := row{Host: h.Name, Secret: list}
		if err != nil {
			r.Error = err.Error()
		}
		rows = append(rows, r)
	}
	if *asJSON {
		return writeJSON(os.Stdout, rows)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tNAME\tUSERNAME\tFIELDS\tSITES")
	for _, r := range rows {
		if r.Error != "" {
			fmt.Fprintf(tw, "%s\t(%s)\t\t\t\n", r.Host, r.Error)
			continue
		}
		if len(r.Secret) == 0 {
			fmt.Fprintf(tw, "%s\t(empty)\t\t\t\n", r.Host)
		}
		for _, s := range r.Secret {
			sites := strings.Join(s.Sites, ", ")
			if sites == "" {
				sites = "anywhere"
			}
			user := s.Username
			if user == "" {
				user = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Host, s.Name, user, strings.Join(s.Fields, ","), sites)
		}
	}
	return tw.Flush()
}

func cmdRemove(ctx context.Context, env Env, args []string) error {
	var host string
	fs := flags(env, "rm", &host)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return fmt.Errorf("usage: %s secret rm NAME...", env.Prog)
	}
	hosts, err := env.Hosts(ctx, host)
	if err != nil {
		return err
	}
	bad := 0
	for _, name := range pos {
		gone := 0
		for _, h := range hosts {
			err := h.Client.DeleteSecret(ctx, name)
			switch {
			case err == nil:
				gone++
				fmt.Printf("%s: removed %s\n", h.Name, name)
			case client.IsStatus(err, 404):
			default:
				fmt.Fprintf(os.Stderr, "%s: %v\n", h.Name, err)
				bad++
			}
		}
		if gone == 0 {
			fmt.Fprintf(os.Stderr, "no secret %q on any host\n", name)
			bad++
		}
	}
	if bad > 0 {
		return errors.New("not everything was removed")
	}
	return nil
}

// Login is one row of a password manager's export.
type Login struct {
	Name     string
	URL      string
	Username string
	Password string
	TOTP     string
}

// Columns each exporter uses, lowercased. First match wins.
var columns = map[string][]string{
	"name":     {"name", "title"},
	"url":      {"url", "login_uri", "website", "uri", "urls"},
	"username": {"username", "login_username", "user", "login", "email"},
	"password": {"password", "login_password"},
	"totp":     {"totp", "login_totp", "otpauth", "otp", "one-time password"},
	"type":     {"type"},
}

// ParseCSV reads a password manager's CSV export into logins. Rows without
// a password, and entries that are not logins, are skipped.
func ParseCSV(r io.Reader) ([]Login, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	head, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("not a CSV export: %w", err)
	}
	idx := map[string]int{}
	for want, names := range columns {
		idx[want] = -1
		for _, n := range names {
			for i, h := range head {
				if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")), n) && idx[want] < 0 {
					idx[want] = i
				}
			}
		}
	}
	if idx["password"] < 0 {
		return nil, errors.New("the CSV has no password column; export logins as CSV from the password manager")
	}
	get := func(rec []string, k string) string {
		if i := idx[k]; i >= 0 && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var out []Login
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if t := strings.ToLower(get(rec, "type")); t != "" && t != "login" {
			continue
		}
		l := Login{Name: get(rec, "name"), URL: get(rec, "url"), Username: get(rec, "username"), Password: get(rec, "password"), TOTP: get(rec, "totp")}
		if l.Password == "" {
			continue
		}
		// Some exporters put several URLs in one cell.
		if i := strings.IndexAny(l.URL, ",\n "); i > 0 {
			l.URL = l.URL[:i]
		}
		out = append(out, l)
	}
	return out, nil
}

// Site is the host a login belongs on, from its URL, or "" when it has no
// web address (an app, a Wi-Fi network).
func (l Login) Site() string {
	u := l.URL
	if u == "" {
		return ""
	}
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") {
		return ""
	}
	h := strings.TrimPrefix(strings.ToLower(p.Hostname()), "www.")
	if !strings.Contains(h, ".") && h != "localhost" {
		return ""
	}
	return h
}

var slugRE = regexp.MustCompile(`[^a-z0-9_-]+`)

// slug makes a secret name from a login's name or site: "GitHub" and
// "github.com" both make github, accounts.google.com makes google.
func (l Login) slug() string {
	base := strings.ToLower(strings.TrimSpace(l.Name))
	if base == "" || (strings.Contains(base, ".") && !strings.Contains(base, " ")) {
		host := l.Site()
		if base != "" {
			host = Login{URL: base}.Site()
		}
		parts := strings.Split(host, ".")
		if len(parts) >= 2 {
			base = parts[len(parts)-2]
		} else if host != "" {
			base = host
		}
	}
	return clean(base, "login")
}

func clean(s, fallback string) string {
	s = strings.Trim(slugRE.ReplaceAllString(strings.ToLower(s), "-"), "-_")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-_")
	}
	if s == "" {
		return fallback
	}
	return s
}

// Names picks unique secret names for logins, in order. Two logins that
// would share a name are told apart by their accounts: github-octo,
// github-work.
func Names(logins []Login) []string {
	count := map[string]int{}
	for _, l := range logins {
		count[l.slug()]++
	}
	used := map[string]int{}
	out := make([]string, len(logins))
	for i, l := range logins {
		s := l.slug()
		if count[s] > 1 && l.Username != "" {
			user, _, _ := strings.Cut(l.Username, "@")
			s += "-" + clean(user, "account")
		}
		used[s]++
		if n := used[s]; n > 1 {
			s = fmt.Sprintf("%s-%d", s, n)
		}
		out[i] = s
	}
	return out
}

func cmdImport(ctx context.Context, env Env, args []string) error {
	var host string
	fs := flags(env, "import", &host)
	match := fs.String("match", "", "only logins whose name or site contains this")
	dry := fs.Bool("dry-run", false, "show what would be stored")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: %s secret import FILE.csv [--match TEXT] [--dry-run]", env.Prog)
	}
	f, err := os.Open(pos[0])
	if err != nil {
		return err
	}
	logins, err := ParseCSV(f)
	f.Close()
	if err != nil {
		return err
	}
	if m := strings.ToLower(*match); m != "" {
		kept := logins[:0]
		for _, l := range logins {
			if strings.Contains(strings.ToLower(l.Name), m) || strings.Contains(l.Site(), m) {
				kept = append(kept, l)
			}
		}
		logins = kept
	}
	if len(logins) == 0 {
		return errors.New("no logins with passwords to import")
	}
	names := Names(logins)
	order := make([]int, len(logins))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return names[order[a]] < names[order[b]] })

	var hosts []Host
	if !*dry {
		if hosts, err = env.Hosts(ctx, host); err != nil {
			return err
		}
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tUSERNAME\tSITE\tTOTP")
	anywhere, failed := 0, 0
	for _, i := range order {
		l := logins[i]
		site := l.Site()
		in := api.SecretSet{Username: &l.Username, Fields: map[string]string{"password": l.Password}}
		if site != "" {
			in.Sites = []string{site}
		} else {
			site = "anywhere"
			anywhere++
		}
		totp := ""
		if l.TOTP != "" {
			in.Fields["totp"] = l.TOTP
			totp = "yes"
		}
		status := ""
		for _, h := range hosts {
			if _, err := h.Client.SetSecret(ctx, names[i], in); err != nil {
				status = fmt.Sprintf("\t(%s: %v)", h.Name, err)
				failed++
			}
		}
		user := l.Username
		if user == "" {
			user = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s%s\n", names[i], user, site, totp, status)
	}
	tw.Flush()
	switch {
	case *dry:
		fmt.Printf("\n%d logins would be stored (dry run: nothing was)\n", len(logins))
	case failed > 0:
		return fmt.Errorf("%d secrets were not stored", failed)
	default:
		var hn []string
		for _, h := range hosts {
			hn = append(hn, h.Name)
		}
		fmt.Printf("\n%d logins stored on %s. The export file holds every password in plain text: delete it.\n", len(logins), strings.Join(hn, ", "))
	}
	if anywhere > 0 {
		fmt.Printf("%d had no web address, so can be typed anywhere; give them a site with: %s secret set NAME --merge --site HOST\n", anywhere, env.Prog)
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
