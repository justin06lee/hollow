package host

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/justin06lee/hollow/api"
	"github.com/justin06lee/hollow/internal/backend"
	"github.com/justin06lee/hollow/internal/backend/qemu"
	"github.com/justin06lee/hollow/internal/image"
)

const (
	defaultMemMB  = 1024
	defaultCPUs   = 2
	defaultWidth  = 1280
	defaultHeight = 800
	minMemMB      = 256
	bootTimeout   = 3 * time.Minute

	// memHeadroomMB is kept free for the host itself when admitting desks.
	memHeadroomMB = 512

	// guestHost is where a guest reaches this machine through QEMU's user
	// networking: the address of the host, seen from inside.
	guestHost = "10.0.2.2"
)

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// ErrExists is returned by Create when a desk of that name is running.
var ErrExists = errors.New("a desk with that name exists")

// Desks starts, tracks, and stops desks.
type Desks struct {
	dir     string
	port    int // the port hollow itself listens on, told to guests
	backend backend.Backend
	images  *image.Manager

	mu    sync.Mutex
	desks map[string]*desk
}

type desk struct {
	api.Desk
	dir       string
	golden    string
	agentPort int
	key       string
	vm        backend.VM
}

// NewDesks makes a manager. Whatever a previous daemon left under dir is
// removed: its VMs died with it, and a desk that is not running is nothing.
func NewDesks(dir string, hostPort int, b backend.Backend, images *image.Manager) (*Desks, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	d := &Desks{dir: dir, port: hostPort, backend: b, images: images, desks: map[string]*desk{}}
	images.SetInUse(d.goldens)
	images.GC()
	return d, nil
}

// goldens is the golden disks running desks are built on.
func (d *Desks) goldens() map[string]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]bool{}
	for _, k := range d.desks {
		out[k.golden] = true
	}
	return out
}

// List is every desk, oldest first.
func (d *Desks) List() []api.Desk {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]api.Desk, 0, len(d.desks))
	for _, k := range d.desks {
		out = append(out, k.Desk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// find looks a desk up by id, then by name.
func (d *Desks) find(ref string) (*desk, bool) {
	if k, ok := d.desks[ref]; ok {
		return k, true
	}
	for _, k := range d.desks {
		if k.Name == ref {
			return k, true
		}
	}
	return nil, false
}

// Get is one desk, by id or name.
func (d *Desks) Get(ref string) (api.Desk, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k, ok := d.find(ref)
	if !ok {
		return api.Desk{}, fmt.Errorf("no desk %q", ref)
	}
	return k.Desk, nil
}

// Agent is where a ready desk's agent answers, on this machine's loopback,
// and the key it wants. Using a desk this way counts as using it.
func (d *Desks) Agent(ref string) (string, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k, ok := d.find(ref)
	if !ok {
		return "", "", fmt.Errorf("no desk %q", ref)
	}
	if k.State != api.DeskReady {
		msg := k.State
		if k.Error != "" {
			msg += ": " + k.Error
		}
		return "", "", fmt.Errorf("desk %s is %s", k.ID, msg)
	}
	k.LastUsed = time.Now()
	return fmt.Sprintf("http://127.0.0.1:%d", k.agentPort), k.key, nil
}

// Paused reports whether a person has taken the desk from agents.
func (d *Desks) Paused(ref string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	k, ok := d.find(ref)
	return ok && k.Paused
}

// SetPaused takes a desk from agents, or gives it back.
func (d *Desks) SetPaused(ref string, paused bool) (api.Desk, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k, ok := d.find(ref)
	if !ok {
		return api.Desk{}, fmt.Errorf("no desk %q", ref)
	}
	k.Paused = paused
	k.LastUsed = time.Now()
	return k.Desk, nil
}

// resumeAfter is how long a paused desk stays paused once nobody is
// watching it. A person who took a desk over and closed the page has
// forgotten about it, and the agent waiting on it should not wait forever;
// a browser reconnecting after a blip is back well within it.
const resumeAfter = time.Minute

// Watch counts a live view open on a desk. While one is, the desk is in
// use, however long it has been since an agent touched it. Call the
// function it returns when the view closes.
func (d *Desks) Watch(ref string) func() {
	d.mu.Lock()
	k, ok := d.find(ref)
	if ok {
		k.Watchers++
		k.LastUsed = time.Now()
	}
	d.mu.Unlock()
	if !ok {
		return func() {}
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				d.mu.Lock()
				k.LastUsed = time.Now()
				d.mu.Unlock()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			d.mu.Lock()
			k.Watchers--
			k.LastUsed = time.Now()
			idle := k.Watchers == 0 && k.Paused
			d.mu.Unlock()
			if idle {
				time.AfterFunc(resumeAfter, func() {
					d.mu.Lock()
					defer d.mu.Unlock()
					if k.Watchers == 0 && k.Paused {
						k.Paused = false
						log.Printf("hollow: desk %s: nobody is watching it, so agents have it back", k.ID)
					}
				})
			}
		})
	}
}

// ConsoleLog is what the guest printed to its serial console.
func (d *Desks) ConsoleLog(ref string) (string, error) {
	d.mu.Lock()
	k, ok := d.find(ref)
	d.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("no desk %q", ref)
	}
	data, err := os.ReadFile(filepath.Join(k.dir, "console.log"))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Create boots a desk. It returns once the VM is running; the desk is
// "booting" until its agent answers, then "ready".
func (d *Desks) Create(spec api.DeskSpec) (api.Desk, error) {
	if spec.OS == "" {
		spec.OS = image.OSLinux
	}
	golden, err := d.images.Golden(spec.OS)
	if err != nil {
		return api.Desk{}, err
	}
	if spec.MemMB == 0 {
		spec.MemMB = defaultMemMB
	}
	if spec.MemMB < minMemMB {
		return api.Desk{}, fmt.Errorf("mem_mb %d is too little; %d is the floor", spec.MemMB, minMemMB)
	}
	if spec.CPUs == 0 {
		spec.CPUs = defaultCPUs
	}
	if spec.Width == 0 || spec.Height == 0 {
		spec.Width, spec.Height = defaultWidth, defaultHeight
	}
	if spec.Width < 320 || spec.Height < 240 || spec.Width > 7680 || spec.Height > 4320 {
		return api.Desk{}, fmt.Errorf("%dx%d is not a screen size", spec.Width, spec.Height)
	}
	if _, avail := meminfo(); avail > 0 && avail < spec.MemMB+memHeadroomMB {
		return api.Desk{}, fmt.Errorf("not enough memory on this host: %d MB available, a %d MB desk needs %d with headroom — close a desk, ask for less (mem_mb), or use another host",
			avail, spec.MemMB, spec.MemMB+memHeadroomMB)
	}

	id, err := newID()
	if err != nil {
		return api.Desk{}, err
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		name = id
	}
	if !nameRE.MatchString(name) {
		return api.Desk{}, fmt.Errorf("desk name %q: letters, digits, dot, dash and underscore, up to 63", name)
	}
	d.mu.Lock()
	if k, ok := d.find(name); ok && k.State != api.DeskStopped && k.State != api.DeskFailed {
		d.mu.Unlock()
		return k.Desk, fmt.Errorf("%w: %s is desk %s", ErrExists, name, k.ID)
	}
	d.mu.Unlock()

	key, err := newKey()
	if err != nil {
		return api.Desk{}, err
	}
	dir := filepath.Join(d.dir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return api.Desk{}, err
	}
	fail := func(err error) (api.Desk, error) {
		os.RemoveAll(dir)
		return api.Desk{}, err
	}

	disk := filepath.Join(dir, "disk.qcow2")
	if err := qemu.CreateOverlay(golden, disk, 0); err != nil {
		return fail(err)
	}
	port, err := backend.FreePort()
	if err != nil {
		return fail(err)
	}
	env := fmt.Sprintf("HOLLOW_HOST=%s\nHOLLOW_PORT=%d\nHOLLOW_WIDTH=%d\nHOLLOW_HEIGHT=%d\nHOLLOW_DESK=%s\nHOLLOW_NAME=%s\nHOLLOW_AGENT_KEY=%s\n",
		guestHost, d.port, spec.Width, spec.Height, id, name, key)
	iso := filepath.Join(dir, "hollow.iso")
	if err := image.WriteISO(iso, "hollow", map[string][]byte{"hollow.env": []byte(env)}); err != nil {
		return fail(err)
	}

	vm, err := d.backend.Start(context.Background(), backend.Spec{
		Name:      id,
		Dir:       dir,
		Disk:      disk,
		ISO:       iso,
		MemMB:     spec.MemMB,
		CPUs:      spec.CPUs,
		AgentPort: port,
	})
	if err != nil {
		return fail(err)
	}
	// The ISO held the key; the VM has it open, and nothing else needs it.
	now := time.Now()
	k := &desk{
		Desk: api.Desk{
			ID: id, Name: name, OS: spec.OS, State: api.DeskBooting,
			MemMB: spec.MemMB, CPUs: spec.CPUs, Width: spec.Width, Height: spec.Height,
			Created: now, LastUsed: now,
		},
		dir: dir, golden: golden, agentPort: port, key: key, vm: vm,
	}
	d.mu.Lock()
	d.desks[id] = k
	d.mu.Unlock()
	go d.watch(k)
	return k.Desk, nil
}

// watch waits for the agent to answer, then for the VM to exit.
func (d *Desks) watch(k *desk) {
	health := fmt.Sprintf("http://127.0.0.1:%d/health", k.agentPort)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(bootTimeout)
	for {
		select {
		case <-k.vm.Done():
			d.setState(k, api.DeskStopped, "the VM exited while booting: "+strings.TrimSpace(errString(k.vm.Wait())))
			return
		case <-time.After(500 * time.Millisecond):
		}
		req, _ := http.NewRequest(http.MethodGet, health, nil)
		req.Header.Set("Authorization", "Bearer "+k.key)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			d.setState(k, api.DeskFailed, "the agent did not answer within "+bootTimeout.String()+"; see the desk's logs")
			return
		}
	}
	d.setState(k, api.DeskReady, "")
	<-k.vm.Done()
	d.setState(k, api.DeskStopped, "")
}

func (d *Desks) setState(k *desk, state, errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if k.State == api.DeskStopped {
		return
	}
	k.State, k.Error = state, errMsg
	k.LastUsed = time.Now()
}

// Delete stops a desk and removes everything it had.
func (d *Desks) Delete(ctx context.Context, ref string) error {
	d.mu.Lock()
	k, ok := d.find(ref)
	if ok {
		delete(d.desks, k.ID)
	}
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("no desk %q", ref)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = k.vm.Shutdown(ctx)
	err := os.RemoveAll(k.dir)
	d.images.GC()
	return err
}

// Reap stops desks nobody has used for idle, and desks whose VM has
// stopped. It runs until ctx ends.
func (d *Desks) Reap(ctx context.Context, idle time.Duration) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d.mu.Lock()
		var stale []string
		for id, k := range d.desks {
			if k.State == api.DeskStopped && time.Since(k.LastUsed) > 10*time.Minute {
				stale = append(stale, id)
			} else if idle > 0 && time.Since(k.LastUsed) > idle {
				stale = append(stale, id)
			}
		}
		d.mu.Unlock()
		for _, id := range stale {
			log.Printf("hollow: stopping desk %s, unused for longer than %s", id, idle)
			_ = d.Delete(ctx, id)
		}
	}
}

// Close stops every desk. Called when the daemon exits.
func (d *Desks) Close() {
	d.mu.Lock()
	all := make([]*desk, 0, len(d.desks))
	for _, k := range d.desks {
		all = append(all, k)
	}
	d.desks = map[string]*desk{}
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, k := range all {
		wg.Add(1)
		go func(k *desk) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_ = k.vm.Shutdown(ctx)
			os.RemoveAll(k.dir)
		}(k)
	}
	wg.Wait()
}

func newID() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func errString(err error) string {
	if err == nil {
		return "exit 0"
	}
	if errors.Is(err, context.Canceled) {
		return "stopped"
	}
	return err.Error()
}
