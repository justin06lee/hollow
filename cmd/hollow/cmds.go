package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/justin06lee/hollow/api"
	"github.com/justin06lee/hollow/client"
	"github.com/justin06lee/hollow/internal/host"
	"github.com/justin06lee/hollow/secrets"
)

// connectFlag is shared by every client command.
var connectFlag string

func newFlags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&connectFlag, "connect", "", "connect code for the host (env HOLLOW_CONNECT)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hollow %s\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// dial finds the host. See the usage text for the order.
func dial() (*client.Client, error) {
	if code := envOr("HOLLOW_CONNECT", connectFlag); code != "" {
		if connectFlag != "" {
			code = connectFlag
		}
		return client.FromConnect(code)
	}
	if u := os.Getenv("HOLLOW_URL"); u != "" {
		tok := os.Getenv("HOLLOW_TOKEN")
		if tok == "" {
			return nil, errors.New("HOLLOW_URL is set but HOLLOW_TOKEN is not")
		}
		return client.NewMulti(splitList(u), tok), nil
	}
	dir := host.DefaultDir()
	tok, err := host.ReadToken(dir)
	if err != nil {
		return nil, fmt.Errorf("no host to talk to: set HOLLOW_CONNECT, or run `hollow serve` here (no token in %s)", dir)
	}
	return client.New(fmt.Sprintf("http://127.0.0.1:%d", defaultPort()), tok), nil
}

// parse reads flags wherever they are among the arguments — `hollow read ID
// --json` as well as `hollow read --json ID` — which is what people and
// agents both type. Everything after a "--" is taken as it is.
func parse(fs *flag.FlagSet, args []string) error {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			pos = append(pos, rest...)
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	return fs.Parse(append([]string{"--"}, pos...))
}

func need(fs *flag.FlagSet, n int, what string) error {
	if fs.NArg() < n {
		return fmt.Errorf("usage: hollow %s %s", fs.Name(), what)
	}
	return nil
}

func atoi(s, what string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", what, s)
	}
	return n, nil
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := newFlags("status", "status")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("hollow %s on %s/%s, backend %s, %d desk(s)\n", st.Version, st.OS, st.Arch, st.Backend, st.Desks)
	for _, img := range st.Images {
		fmt.Printf("  image %-7s %s\n", img.OS, imageLine(img))
	}
	return nil
}

func cmdPull(ctx context.Context, args []string) error {
	fs := newFlags("pull", "pull [linux]")
	if err := parse(fs, args); err != nil {
		return err
	}
	osName := "linux"
	if fs.NArg() > 0 {
		osName = fs.Arg(0)
	}
	c, err := dial()
	if err != nil {
		return err
	}
	img, err := c.PullAndWait(ctx, osName, func(img api.Image) {
		fmt.Fprintf(os.Stderr, "\r\033[K  %-13s %s", img.State, img.Progress)
	})
	fmt.Fprint(os.Stderr, "\r\033[K")
	if err != nil {
		return err
	}
	fmt.Printf("%s image ready (%d MB)\n", osName, img.Size>>20)
	return nil
}

func cmdNew(ctx context.Context, args []string) error {
	fs := newFlags("new", "new [flags]")
	osName := fs.String("os", "linux", "guest os")
	name := fs.String("name", "", "a label for the desk")
	mem := fs.Int("mem", 0, "memory in MB (default 768)")
	cpus := fs.Int("cpus", 0, "virtual cpus (default 2)")
	size := fs.String("size", "", "screen size, WxH (default 1280x800)")
	noWait := fs.Bool("no-wait", false, "return as soon as the VM starts, before the desk can be driven")
	if err := parse(fs, args); err != nil {
		return err
	}
	spec := api.DeskSpec{OS: *osName, Name: *name, MemMB: *mem, CPUs: *cpus}
	if *size != "" {
		w, h, ok := strings.Cut(*size, "x")
		if !ok {
			return fmt.Errorf("--size wants WxH, not %q", *size)
		}
		var err error
		if spec.Width, err = atoi(w, "width"); err != nil {
			return err
		}
		if spec.Height, err = atoi(h, "height"); err != nil {
			return err
		}
	}
	c, err := dial()
	if err != nil {
		return err
	}
	d, err := c.Create(ctx, spec)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "desk %s booting (%s, %d MB, %d cpu, %dx%d)\n", d.ID, d.OS, d.MemMB, d.CPUs, d.Width, d.Height)
	if !*noWait {
		start := time.Now()
		if d, err = c.Wait(ctx, d.ID); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "desk %s ready in %s\n", d.ID, time.Since(start).Round(100*time.Millisecond))
	}
	fmt.Println(d.ID)
	return nil
}

func cmdLs(ctx context.Context, args []string) error {
	fs := newFlags("ls", "ls")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	desks, err := c.Desks(ctx)
	if err != nil {
		return err
	}
	if len(desks) == 0 {
		fmt.Fprintln(os.Stderr, "no desks; start one with: hollow new")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tOS\tSTATE\tMEM\tSCREEN\tAGE\tIDLE")
	for _, d := range desks {
		state := d.State
		if d.Error != "" {
			state += " (" + d.Error + ")"
		}
		if d.Paused {
			state += ", paused"
		}
		if d.Watchers > 0 {
			state += fmt.Sprintf(", %d watching", d.Watchers)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%dM\t%dx%d\t%s\t%s\n", d.ID, d.Name, d.OS, state, d.MemMB, d.Width, d.Height,
			time.Since(d.Created).Round(time.Second), time.Since(d.LastUsed).Round(time.Second))
	}
	return tw.Flush()
}

func cmdRm(ctx context.Context, args []string) error {
	fs := newFlags("rm", "rm ID...")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 1, "ID..."); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	for _, id := range fs.Args() {
		if err := c.Delete(ctx, id); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "desk %s stopped\n", id)
	}
	return nil
}

func cmdShot(ctx context.Context, args []string) error {
	fs := newFlags("shot", "shot ID [FILE]")
	jpeg := fs.Bool("jpeg", false, "JPEG instead of PNG")
	quality := fs.Int("quality", 80, "JPEG quality")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 1, "ID [FILE]"); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	id := fs.Arg(0)
	var data []byte
	ext := "png"
	if *jpeg {
		ext = "jpg"
		data, err = c.ScreenshotJPEG(ctx, id, *quality)
	} else {
		data, err = c.Screenshot(ctx, id)
	}
	if err != nil {
		return err
	}
	out := fs.Arg(1)
	if out == "" {
		out = fmt.Sprintf("shot-%s-%s.%s", id, time.Now().Format("150405"), ext)
	}
	if out == "-" {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

func ptr(n int) *int { return &n }

func cmdClick(ctx context.Context, args []string) error {
	fs := newFlags("click", "click ID [X Y] [flags]")
	right := fs.Bool("right", false, "right button")
	middle := fs.Bool("middle", false, "middle button")
	double := fs.Bool("double", false, "double-click")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 1, "ID [X Y]"); err != nil {
		return err
	}
	in := api.Input{Action: api.InputClick}
	if *double {
		in.Action = api.InputDblClick
	}
	switch {
	case *right:
		in.Button = "right"
	case *middle:
		in.Button = "middle"
	}
	if fs.NArg() >= 3 {
		x, err := atoi(fs.Arg(1), "x")
		if err != nil {
			return err
		}
		y, err := atoi(fs.Arg(2), "y")
		if err != nil {
			return err
		}
		in.X, in.Y = ptr(x), ptr(y)
	}
	return input(ctx, fs.Arg(0), in)
}

func cmdMove(ctx context.Context, args []string) error {
	fs := newFlags("move", "move ID X Y")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 3, "ID X Y"); err != nil {
		return err
	}
	x, err := atoi(fs.Arg(1), "x")
	if err != nil {
		return err
	}
	y, err := atoi(fs.Arg(2), "y")
	if err != nil {
		return err
	}
	return input(ctx, fs.Arg(0), api.Input{Action: api.InputMove, X: ptr(x), Y: ptr(y)})
}

func cmdScroll(ctx context.Context, args []string) error {
	fs := newFlags("scroll", "scroll ID N [flags]")
	x := fs.Int("x", -1, "pointer x before scrolling")
	y := fs.Int("y", -1, "pointer y before scrolling")
	dx := fs.Int("dx", 0, "horizontal notches (positive: right)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 2, "ID N"); err != nil {
		return err
	}
	n, err := atoi(fs.Arg(1), "n")
	if err != nil {
		return err
	}
	in := api.Input{Action: api.InputScroll, DY: n, DX: *dx}
	if *x >= 0 && *y >= 0 {
		in.X, in.Y = x, y
	}
	return input(ctx, fs.Arg(0), in)
}

func cmdDrag(ctx context.Context, args []string) error {
	fs := newFlags("drag", "drag ID X Y X2 Y2")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 5, "ID X Y X2 Y2"); err != nil {
		return err
	}
	var v [4]int
	for i := range v {
		n, err := atoi(fs.Arg(i+1), "coordinate")
		if err != nil {
			return err
		}
		v[i] = n
	}
	return input(ctx, fs.Arg(0), api.Input{Action: api.InputDrag, X: ptr(v[0]), Y: ptr(v[1]), ToX: ptr(v[2]), ToY: ptr(v[3])})
}

func cmdType(ctx context.Context, args []string) error {
	fs := newFlags("type", "type ID TEXT...")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 2, "ID TEXT..."); err != nil {
		return err
	}
	return input(ctx, fs.Arg(0), api.Input{Action: api.InputType, Text: strings.Join(fs.Args()[1:], " ")})
}

func cmdKey(ctx context.Context, args []string) error {
	fs := newFlags("key", "key ID KEYS...")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 2, "ID KEYS..."); err != nil {
		return err
	}
	return input(ctx, fs.Arg(0), api.Input{Action: api.InputKey, Keys: strings.Join(fs.Args()[1:], " ")})
}

func input(ctx context.Context, id string, in api.Input) error {
	c, err := dial()
	if err != nil {
		return err
	}
	return c.Input(ctx, id, in)
}

func cmdExec(ctx context.Context, args []string) error {
	fs := newFlags("exec", "exec ID [flags] -- CMD [ARGS...]")
	shell := fs.Bool("shell", false, "run CMD through /bin/sh -c")
	detach := fs.Bool("detach", false, "start it and return at once, printing its pid")
	timeout := fs.Duration("timeout", 60*time.Second, "give up after this long")
	dir := fs.String("dir", "", "working directory on the desk")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: hollow exec ID [flags] -- CMD [ARGS...]")
	}
	id := fs.Arg(0)
	rest := fs.Args()[1:]
	if len(rest) == 0 {
		return errors.New("nothing to run: hollow exec ID -- CMD [ARGS...]")
	}
	req := api.Exec{Cmd: rest[0], Args: rest[1:], Shell: *shell, Detach: *detach, Dir: *dir, TimeoutMS: int(timeout.Milliseconds())}
	if *shell {
		req.Cmd = strings.Join(rest, " ")
		req.Args = nil
	}
	c, err := dial()
	if err != nil {
		return err
	}
	res, err := c.Exec(ctx, id, req)
	if err != nil {
		return err
	}
	if *detach {
		fmt.Println(res.PID)
		return nil
	}
	os.Stdout.WriteString(res.Stdout)
	os.Stderr.WriteString(res.Stderr)
	if res.TimedOut {
		return fmt.Errorf("timed out after %s", *timeout)
	}
	if res.Code != 0 {
		return exitError(res.Code)
	}
	return nil
}

func cmdPut(ctx context.Context, args []string) error {
	fs := newFlags("put", "put ID LOCAL REMOTE")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 3, "ID LOCAL REMOTE"); err != nil {
		return err
	}
	f, err := os.Open(fs.Arg(1))
	if err != nil {
		return err
	}
	defer f.Close()
	c, err := dial()
	if err != nil {
		return err
	}
	return c.PutFile(ctx, fs.Arg(0), fs.Arg(2), f)
}

func cmdGet(ctx context.Context, args []string) error {
	fs := newFlags("get", "get ID REMOTE LOCAL")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 3, "ID REMOTE LOCAL"); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	r, err := c.GetFile(ctx, fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	defer r.Close()
	var w io.Writer = os.Stdout
	if fs.Arg(2) != "-" {
		f, err := os.Create(fs.Arg(2))
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	_, err = io.Copy(w, r)
	return err
}

func cmdRec(ctx context.Context, args []string) error {
	fs := newFlags("rec", "rec ID start|stop [FILE]")
	fps := fs.Int("fps", 10, "frames per second, for start")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 2, "ID start|stop [FILE]"); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	id := fs.Arg(0)
	switch fs.Arg(1) {
	case "start":
		if err := c.RecordStart(ctx, id, *fps); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "recording desk %s at %d fps; stop with: hollow rec %s stop FILE.mp4\n", id, *fps, id)
		return nil
	case "stop":
		data, err := c.RecordStop(ctx, id)
		if err != nil {
			return err
		}
		out := fs.Arg(2)
		if out == "" {
			out = fmt.Sprintf("rec-%s-%s.mp4", id, time.Now().Format("150405"))
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	}
	return fmt.Errorf("rec wants start or stop, not %q", fs.Arg(1))
}

func cmdHealth(ctx context.Context, args []string) error {
	fs := newFlags("health", "health ID")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 1, "ID"); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	h, err := c.Health(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("display %s, %dx%d, agent %s, up %s\n", h.Display, h.Width, h.Height, h.Agent, (time.Duration(h.Uptime) * time.Second).String())
	return nil
}

func cmdLogs(ctx context.Context, args []string) error {
	fs := newFlags("logs", "logs ID")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 1, "ID"); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	out, err := c.Logs(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func cmdOpen(ctx context.Context, args []string) error {
	fs := newFlags("open", "open ID URL")
	newTab := fs.Bool("tab", false, "in a new tab")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 2, "ID URL"); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	res, err := c.BrowserOpen(ctx, fs.Arg(0), api.BrowserOpen{URL: fs.Arg(1), NewTab: *newTab})
	if err != nil {
		return err
	}
	fmt.Println(res.URL)
	return nil
}

func cmdRead(ctx context.Context, args []string) error {
	fs := newFlags("read", "read ID")
	offset := fs.Int("offset", 0, "start this many characters into the page text")
	max := fs.Int("max", 0, "at most this many characters (default 6000)")
	asJSON := fs.Bool("json", false, "print the whole state as JSON")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 1, "ID"); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	st, err := c.BrowserRead(ctx, fs.Arg(0), api.BrowserRead{Offset: *offset, MaxChars: *max})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	fmt.Printf("%s\n%s\n\n%s\n", st.Title, st.URL, st.Text)
	if end := st.Offset + len([]rune(st.Text)); end < st.TotalChars {
		fmt.Printf("\n[%d of %d characters; --offset %d for more]\n", end, st.TotalChars, end)
	}
	fmt.Println()
	for _, e := range st.Elements {
		where := ""
		if !e.Visible {
			where = " (off screen)"
		}
		fmt.Printf("[%d] %s %q%s\n", e.Index, e.Kind, e.Label, where)
	}
	return nil
}

func cmdWindows(ctx context.Context, args []string) error {
	fs := newFlags("windows", "windows ID [activate|close WINDOW]")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 1, "ID"); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	if fs.NArg() >= 3 {
		return c.WindowAction(ctx, fs.Arg(0), api.WindowAction{Action: fs.Arg(1), ID: fs.Arg(2)})
	}
	list, err := c.Windows(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "WINDOW\tCLASS\tGEOMETRY\tTITLE")
	for _, w := range list {
		mark := ""
		if w.Active {
			mark = " *"
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%dx%d+%d+%d\t%s\n", w.ID, mark, w.Class, w.Width, w.Height, w.X, w.Y, w.Title)
	}
	return tw.Flush()
}

func cmdClip(ctx context.Context, args []string) error {
	fs := newFlags("clip", "clip ID [TEXT...]")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 1, "ID [TEXT...]"); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return c.SetClipboard(ctx, fs.Arg(0), strings.Join(fs.Args()[1:], " "))
	}
	text, err := c.Clipboard(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Print(text)
	return nil
}

func cmdSecret(ctx context.Context, args []string) error {
	// --connect may come anywhere; take it out before the subcommand sees
	// the rest.
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--connect" || a == "-connect":
			if i+1 < len(args) {
				connectFlag = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--connect=") || strings.HasPrefix(a, "-connect="):
			_, connectFlag, _ = strings.Cut(a, "=")
		default:
			rest = append(rest, a)
		}
	}
	return secrets.Main(ctx, secrets.Env{
		Prog: "hollow",
		Hosts: func(ctx context.Context, _ string) ([]secrets.Host, error) {
			c, err := dial()
			if err != nil {
				return nil, err
			}
			name := c.Name
			if name == "" {
				name = "host"
				if st, err := c.Status(ctx); err == nil {
					name = st.Name
				}
			}
			return []secrets.Host{{Name: name, Client: c}}, nil
		},
	}, rest)
}

func cmdView(ctx context.Context, args []string) error {
	fs := newFlags("view", "view [ID] [--print]")
	printOnly := fs.Bool("print", false, "print the link instead of opening it")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	u, err := c.ViewURL(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if *printOnly || !openBrowser(u) {
		fmt.Println(u)
		fmt.Fprintln(os.Stderr, "open it in a browser on a machine that reaches the host; it works once, within 15 minutes")
	}
	return nil
}

// openBrowser opens a URL in this machine's browser, when it has one.
func openBrowser(u string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return false
		}
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start() == nil
}

func cmdPause(ctx context.Context, args []string, paused bool) error {
	name := "pause"
	if !paused {
		name = "resume"
	}
	fs := newFlags(name, name+" ID")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(fs, 1, "ID"); err != nil {
		return err
	}
	c, err := dial()
	if err != nil {
		return err
	}
	d, err := c.Pause(ctx, fs.Arg(0), paused)
	if err != nil {
		return err
	}
	if d.Paused {
		fmt.Printf("%s: paused; agents wait until you run hollow resume %s\n", d.Name, d.Name)
	} else {
		fmt.Printf("%s: agents have it back\n", d.Name)
	}
	return nil
}
