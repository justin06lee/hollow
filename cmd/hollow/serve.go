package main

import (
	"context"
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

const defaultAddr = "127.0.0.1:7070"

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", envOr("HOLLOW_ADDR", defaultAddr), "address to listen on")
	state := fs.String("state", host.DefaultDir(), "where images, desks and the token live")
	advertise := fs.String("advertise", os.Getenv("HOLLOW_ADVERTISE"), "URL to put in the printed connect code, when clients reach this host by another name")
	quiet := fs.Bool("quiet", false, "print nothing but errors")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: hollow serve [flags]\n\nRuns the host. Loopback by default: put it on a mesh (makima publishes\nloopback listeners on its own) or bind --addr to an address you mean to expose.")
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

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// Guests reach this machine at its loopback, through the hypervisor's
	// user networking, and that is where they fetch the agent from. A host
	// bound to a mesh address instead of loopback still has to answer there.
	var loopback net.Listener
	if ip := ln.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() && !ip.IsUnspecified() {
		loopback, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			ln.Close()
			return fmt.Errorf("guests need 127.0.0.1:%d as well: %w", port, err)
		}
	}
	desks, err := host.NewDesks(filepath.Join(dir, "desks"), port, b, images)
	if err != nil {
		return err
	}
	srv := host.NewServer(version, token, b, images, desks)
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 15 * time.Second}

	url := *advertise
	if url == "" {
		url = "http://" + printableAddr(ln.Addr().String())
	}
	if !*quiet {
		fmt.Printf("\nhollow %s — listening on %s\n\n", version, ln.Addr())
		fmt.Printf("  state    %s\n", dir)
		if loopback != nil {
			fmt.Printf("  guests   %s\n", loopback.Addr())
		}
		if backendErr != nil {
			fmt.Printf("  backend  %s — unavailable: %v\n", b.Name(), backendErr)
			fmt.Printf("           the API is up, but no desk can start here\n")
		} else {
			fmt.Printf("  backend  %s with kvm\n", b.Name())
		}
		for _, img := range images.List() {
			fmt.Printf("  image    %-7s %s\n", img.OS, imageLine(img))
		}
		fmt.Printf("  connect  %s\n", client.Connect{URL: url, Token: token}.Encode())
		if *advertise == "" {
			fmt.Printf("\n  that code names this host as %s. For a client elsewhere, mint one\n  with the address it will use:  hollow connect --url http://HOST:%d\n", url, port)
		}
		fmt.Println()
	}

	errc := make(chan error, 2)
	go func() { errc <- httpSrv.Serve(ln) }()
	if loopback != nil {
		go func() { errc <- httpSrv.Serve(loopback) }()
	}
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	if !*quiet {
		fmt.Println("hollow: stopping desks")
	}
	desks.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func imageLine(img api.Image) string {
	switch img.State {
	case api.ImageReady:
		return fmt.Sprintf("ready (%d MB)", img.Size>>20)
	case api.ImageMissing:
		return "missing — run: hollow pull " + img.OS
	case api.ImageFailed:
		return "failed: " + img.Error + " — run: hollow pull " + img.OS
	default:
		return img.State + ": " + img.Progress
	}
}

// printableAddr turns a listen address into one a client can dial.
func printableAddr(addr string) string {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if h == "" || h == "0.0.0.0" || h == "::" {
		h = "127.0.0.1"
	}
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	return h + ":" + p
}

func cmdConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	url := fs.String("url", "", "the address clients will reach this host at (default: this machine's loopback)")
	state := fs.String("state", host.DefaultDir(), "the host's state directory")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: hollow connect [--url URL]\n\nPrints a connect code for the host on this machine: its address and token in\none string, to paste into HOLLOW_CONNECT wherever bangboo or hollow runs.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	token, err := host.ReadToken(*state)
	if err != nil {
		return fmt.Errorf("no host state in %s (%v); has `hollow serve` run here?", *state, err)
	}
	u := *url
	if u == "" {
		_, p, err := net.SplitHostPort(envOr("HOLLOW_ADDR", defaultAddr))
		if err != nil {
			p = strconv.Itoa(7070)
		}
		u = "http://127.0.0.1:" + p
	}
	fmt.Println(client.Connect{URL: strings.TrimRight(u, "/"), Token: token}.Encode())
	return nil
}
