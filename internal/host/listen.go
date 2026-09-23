package host

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A host listens on loopback, and on every address this machine has on an
// overlay network — makima, Tailscale, a plain WireGuard tunnel — and on
// nothing else. Those are exactly the interfaces that are point-to-point:
// a tunnel has a peer on the other end, not a broadcast domain. The LAN and
// the internet never see the port.
//
// Overlay addresses come and go (a mesh daemon starts after this one, a
// laptop reconnects), so the set is looked at again every few seconds.
//
// makima publishes loopback listeners on its mesh address by itself. When it
// got there first, binding the same address fails, and that is fine: the
// host is reachable there either way, and the address is still advertised.

// MeshAddrs is this machine's addresses on point-to-point interfaces.
func MeshAddrs() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ifc.Flags&net.FlagPointToPoint == 0 && !tunnelName(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ipn.IP.To4())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// tunnelName catches overlay interfaces that some platforms do not flag as
// point-to-point.
func tunnelName(name string) bool {
	for _, p := range []string{"tailscale", "makima", "utun", "wg", "zt"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Listeners serves one handler on a changing set of addresses.
type Listeners struct {
	srv  *http.Server
	port int
	auto bool

	mu      sync.Mutex
	bound   map[string]net.Listener
	skipped map[string]bool
	errc    chan error
	stop    chan struct{}
}

// Listen binds the fixed addresses (host:port), and when auto is set, the
// loopback and mesh addresses on port as well. The first address decides the
// port when auto is set and port is zero.
func Listen(srv *http.Server, fixed []string, port int, auto bool) (*Listeners, error) {
	l := &Listeners{srv: srv, port: port, auto: auto, bound: map[string]net.Listener{},
		skipped: map[string]bool{}, errc: make(chan error, 8), stop: make(chan struct{})}
	for _, a := range fixed {
		if err := l.bind(a, true); err != nil {
			l.Close()
			return nil, err
		}
	}
	if auto {
		if err := l.bind(fmt.Sprintf("127.0.0.1:%d", port), true); err != nil {
			l.Close()
			return nil, err
		}
		l.refresh()
		go l.watch()
	}
	return l, nil
}

func (l *Listeners) bind(addr string, must bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.bound[addr]; ok {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if !must {
			if !l.skipped[addr] {
				l.skipped[addr] = true
				if errors.Is(err, syscall.EADDRINUSE) {
					log.Printf("hollow: %s is taken — a mesh daemon is probably already forwarding it here", addr)
				} else {
					log.Printf("hollow: cannot listen on %s: %v", addr, err)
				}
			}
			return nil
		}
		return err
	}
	delete(l.skipped, addr)
	l.bound[addr] = ln
	go func() {
		err := l.srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			select {
			case l.errc <- fmt.Errorf("%s: %w", addr, err):
			default:
			}
		}
	}()
	return nil
}

func (l *Listeners) refresh() {
	want := map[string]bool{}
	for _, ip := range MeshAddrs() {
		want[net.JoinHostPort(ip.String(), fmt.Sprint(l.port))] = true
	}
	for a := range want {
		_ = l.bind(a, false)
	}
	// An address that went away (the tunnel went down) is let go of, so it
	// can be bound again when it comes back.
	l.mu.Lock()
	for a, ln := range l.bound {
		host, _, _ := net.SplitHostPort(a)
		if host == "127.0.0.1" || want[a] {
			continue
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			continue
		}
		if l.auto && !want[a] && isMeshBind(a) {
			ln.Close()
			delete(l.bound, a)
		}
	}
	for a := range l.skipped {
		if !want[a] {
			delete(l.skipped, a)
		}
	}
	l.mu.Unlock()
}

func isMeshBind(addr string) bool {
	host, _, _ := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	return ip != nil && !ip.IsLoopback() && !ip.IsUnspecified()
}

func (l *Listeners) watch() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.refresh()
		}
	}
}

// Err reports a listener that failed after it started.
func (l *Listeners) Err() <-chan error { return l.errc }

// Bound is the addresses actually listened on.
func (l *Listeners) Bound() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.bound))
	for a := range l.bound {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Close stops listening everywhere.
func (l *Listeners) Close() {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for a, ln := range l.bound {
		ln.Close()
		delete(l.bound, a)
	}
}

// URLs is where a client can reach this host: every mesh address on port
// (bound by us or forwarded to us by a mesh daemon) and every fixed
// non-loopback address, then loopback last, for a client on this machine.
func URLs(port int, fixed []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(hostport string) {
		u := "http://" + hostport
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, ip := range MeshAddrs() {
		add(net.JoinHostPort(ip.String(), fmt.Sprint(port)))
	}
	for _, a := range fixed {
		h, p, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		if ip := net.ParseIP(h); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
			continue
		}
		add(net.JoinHostPort(h, p))
	}
	add(fmt.Sprintf("127.0.0.1:%d", port))
	return out
}
