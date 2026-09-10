package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
	defaultMemMB  = 768
	defaultCPUs   = 2
	defaultWidth  = 1280
	defaultHeight = 800
	minMemMB      = 256
	bootTimeout   = 3 * time.Minute

	// guestHost is where a guest reaches this machine through QEMU's user
	// networking: the address of the host, seen from inside.
	guestHost = "10.0.2.2"
)

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
	agentPort int
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
	return &Desks{dir: dir, port: hostPort, backend: b, images: images, desks: map[string]*desk{}}, nil
}

// List is every desk, newest last.
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

// Get is one desk.
func (d *Desks) Get(id string) (api.Desk, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k, ok := d.desks[id]
	if !ok {
		return api.Desk{}, fmt.Errorf("no desk %q", id)
	}
	return k.Desk, nil
}

// AgentURL is where a ready desk's agent answers, on this machine's loopback.
func (d *Desks) AgentURL(id string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k, ok := d.desks[id]
	if !ok {
		return "", fmt.Errorf("no desk %q", id)
	}
	if k.State != api.DeskReady {
		msg := k.State
		if k.Error != "" {
			msg += ": " + k.Error
		}
		return "", fmt.Errorf("desk %s is %s", id, msg)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", k.agentPort), nil
}

// ConsoleLog is what the guest printed to its serial console.
func (d *Desks) ConsoleLog(id string) (string, error) {
	d.mu.Lock()
	k, ok := d.desks[id]
	d.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("no desk %q", id)
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

	id, err := newID()
	if err != nil {
		return api.Desk{}, err
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		name = id
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
	env := fmt.Sprintf("HOLLOW_HOST=%s\nHOLLOW_PORT=%d\nHOLLOW_WIDTH=%d\nHOLLOW_HEIGHT=%d\nHOLLOW_DESK=%s\nHOLLOW_NAME=%s\n",
		guestHost, d.port, spec.Width, spec.Height, id, name)
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

	k := &desk{
		Desk: api.Desk{
			ID: id, Name: name, OS: spec.OS, State: api.DeskBooting,
			MemMB: spec.MemMB, CPUs: spec.CPUs, Width: spec.Width, Height: spec.Height,
			Created: time.Now(),
		},
		dir: dir, agentPort: port, vm: vm,
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
		resp, err := client.Get(health)
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
}

// Delete stops a desk and removes everything it had.
func (d *Desks) Delete(ctx context.Context, id string) error {
	d.mu.Lock()
	k, ok := d.desks[id]
	if ok {
		delete(d.desks, id)
	}
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("no desk %q", id)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = k.vm.Shutdown(ctx)
	return os.RemoveAll(k.dir)
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

func errString(err error) string {
	if err == nil {
		return "exit 0"
	}
	if errors.Is(err, context.Canceled) {
		return "stopped"
	}
	return err.Error()
}
