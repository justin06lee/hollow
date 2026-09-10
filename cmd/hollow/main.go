// Command hollow gives bots computers.
//
// `hollow serve` runs the host: it builds a golden image per guest OS, boots
// a small VM — a desk — for every bot that asks, and answers an HTTP API for
// seeing and driving them. Every other subcommand is a client of that API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/justin06lee/hollow/client"
)

// version is stamped at build time by the Makefile. A `go install` build has
// no ldflags, so it reads its version from the module's build info instead.
var version = ""

func resolveVersion() string {
	if version != "" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			return s.Value[:7]
		}
	}
	return "dev"
}

const usage = `hollow — computers for bots.

usage:
  hollow serve    [flags]                 run the host: images, desks, the API
  hollow status                           what the host has
  hollow pull     [linux]                 build the golden image; once, a few minutes
  hollow new      [flags]                 boot a desk and wait until it can be driven
  hollow ls                               running desks
  hollow rm       ID...                   stop desks
  hollow shot     ID [FILE]               screenshot as PNG (default shot-ID-TIME.png)
  hollow click    ID [X Y] [flags]        click; --right, --middle, --double
  hollow move     ID X Y                  move the pointer
  hollow scroll   ID N [flags]            scroll down N notches (negative: up)
  hollow drag     ID X Y X2 Y2            drag with the left button
  hollow type     ID TEXT...              type text
  hollow key      ID KEYS...              press keys: ctrl+l, Return, alt+F4
  hollow exec     ID [flags] -- CMD...    run a program on the desk
  hollow put      ID LOCAL REMOTE         copy a file onto the desk
  hollow get      ID REMOTE LOCAL         copy a file off the desk
  hollow rec      ID start|stop [FILE]    record the screen; stop writes an MP4
  hollow health   ID                      the desk's display and agent
  hollow logs     ID                      the desk's console
  hollow connect  [--url URL]             print a connect code for this host
  hollow version

A client finds its host from, in order: --connect CODE, HOLLOW_CONNECT,
HOLLOW_URL with HOLLOW_TOKEN, or the token of a host on this machine
(HOLLOW_HOME; /var/lib/hollow as root, ~/.local/share/hollow otherwise)
at http://127.0.0.1:7070.

run "hollow <command> -h" for the rest.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	version = resolveVersion()
	client.UserAgent = "hollow/" + version

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "serve", "host":
		err = cmdServe(ctx, args)
	case "status":
		err = cmdStatus(ctx, args)
	case "pull":
		err = cmdPull(ctx, args)
	case "new", "create":
		err = cmdNew(ctx, args)
	case "ls", "list", "desks":
		err = cmdLs(ctx, args)
	case "rm", "stop", "delete":
		err = cmdRm(ctx, args)
	case "shot", "screenshot":
		err = cmdShot(ctx, args)
	case "click":
		err = cmdClick(ctx, args)
	case "move":
		err = cmdMove(ctx, args)
	case "scroll":
		err = cmdScroll(ctx, args)
	case "drag":
		err = cmdDrag(ctx, args)
	case "type":
		err = cmdType(ctx, args)
	case "key", "keys":
		err = cmdKey(ctx, args)
	case "exec", "run":
		err = cmdExec(ctx, args)
	case "put":
		err = cmdPut(ctx, args)
	case "get":
		err = cmdGet(ctx, args)
	case "rec", "record":
		err = cmdRec(ctx, args)
	case "health":
		err = cmdHealth(ctx, args)
	case "logs", "log":
		err = cmdLogs(ctx, args)
	case "connect":
		err = cmdConnect(args)
	case "version", "-v", "--version":
		fmt.Println("hollow", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "hollow: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		var exit exitError
		if errors.As(err, &exit) {
			os.Exit(int(exit))
		}
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "hollow:", err)
		os.Exit(1)
	}
}

// exitError carries a program's exit code out of `hollow exec` unchanged.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
