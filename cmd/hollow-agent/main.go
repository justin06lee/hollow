// Command hollow-agent runs inside a desk and does what the host asks.
//
// It is fetched from the host at every boot, so it is never older than the
// hollow that runs it, and the guest image never needs rebuilding for it.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/justin06lee/hollow/internal/agent"
)

// version is stamped by the Makefile.
var version = "dev"

func main() {
	listen := flag.String("listen", "0.0.0.0:7000", "address to answer the host on")
	display := flag.String("display", envOr("DISPLAY", ":0"), "X display to drive")
	envFile := flag.String("env", "/etc/hollow/desk.env", "the desk's configuration, as the host wrote it; holds the key")
	flag.Parse()

	// The key comes from the file the host put on the desk's configuration
	// disk, which every image copies to /etc/hollow at boot. The environment
	// wins, for running the agent by hand.
	key := os.Getenv("HOLLOW_AGENT_KEY")
	if key == "" {
		key = readEnvValue(*envFile, "HOLLOW_AGENT_KEY")
	}
	if key == "" {
		fmt.Fprintf(os.Stderr, "hollow-agent: no key in %s or HOLLOW_AGENT_KEY; refusing to answer anyone\n", *envFile)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           agent.New(*display, version, key).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.SetFlags(log.Ltime)
	log.Printf("hollow-agent %s: display %s, listening on %s", version, *display, *listen)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "hollow-agent:", err)
		os.Exit(1)
	}
}

// readEnvValue reads KEY=value from a shell-style env file.
func readEnvValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && k == key {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
