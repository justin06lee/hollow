// Package qemu runs desks under QEMU with KVM.
package qemu

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/justin06lee/hollow/api"
	"github.com/justin06lee/hollow/internal/backend"
)

// Backend is the QEMU backend.
type Backend struct {
	binary string
}

// New finds QEMU. Call Check to learn whether it can actually run a VM here.
func New() *Backend {
	b := &Backend{}
	if runtime.GOARCH == "amd64" {
		b.binary, _ = exec.LookPath("qemu-system-x86_64")
	}
	return b
}

func (b *Backend) Name() string { return "qemu" }

func (b *Backend) Check() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("the qemu backend needs a Linux host with KVM; this is %s", runtime.GOOS)
	}
	if runtime.GOARCH != "amd64" {
		return fmt.Errorf("the qemu backend runs x86_64 guests and needs an x86_64 host; this is %s", runtime.GOARCH)
	}
	if b.binary == "" {
		return errors.New("qemu-system-x86_64 is not installed (pacman -S qemu-base, apt install qemu-system-x86)")
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return errors.New("qemu-img is not installed (pacman -S qemu-img, apt install qemu-utils)")
	}
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("cannot open /dev/kvm: %w (is this account in the kvm group?)", err)
	}
	f.Close()
	return nil
}

func (b *Backend) Start(ctx context.Context, spec backend.Spec) (backend.VM, error) {
	if err := b.Check(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return nil, err
	}
	qmp := filepath.Join(spec.Dir, "qmp.sock")
	os.Remove(qmp)
	pidfile := filepath.Join(spec.Dir, "qemu.pid")
	console := filepath.Join(spec.Dir, "console.log")

	args := []string{
		"-name", "hollow-" + spec.Name + ",process=hollow-" + spec.Name,
		"-machine", "q35,accel=kvm",
		"-cpu", "host",
		"-smp", strconv.Itoa(spec.CPUs),
		"-m", strconv.Itoa(spec.MemMB) + "M",
		"-display", "none",
		"-vga", "none",
		"-serial", "file:" + console,
		"-device", "virtio-rng-pci",
		"-drive", "file=" + spec.Disk + ",if=virtio,format=qcow2,cache=writeback,discard=unmap",
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:%d", spec.AgentPort, api.AgentPort),
		"-device", "virtio-net-pci,netdev=net0",
		"-qmp", "unix:" + qmp + ",server=on,wait=off",
		"-pidfile", pidfile,
		"-rtc", "base=utc",
	}
	if spec.ISO != "" {
		args = append(args, "-cdrom", spec.ISO)
	}
	if spec.NoReboot {
		args = append(args, "-no-reboot")
	}

	logf, err := os.Create(filepath.Join(spec.Dir, "qemu.log"))
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(b.binary, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("start qemu: %w", err)
	}
	v := &vm{cmd: cmd, qmp: qmp, done: make(chan struct{})}
	go func() {
		v.err = cmd.Wait()
		logf.Close()
		close(v.done)
	}()
	// QEMU that dies immediately has a reason in its log that is worth more
	// than "the agent never answered" two minutes later.
	select {
	case <-v.done:
		msg, _ := os.ReadFile(filepath.Join(spec.Dir, "qemu.log"))
		return nil, fmt.Errorf("qemu exited: %v: %s", v.err, strings.TrimSpace(string(msg)))
	case <-time.After(300 * time.Millisecond):
	}
	return v, nil
}

type vm struct {
	cmd  *exec.Cmd
	qmp  string
	done chan struct{}
	err  error
	once sync.Once
}

func (v *vm) Done() <-chan struct{} { return v.done }

func (v *vm) Wait() error {
	<-v.done
	return v.err
}

func (v *vm) Shutdown(ctx context.Context) error {
	select {
	case <-v.done:
		return nil
	default:
	}
	if err := qmpCommand(v.qmp, "system_powerdown"); err != nil {
		return v.Kill()
	}
	select {
	case <-v.done:
		return nil
	case <-ctx.Done():
		return v.Kill()
	}
}

func (v *vm) Kill() error {
	var err error
	v.once.Do(func() {
		select {
		case <-v.done:
			return
		default:
		}
		err = v.cmd.Process.Kill()
		<-v.done
	})
	return err
}

// qmpCommand sends one argument-less command over QEMU's control socket.
func qmpCommand(sock, command string) error {
	c, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	if _, err := r.ReadString('\n'); err != nil { // greeting
		return err
	}
	for _, cmd := range []string{"qmp_capabilities", command} {
		if _, err := fmt.Fprintf(c, "{\"execute\":%q}\n", cmd); err != nil {
			return err
		}
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		if strings.Contains(line, `"error"`) {
			return fmt.Errorf("qmp %s: %s", cmd, strings.TrimSpace(line))
		}
	}
	return nil
}

// CreateOverlay makes a copy-on-write disk on top of base. sizeGB of 0 keeps
// the base's size; larger grows the virtual disk, which the guest's first boot
// is expected to notice.
func CreateOverlay(base, path string, sizeGB int) error {
	abs, err := filepath.Abs(base)
	if err != nil {
		return err
	}
	args := []string{"create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", abs, path}
	if sizeGB > 0 {
		args = append(args, fmt.Sprintf("%dG", sizeGB))
	}
	out, err := exec.Command("qemu-img", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
