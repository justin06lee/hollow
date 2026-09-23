// Package image builds and keeps the golden images desks are cloned from.
//
// One per guest OS. A pull downloads the vendor's stock image, boots it once
// with a provisioning script, flattens the result into a standalone golden
// disk, and keeps it; every desk is then a copy-on-write overlay on it, so a
// desk costs the disk it writes and nothing else.
//
// Each build gets its own file name. A desk's overlay names its golden disk
// as its backing file, so a golden disk must never change underneath a running
// desk: a new pull writes a new file, and the old one is removed only once no
// desk is using it.
package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

	// OSLinux is the only OS with an image builder so far.
	OSLinux = "linux"
)

// record is what is kept on disk about an image: what the API shows, and
// which file is the current golden disk.
type record struct {
	api.Image
	Golden string `json:"golden,omitempty"` // file name in the OS's directory
}

// Manager keeps one image per OS under a directory.
type Manager struct {
	dir     string
	backend backend.Backend

	mu     sync.Mutex
	images map[string]*record
	jobs   map[string]bool
	inUse  func() map[string]bool
}

// New loads whatever state is on disk. A pull that a previous daemon left
// half-done is reported as failed rather than as still running.
func New(dir string, b backend.Backend) *Manager {
	m := &Manager{dir: dir, backend: b, images: map[string]*record{}, jobs: map[string]bool{}}
	for _, osName := range []string{OSLinux} {
		rec := &record{Image: api.Image{OS: osName, State: api.ImageMissing}}
		if data, err := os.ReadFile(m.statePath(osName)); err == nil {
			_ = json.Unmarshal(data, rec)
			rec.OS = osName
		}
		// Images built before recipes had numbers were built with the first.
		if rec.Recipe == 0 && rec.State == api.ImageReady {
			rec.Recipe = 1
		}
		if rec.Golden == "" {
			rec.Golden = "golden.qcow2"
		}
		switch rec.State {
		case api.ImagePulling, api.ImageProvisioning:
			rec.State = api.ImageFailed
			rec.Error = "interrupted; pull again"
		case api.ImageReady:
			if _, err := os.Stat(filepath.Join(m.osDir(osName), rec.Golden)); err != nil {
				rec.State = api.ImageMissing
				rec.Error = ""
			}
		}
		if rec.State == api.ImageReady {
			repairBacking(m.osDir(osName), rec.Golden)
		}
		m.images[osName] = rec
	}
	return m
}

// repairBacking points a golden disk that was built as an overlay back at
// the base beside it, when the path it recorded no longer exists — which is
// what happens when a state directory is moved. Flattened golden disks have
// no backing file and are left alone.
func repairBacking(dir, golden string) {
	path := filepath.Join(dir, golden)
	out, err := exec.Command("qemu-img", "info", "--output=json", "-U", path).Output()
	if err != nil {
		return
	}
	var info struct {
		Backing string `json:"full-backing-filename"`
	}
	if json.Unmarshal(out, &info) != nil || info.Backing == "" || fileExists(info.Backing) {
		return
	}
	base := filepath.Join(dir, "base.qcow2")
	if !fileExists(base) {
		return
	}
	// -u rewrites only the pointer: the data is the same base, moved.
	cmd := exec.Command("qemu-img", "rebase", "-u", "-F", "qcow2", "-b", "base.qcow2", golden)
	cmd.Dir = dir
	_ = cmd.Run()
}

// SetInUse tells the manager how to learn which golden disks running desks
// are built on, so that it never removes one of those.
func (m *Manager) SetInUse(f func() map[string]bool) {
	m.mu.Lock()
	m.inUse = f
	m.mu.Unlock()
}

func (m *Manager) osDir(osName string) string { return filepath.Join(m.dir, osName) }
func (m *Manager) statePath(osName string) string {
	return filepath.Join(m.osDir(osName), "state.json")
}
func (m *Manager) basePath(osName string) string {
	return filepath.Join(m.osDir(osName), "base-"+alpineFile)
}

func (m *Manager) view(rec *record) api.Image {
	img := rec.Image
	img.Outdated = img.State == api.ImageReady && img.Recipe < linuxRecipe
	return img
}

// List is every image hollow knows how to build, with its state.
func (m *Manager) List() []api.Image {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]api.Image, 0, len(m.images))
	for _, osName := range []string{OSLinux} {
		out = append(out, m.view(m.images[osName]))
	}
	return out
}

// Get is one image's state.
func (m *Manager) Get(osName string) (api.Image, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.images[osName]
	if !ok {
		return api.Image{}, fmt.Errorf("no such os %q; hollow builds: %s", osName, OSLinux)
	}
	return m.view(rec), nil
}

// Golden is the path of a ready image's disk, to be cloned.
func (m *Manager) Golden(osName string) (string, error) {
	m.mu.Lock()
	rec, ok := m.images[osName]
	var img api.Image
	var golden string
	if ok {
		img, golden = m.view(rec), rec.Golden
	}
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("no such os %q; hollow builds: %s", osName, OSLinux)
	}
	switch img.State {
	case api.ImageReady:
		return filepath.Join(m.osDir(osName), golden), nil
	case api.ImagePulling, api.ImageProvisioning:
		// A rebuild of an image that was ready leaves the old one usable
		// until the new one replaces it.
		if p := filepath.Join(m.osDir(osName), golden); fileExists(p) && img.Recipe > 0 {
			return p, nil
		}
		return "", fmt.Errorf("the %s image is still being built (%s)", osName, img.Progress)
	case api.ImageFailed:
		if p := filepath.Join(m.osDir(osName), golden); fileExists(p) && img.Recipe > 0 {
			return p, nil
		}
		return "", fmt.Errorf("the %s image failed to build: %s — pull it again", osName, img.Error)
	}
	return "", fmt.Errorf("no %s image yet: pull it first", osName)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Pull builds the image in the background. It returns at once; watch Get.
func (m *Manager) Pull(osName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.images[osName]
	if !ok {
		return fmt.Errorf("no such os %q; hollow builds: %s", osName, OSLinux)
	}
	if m.jobs[osName] {
		return fmt.Errorf("the %s image is already being built (%s)", osName, rec.Progress)
	}
	if err := os.MkdirAll(m.osDir(osName), 0o755); err != nil {
		return err
	}
	m.jobs[osName] = true
	rec.State, rec.Progress, rec.Error = api.ImagePulling, "starting", ""
	go func() {
		err := m.pullLinux(context.Background())
		m.mu.Lock()
		delete(m.jobs, osName)
		m.mu.Unlock()
		if err != nil {
			m.set(osName, func(r *record) { r.State, r.Progress, r.Error = api.ImageFailed, "", err.Error() })
		}
	}()
	return nil
}

func (m *Manager) set(osName string, change func(*record)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.images[osName]
	change(rec)
	rec.Updated = time.Now()
	if data, err := json.MarshalIndent(rec, "", "  "); err == nil {
		tmp := m.statePath(osName) + ".tmp"
		if os.WriteFile(tmp, data, 0o644) == nil {
			_ = os.Rename(tmp, m.statePath(osName))
		}
	}
}

func (m *Manager) progress(osName, state string) func(string) {
	return func(line string) {
		m.set(osName, func(r *record) { r.State, r.Progress, r.Error = state, line, "" })
	}
}

// GC removes golden disks that are neither current nor under a running
// desk, and base images this hollow would no longer build from.
func (m *Manager) GC() {
	m.mu.Lock()
	var inUse map[string]bool
	if m.inUse != nil {
		inUse = m.inUse()
	}
	type keep struct{ dir, golden string }
	var dirs []keep
	for osName, rec := range m.images {
		if m.jobs[osName] {
			continue
		}
		dirs = append(dirs, keep{m.osDir(osName), rec.Golden})
	}
	m.mu.Unlock()

	for _, k := range dirs {
		entries, err := os.ReadDir(k.dir)
		if err != nil {
			continue
		}
		legacyGolden := false
		for _, e := range entries {
			name := e.Name()
			path := filepath.Join(k.dir, name)
			switch {
			case strings.HasPrefix(name, "golden") && strings.HasSuffix(name, ".qcow2"):
				if name == k.golden || inUse[path] {
					if name == "golden.qcow2" {
						legacyGolden = true
					}
					continue
				}
				os.Remove(path)
			case name == "build.qcow2" || name == "build" || name == "seed.iso":
				os.RemoveAll(path)
			case strings.HasPrefix(name, "base-") && name != "base-"+alpineFile:
				os.Remove(path)
			}
		}
		// The first images were built on an overlay of base.qcow2 rather
		// than flattened, so that file lives as long as such an image does.
		if !legacyGolden {
			os.Remove(filepath.Join(k.dir, "base.qcow2"))
		}
	}
}

func (m *Manager) pullLinux(ctx context.Context) error {
	if err := m.backend.Check(); err != nil {
		return err
	}
	osName := OSLinux
	dir := m.osDir(osName)
	base := m.basePath(osName)
	report := m.progress(osName, api.ImagePulling)

	report("fetching checksum")
	want, err := fetchSHA512(ctx, alpineURL+".sha512")
	if err != nil {
		return fmt.Errorf("checksum: %w", err)
	}
	// A base downloaded under the old name is the same file: link it rather
	// than fetch it again.
	if old := filepath.Join(dir, "base.qcow2"); !fileExists(base) && fileExists(old) {
		if sum, err := fileSHA512(old); err == nil && sum == want {
			_ = os.Link(old, base)
		}
	}
	have := ""
	if fileExists(base) {
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
	build := filepath.Join(dir, "build.qcow2")
	os.Remove(build)
	if err := qemu.CreateOverlay(base, build, goldenSizeGB); err != nil {
		return err
	}
	defer os.Remove(build)
	seed := filepath.Join(dir, "seed.iso")
	err = WriteISO(seed, "cidata", map[string][]byte{
		"user-data": []byte(linuxProvision),
		"meta-data": []byte(linuxMetaData),
	})
	if err != nil {
		return fmt.Errorf("seed iso: %w", err)
	}
	defer os.Remove(seed)
	port, err := backend.FreePort()
	if err != nil {
		return err
	}
	buildDir := filepath.Join(dir, "build")
	os.RemoveAll(buildDir)
	vm, err := m.backend.Start(ctx, backend.Spec{
		Name:      "build-" + osName,
		Dir:       buildDir,
		Disk:      build,
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
	os.RemoveAll(buildDir)

	// Flatten: a golden disk that stands alone does not care what happens
	// to the base it was made from.
	report("finishing: flattening the disk")
	name := fmt.Sprintf("golden-%d.qcow2", time.Now().Unix())
	if out, err := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "qcow2", build, filepath.Join(dir, name)).CombinedOutput(); err != nil {
		os.Remove(filepath.Join(dir, name))
		return fmt.Errorf("qemu-img convert: %s", strings.TrimSpace(string(out)))
	}
	size := int64(0)
	if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
		size = st.Size()
	}
	m.set(osName, func(r *record) {
		r.State, r.Progress, r.Error = api.ImageReady, "", ""
		r.Golden, r.Recipe, r.Size = name, linuxRecipe, size
	})
	m.mu.Lock()
	delete(m.jobs, osName) // so GC does not skip this directory
	m.mu.Unlock()
	m.GC()
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
