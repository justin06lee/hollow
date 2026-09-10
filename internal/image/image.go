// Package image builds and keeps the golden images desks are cloned from.
//
// One per guest OS. A pull downloads the vendor's stock image, boots it once
// with a provisioning script, and keeps the powered-off result; every desk
// is then a copy-on-write overlay on it, so a desk costs the disk it writes
// and nothing else.
package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/justin06lee/hollow/api"
	"github.com/justin06lee/hollow/internal/backend"
	"github.com/justin06lee/hollow/internal/backend/qemu"
)

const (
	// Alpine's "generic" cloud image: BIOS boot, cloud-init, x86_64.
	alpineBranch = "v3.24"
	alpineFile   = "generic_alpine-3.24.1-x86_64-bios-cloudinit-r0.qcow2"
	alpineURL    = "https://dl-cdn.alpinelinux.org/alpine/" + alpineBranch + "/releases/cloud/" + alpineFile

	// The golden disk's virtual size. Sparse, so this costs nothing until a
	// desk writes to it; cloud-init grows the filesystem into it on the
	// provisioning boot.
	goldenSizeGB = 10

	provisionMemMB   = 1024
	provisionCPUs    = 2
	provisionTimeout = 30 * time.Minute

	// Linux is the only OS with an image builder so far.
	OSLinux = "linux"
)

// Manager keeps one image per OS under a directory.
type Manager struct {
	dir     string
	backend backend.Backend

	mu     sync.Mutex
	images map[string]*api.Image
	jobs   map[string]bool
}

// New loads whatever state is on disk. A pull that a previous daemon left
// half-done is reported as failed rather than as still running.
func New(dir string, b backend.Backend) *Manager {
	m := &Manager{dir: dir, backend: b, images: map[string]*api.Image{}, jobs: map[string]bool{}}
	for _, osName := range []string{OSLinux} {
		img := &api.Image{OS: osName, State: api.ImageMissing}
		if data, err := os.ReadFile(m.statePath(osName)); err == nil {
			_ = json.Unmarshal(data, img)
			img.OS = osName
		}
		switch img.State {
		case api.ImagePulling, api.ImageProvisioning:
			img.State = api.ImageFailed
			img.Error = "interrupted; pull again"
		case api.ImageReady:
			if _, err := os.Stat(m.goldenPath(osName)); err != nil {
				img.State = api.ImageMissing
				img.Error = ""
			}
		}
		m.images[osName] = img
	}
	return m
}

func (m *Manager) osDir(osName string) string { return filepath.Join(m.dir, osName) }
func (m *Manager) statePath(osName string) string {
	return filepath.Join(m.osDir(osName), "state.json")
}
func (m *Manager) basePath(osName string) string { return filepath.Join(m.osDir(osName), "base.qcow2") }
func (m *Manager) goldenPath(osName string) string {
	return filepath.Join(m.osDir(osName), "golden.qcow2")
}

// List is every image hollow knows how to build, with its state.
func (m *Manager) List() []api.Image {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]api.Image, 0, len(m.images))
	for _, osName := range []string{OSLinux} {
		out = append(out, *m.images[osName])
	}
	return out
}

// Get is one image's state.
func (m *Manager) Get(osName string) (api.Image, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	img, ok := m.images[osName]
	if !ok {
		return api.Image{}, fmt.Errorf("no such os %q; hollow builds: %s", osName, OSLinux)
	}
	return *img, nil
}

// Golden is the path of a ready image's disk, to be cloned.
func (m *Manager) Golden(osName string) (string, error) {
	img, err := m.Get(osName)
	if err != nil {
		return "", err
	}
	switch img.State {
	case api.ImageReady:
		return m.goldenPath(osName), nil
	case api.ImagePulling, api.ImageProvisioning:
		return "", fmt.Errorf("the %s image is still being built (%s)", osName, img.Progress)
	case api.ImageFailed:
		return "", fmt.Errorf("the %s image failed to build: %s — pull it again", osName, img.Error)
	}
	return "", fmt.Errorf("no %s image yet: pull it first", osName)
}

// Pull builds the image in the background. It returns at once; watch Get.
func (m *Manager) Pull(osName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	img, ok := m.images[osName]
	if !ok {
		return fmt.Errorf("no such os %q; hollow builds: %s", osName, OSLinux)
	}
	if m.jobs[osName] {
		return fmt.Errorf("the %s image is already being built (%s)", osName, img.Progress)
	}
	if err := os.MkdirAll(m.osDir(osName), 0o755); err != nil {
		return err
	}
	m.jobs[osName] = true
	go func() {
		err := m.pullLinux(context.Background())
		m.mu.Lock()
		delete(m.jobs, osName)
		m.mu.Unlock()
		if err != nil {
			m.set(osName, api.ImageFailed, "", err.Error())
		}
	}()
	return nil
}

func (m *Manager) set(osName, state, progress, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	img := m.images[osName]
	img.State, img.Progress, img.Error = state, progress, errMsg
	img.Updated = time.Now()
	if state == api.ImageReady {
		img.Size = 0
		for _, p := range []string{m.basePath(osName), m.goldenPath(osName)} {
			if st, err := os.Stat(p); err == nil {
				img.Size += st.Size()
			}
		}
	}
	if data, err := json.MarshalIndent(img, "", "  "); err == nil {
		_ = os.WriteFile(m.statePath(osName), data, 0o644)
	}
}

func (m *Manager) progress(osName, state string) func(string) {
	return func(line string) { m.set(osName, state, line, "") }
}

func (m *Manager) pullLinux(ctx context.Context) error {
	if err := m.backend.Check(); err != nil {
		return err
	}
	osName := OSLinux
	base, golden := m.basePath(osName), m.goldenPath(osName)
	report := m.progress(osName, api.ImagePulling)

	report("fetching checksum")
	want, err := fetchSHA512(ctx, alpineURL+".sha512")
	if err != nil {
		return fmt.Errorf("checksum: %w", err)
	}
	have := ""
	if _, err := os.Stat(base); err == nil {
		report("checking the base image already here")
		have, _ = fileSHA512(base)
	}
	if have != want {
		report("downloading alpine")
		if err := download(ctx, alpineURL, base, want, report); err != nil {
			return fmt.Errorf("download: %w", err)
		}
	}

	report = m.progress(osName, api.ImageProvisioning)
	report("preparing the first boot")
	os.Remove(golden)
	if err := qemu.CreateOverlay(base, golden, goldenSizeGB); err != nil {
		return err
	}
	seed := filepath.Join(m.osDir(osName), "seed.iso")
	err = WriteISO(seed, "cidata", map[string][]byte{
		"user-data": []byte(linuxProvision),
		"meta-data": []byte(linuxMetaData),
	})
	if err != nil {
		return fmt.Errorf("seed iso: %w", err)
	}
	port, err := backend.FreePort()
	if err != nil {
		return err
	}
	buildDir := filepath.Join(m.osDir(osName), "build")
	os.RemoveAll(buildDir)
	vm, err := m.backend.Start(ctx, backend.Spec{
		Name:      "build-" + osName,
		Dir:       buildDir,
		Disk:      golden,
		ISO:       seed,
		MemMB:     provisionMemMB,
		CPUs:      provisionCPUs,
		AgentPort: port,
		NoReboot:  true,
	})
	if err != nil {
		return err
	}
	report("first boot: waiting for the guest")

	console := filepath.Join(buildDir, "console.log")
	deadline := time.After(provisionTimeout)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
wait:
	for {
		select {
		case <-vm.Done():
			break wait
		case <-deadline:
			_ = vm.Kill()
			return fmt.Errorf("provisioning did not finish in %s; last output: %s", provisionTimeout, lastConsoleLine(console))
		case <-ctx.Done():
			_ = vm.Kill()
			return ctx.Err()
		case <-tick.C:
			if line := lastHollowLine(console); line != "" {
				report("first boot: " + line)
			}
		}
	}
	data, _ := os.ReadFile(console)
	out := string(data)
	if !strings.Contains(out, "HOLLOW-PROVISION-OK") {
		if i := strings.Index(out, "HOLLOW-PROVISION-FAILED"); i >= 0 {
			return errors.New("provisioning failed inside the guest: " + strings.TrimSpace(out[i:]))
		}
		return fmt.Errorf("the guest powered off without finishing; last output: %s", lastConsoleLine(console))
	}
	os.Remove(seed)
	m.set(osName, api.ImageReady, "", "")
	return nil
}

// lastHollowLine is the newest line the provisioning script printed for us.
func lastHollowLine(console string) string {
	data, err := os.ReadFile(console)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); strings.HasPrefix(line, "hollow: ") {
			return strings.TrimPrefix(line, "hollow: ")
		}
	}
	return ""
}

func lastConsoleLine(console string) string {
	data, err := os.ReadFile(console)
	if err != nil {
		return "(no console output)"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "(no console output)"
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
