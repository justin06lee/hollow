package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// `hollow service` turns a Linux machine into a host with nothing but this
// binary: no checkout, no make, no Go. It is what phaethon runs on a machine
// it sets up, and what a person runs on a box of their own.

const (
	serviceBin  = "/usr/local/bin/hollow"
	serviceUnit = "/etc/systemd/system/hollow.service"
	serviceHome = "/var/lib/hollow"
	serviceUser = "hollow"
)

const unitTemplate = `[Unit]
Description=hollow — computers for bots
Documentation=https://github.com/justin06lee/hollow
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s serve --quiet%s
Restart=on-failure
RestartSec=2
User=hollow
Group=hollow
SupplementaryGroups=kvm
Environment=HOLLOW_HOME=/var/lib/hollow
EnvironmentFile=-/etc/hollow.env
KillMode=mixed
TimeoutStopSec=30

NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/hollow
PrivateTmp=true
DevicePolicy=closed
DeviceAllow=/dev/kvm rw

[Install]
WantedBy=multi-user.target
`

func cmdService(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: hollow service install|uninstall|status")
	}
	switch args[0] {
	case "install", "update":
		return serviceInstall(ctx, args[1:])
	case "uninstall", "remove":
		return serviceUninstall(args[1:])
	case "status":
		return serviceStatus(ctx)
	}
	return fmt.Errorf("unknown service command %q: install, uninstall or status", args[0])
}

func say(quiet bool, format string, a ...any) {
	if !quiet {
		fmt.Fprintf(os.Stderr, "  "+format+"\n", a...)
	}
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func needRoot() error {
	if runtime.GOOS != "linux" {
		return errNotLinux
	}
	if os.Geteuid() != 0 {
		return errors.New("this needs root: run it with sudo")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("no systemctl here; run `hollow serve` under whatever supervises processes on this machine")
	}
	return nil
}

func serviceInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("service install", flag.ContinueOnError)
	noDeps := fs.Bool("no-deps", false, "do not install QEMU when it is missing")
	idle := fs.Duration("idle", 0, "stop desks unused for this long (0: never)")
	quiet := fs.Bool("quiet", false, "print nothing but errors")
	asJSON := fs.Bool("json", false, "when done, print the connect details as JSON on stdout")
	var urls multiFlag
	fs.Var(&urls, "url", "an address clients will reach this host at, put first in the connect code (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := needRoot(); err != nil {
		return err
	}

	if runtime.GOARCH != "amd64" {
		return fmt.Errorf("hollow runs x86_64 guests and needs an x86_64 host; this is %s", runtime.GOARCH)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return errors.New("no /dev/kvm: turn on virtualization (VT-x/AMD-V) in the firmware, or nested virtualization if this is itself a VM")
	}
	if err := ensureQEMU(*noDeps, *quiet); err != nil {
		return err
	}

	// The binary: copy whichever one is running to where the unit expects
	// it. Renamed into place, so a running old one is not written over.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	if self != serviceBin {
		if err := copyFile(self, serviceBin+".new", 0o755); err != nil {
			return err
		}
		if err := os.Rename(serviceBin+".new", serviceBin); err != nil {
			return err
		}
		say(*quiet, "installed %s", serviceBin)
	}

	if err := exec.Command("id", serviceUser).Run(); err != nil {
		if err := run("useradd", "--system", "--home-dir", serviceHome, "--shell", "/usr/sbin/nologin", "--user-group", serviceUser); err != nil {
			return err
		}
		say(*quiet, "created the %s user", serviceUser)
	}
	if exec.Command("getent", "group", "kvm").Run() == nil {
		_ = run("usermod", "-aG", "kvm", serviceUser)
	}
	if err := os.MkdirAll(serviceHome, 0o755); err != nil {
		return err
	}
	// State made by a hollow run by hand as root — an image already built,
	// a token already handed out — carries over to the service.
	if err := run("chown", "-R", serviceUser+":"+serviceUser, serviceHome); err != nil {
		return err
	}

	flags := ""
	if *idle > 0 {
		flags = " --idle " + idle.String()
	}
	unit := fmt.Sprintf(unitTemplate, serviceBin, flags)
	if err := os.WriteFile(serviceUnit, []byte(unit), 0o644); err != nil {
		return err
	}
	for _, a := range [][]string{{"daemon-reload"}, {"enable", "hollow"}, {"restart", "hollow"}} {
		if err := run("systemctl", a...); err != nil {
			return err
		}
	}
	say(*quiet, "hollow is running as a service, and will be after every boot")

	// Wait for it to answer, and say why when it does not.
	deadline := time.Now().Add(20 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/hello", defaultPort()), nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			out, _ := exec.Command("journalctl", "-u", "hollow", "-n", "20", "--no-pager").CombinedOutput()
			return fmt.Errorf("the service started but does not answer on port %d; its log:\n%s", defaultPort(), out)
		}
		time.Sleep(300 * time.Millisecond)
	}

	// The token is readable by the hollow group, and whoever installed this
	// with sudo joins it: they can mint connect codes from now on without
	// being root.
	token := filepath.Join(serviceHome, "token")
	_ = os.Chmod(token, 0o640)
	_ = run("chgrp", serviceUser, token)
	if u := os.Getenv("SUDO_USER"); u != "" && u != "root" {
		if run("usermod", "-aG", serviceUser, u) == nil {
			say(*quiet, "%s can now run `hollow connect --state %s` without sudo (from its next login)", u, serviceHome)
		}
	}

	os.Setenv("HOLLOW_HOME", serviceHome)
	cargs := []string{"--state", serviceHome}
	for _, u := range urls {
		cargs = append(cargs, "--url", u)
	}
	if *asJSON {
		cargs = append(cargs, "--json")
		return cmdConnect(ctx, cargs)
	}
	if !*quiet {
		fmt.Fprintln(os.Stderr, "\n  connect code, for bangboo host add or HOLLOW_CONNECT:")
		fmt.Fprint(os.Stderr, "      ")
	}
	return cmdConnect(ctx, cargs)
}

// ensureQEMU installs QEMU with whichever package manager this machine has.
func ensureQEMU(noDeps, quiet bool) error {
	_, e1 := exec.LookPath("qemu-system-x86_64")
	_, e2 := exec.LookPath("qemu-img")
	if e1 == nil && e2 == nil {
		return nil
	}
	type pm struct {
		bin  string
		cmds [][]string
	}
	managers := []pm{
		{"pacman", [][]string{{"pacman", "-S", "--needed", "--noconfirm", "qemu-base", "qemu-img"}}},
		{"apt-get", [][]string{{"apt-get", "update", "-q"}, {"apt-get", "install", "-y", "-q", "qemu-system-x86", "qemu-utils"}}},
		{"dnf", [][]string{{"dnf", "install", "-y", "qemu-kvm", "qemu-img"}}},
		{"zypper", [][]string{{"zypper", "-n", "install", "qemu-x86", "qemu-tools"}}},
		{"apk", [][]string{{"apk", "add", "qemu-system-x86_64", "qemu-img"}}},
	}
	for _, m := range managers {
		if _, err := exec.LookPath(m.bin); err != nil {
			continue
		}
		if noDeps {
			return fmt.Errorf("QEMU is not installed; install it with: %s", strings.Join(m.cmds[len(m.cmds)-1], " "))
		}
		say(quiet, "installing QEMU with %s", m.bin)
		for _, c := range m.cmds {
			cmd := exec.Command(c[0], c[1:]...)
			cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
			if out, err := cmd.CombinedOutput(); err != nil {
				hint := ""
				if m.bin == "pacman" {
					hint = "\n(on Arch this usually means the package database is stale: run pacman -Syu, then try again)"
				}
				return fmt.Errorf("%s: %v\n%s%s", strings.Join(c, " "), err, tail(string(out), 12), hint)
			}
		}
		return nil
	}
	return errors.New("QEMU is not installed and no known package manager is here; install qemu-system-x86_64 and qemu-img")
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func serviceUninstall(args []string) error {
	fs := flag.NewFlagSet("service uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also delete images, the token, and the hollow user")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := needRoot(); err != nil {
		return err
	}
	_ = run("systemctl", "disable", "--now", "hollow")
	os.Remove(serviceUnit)
	_ = run("systemctl", "daemon-reload")
	fmt.Fprintln(os.Stderr, "  the hollow service is gone")
	if *purge {
		os.RemoveAll(serviceHome)
		_ = run("userdel", serviceUser)
		fmt.Fprintf(os.Stderr, "  removed %s and the %s user\n", serviceHome, serviceUser)
	} else {
		fmt.Fprintf(os.Stderr, "  images and the token are still in %s (--purge removes them)\n", serviceHome)
	}
	return nil
}

func serviceStatus(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return errNotLinux
	}
	active, _ := exec.Command("systemctl", "is-active", "hollow").Output()
	enabled, _ := exec.Command("systemctl", "is-enabled", "hollow").Output()
	fmt.Printf("service   %s, %s at boot\n", strings.TrimSpace(string(active)), strings.TrimSpace(string(enabled)))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/hello", defaultPort()), nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		fmt.Printf("answering %s\n", strings.TrimSpace(string(body)))
	} else {
		fmt.Printf("answering no: %v\n", err)
	}
	return nil
}
