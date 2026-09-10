// Command hollow-agent runs inside a desk and does what the host asks.
//
// It is fetched from the host at every boot, so it is never older than the
// hollow that runs it, and the guest image never needs rebuilding for it.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/justin06lee/hollow/internal/agent"
)

// version is stamped by the Makefile.
var version = "dev"

func main() {
	listen := flag.String("listen", "0.0.0.0:7000", "address to answer the host on")
	display := flag.String("display", envOr("DISPLAY", ":0"), "X display to drive")
	flag.Parse()

	srv := &http.Server{
		Addr:              *listen,
		Handler:           agent.New(*display, version).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.SetFlags(log.Ltime)
	log.Printf("hollow-agent %s: display %s, listening on %s", version, *display, *listen)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "hollow-agent:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
