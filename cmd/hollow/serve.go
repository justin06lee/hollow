package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/justin06lee/hollow/api"
	"github.com/justin06lee/hollow/client"
	"github.com/justin06lee/hollow/internal/backend/qemu"
	"github.com/justin06lee/hollow/internal/host"
	"github.com/justin06lee/hollow/internal/image"
)

func defaultPort() int {
	if v, err := strconv.Atoi(os.Getenv("HOLLOW_PORT")); err == nil && v > 0 {
		return v
	}
	return api.DefaultPort
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", os.Getenv("HOLLOW_ADDR"), "listen only here: host:port, comma-separated (default: loopback and every mesh address)")
	port := fs.Int("port", defaultPort(), "port for the default addresses")
	state := fs.String("state", host.DefaultDir(), "where images, desks and the token live")
	advertise := fs.String("advertise", os.Getenv("HOLLOW_ADVERTISE"), "a URL to put first in connect codes, when clients reach this host by a name")
	idle := fs.Duration("idle", envDuration("HOLLOW_IDLE", 0), "stop desks nobody has used for this long (0: never)")
	quiet := fs.Bool("quiet", false, "print nothing but errors")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: hollow serve [flags]\n\nRuns the host. It listens on loopback and on every overlay-network address\nthis machine has — makima, Tailscale, WireGuard — and nowhere else, and\nnotices when those come and go. --addr replaces that with exactly what you say.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir := *state
	token, err := host.Token(dir)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	b := qemu.New()
	backendErr := b.Check()
	images := image.New(filepath.Join(dir, "images"), b)

	fixed := splitList(*addr)
	auto := len(fixed) == 0
	p := *port
	if !auto {
		_, ps, err := net.SplitHostPort(fixed[0])
		if err != nil {
			return fmt.Errorf("--addr %s: %w", fixed[0], err)
		}
		if p, err = strconv.Atoi(ps); err != nil {
			return fmt.Errorf("--addr %s: bad port", fixed[0])
		}
		// Guests reach this machine at its loopback, through the
		// hypervisor's user networking, and fetch their agent there. A host
		// told to listen somewhere else still has to answer on loopback.
		hasLoop := false
		for _, a := range fixed {
			h, _, _ := net.SplitHostPort(a)
			if ip := net.ParseIP(h); h == "" || (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
				hasLoop = true
			}
		}
		if !hasLoop {
			fixed = append(fixed, fmt.Sprintf("127.0.0.1:%d", p))
		}
	}

	desks, err := host.NewDesks(filepath.Join(dir, "desks"), p, b, images)
	if err != nil {
		return err
	}
	urls := func() []string {
		out := host.URLs(p, fixed)
		if *advertise != "" {
			out = append([]string{strings.TrimRight(*advertise, "/")}, out...)
		}
		return out
	}
	srv := host.NewServer(version, token, b, images, desks, urls)
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 15 * time.Second}
	ls, err := host.Listen(httpSrv, fixed, p, auto)
	if err != nil {
		return err
	}
	defer ls.Close()

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go desks.Reap(rctx, *idle)

	if !*quiet {
		fmt.Printf("\nhollow %s — %s\n\n", version, host.Hostname())
		fmt.Printf("  listening  %s\n", strings.Join(ls.Bound(), "  "))
		fmt.Printf("  state      %s\n", dir)
		if backendErr != nil {
			fmt.Printf("  backend    %s — unavailable: %v\n", b.Name(), backendErr)
			fmt.Printf("             the API is up, but no desk can start here\n")
		} else {
			fmt.Printf("  backend    %s with kvm\n", b.Name())
		}
		for _, img := range images.List() {
			fmt.Printf("  image      %-7s %s\n", img.OS, imageLine(img))
		}
		if *idle > 0 {
			fmt.Printf("  idle       desks unused for %s are stopped\n", *idle)
		}
		fmt.Printf("  connect    %s\n\n", client.Connect{URLs: urls(), Token: token, Name: host.Hostname()}.Encode())
	}

	select {
	case err := <-ls.Err():
		desks.Close()
		return err
	case <-ctx.Done():
	}
	if !*quiet {
		fmt.Println("hollow: stopping desks")
	}
	desks.Close()
	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	return httpSrv.Shutdown(shutdownCtx)
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}

func imageLine(img api.Image) string {
	switch img.State {
	case api.ImageReady:
		s := fmt.Sprintf("ready (%d MB)", img.Size>>20)
		if img.Outdated {
			s += " — outdated recipe; pull again to update"
		}
		return s
	case api.ImageMissing:
		return "missing — run: hollow pull " + img.OS
	case api.ImageFailed:
		return "failed: " + img.Error + " — run: hollow pull " + img.OS
	default:
		return img.State + ": " + img.Progress
	}
}

// connectInfo is what `hollow connect --json` prints.
type connectInfo struct {
	Code    string   `json:"code"`
	Name    string   `json:"name"`
	URLs    []string `json:"urls"`
	Version string   `json:"version,omitempty"`
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdConnect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	var extra multiFlag
	fs.Var(&extra, "url", "an address clients will reach this host at, put first (repeatable)")
	name := fs.String("name", "", "what clients should call this host (default: its hostname)")
	state := fs.String("state", host.DefaultDir(), "the host's state directory")
	asJSON := fs.Bool("json", false, "print JSON: the code, name, and addresses")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: hollow connect [--url URL]... [--json]\n\nPrints a connect code for the host on this machine: its name, every address it\nanswers at, and its token, in one string for bangboo host add or HOLLOW_CONNECT.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	token, err := host.ReadToken(*state)
	if err != nil {
		return fmt.Errorf("no host state in %s (%v); has `hollow serve` run here?", *state, err)
	}
	info := connectInfo{Name: *name}
	// The running host knows where it listens; ask it. Without one, work it
	// out the same way it would.
	var urls []string
	c := client.New(fmt.Sprintf("http://127.0.0.1:%d", defaultPort()), token)
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	if st, err := c.Status(sctx); err == nil {
		urls = st.URLs
		info.Version = st.Version
		if info.Name == "" {
			info.Name = st.Name
		}
	} else {
		urls = host.URLs(defaultPort(), nil)
	}
	cancel()
	if info.Name == "" {
		info.Name = host.Hostname()
	}
	seen := map[string]bool{}
	for _, u := range append(append([]string{}, extra...), urls...) {
		u = strings.TrimRight(u, "/")
		if !strings.Contains(u, "://") {
			u = "http://" + u
		}
		if !seen[u] {
			seen[u] = true
			info.URLs = append(info.URLs, u)
		}
	}
	info.Code = client.Connect{URLs: info.URLs, Token: token, Name: info.Name}.Encode()
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}
	fmt.Println(info.Code)
	return nil
}

var errNotLinux = errors.New("hollow service manages a systemd service, and this machine is not Linux")
