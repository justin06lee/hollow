// Package backend is the hypervisor behind a desk, behind an interface.
//
// The first backend is QEMU with KVM, on a Linux host. Apple's Virtualization
// framework on a Mac is the second, for macOS guests, which can run nowhere
// else. Everything above this package — images, desks, the API — is the same
// either way, which is the reason the interface exists before the second
// implementation does.
package backend

import "context"

// Spec is everything a backend needs to start one VM.
type Spec struct {
	// Name labels the process, for ps and for logs.
	Name string

	// Dir is the VM's working directory: pid file, control socket, logs.
	Dir string

	// Disk is the qcow2 to boot.
	Disk string

	// ISO, if set, is attached as a CD-ROM. It is how a desk learns what it
	// is: hollow's port, the display size, its own name.
	ISO string

	MemMB int
	CPUs  int

	// AgentPort is a port on the host's loopback that is forwarded to the
	// agent's port inside the guest.
	AgentPort int

	// NoReboot makes a guest that powers off exit rather than restart —
	// what a provisioning run wants, so that its end is observable.
	NoReboot bool
}

// VM is a running machine.
type VM interface {
	// Done is closed when the VM has exited.
	Done() <-chan struct{}

	// Wait blocks until the VM exits and reports how.
	Wait() error

	// Shutdown asks the guest to power off, and kills it when ctx ends
	// before it has.
	Shutdown(ctx context.Context) error

	// Kill ends the VM at once.
	Kill() error
}

// Backend starts VMs.
type Backend interface {
	// Name is what status output calls this backend.
	Name() string

	// Check reports why this backend cannot run on this machine, or nil.
	Check() error

	// Start boots a VM. It returns once the process is running, not once
	// the guest is up.
	Start(ctx context.Context, spec Spec) (VM, error)
}
