package agent

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/justin06lee/hollow/api"
)

// Input is done with xdotool rather than XTEST directly. Moving and clicking
// would be easy to do by hand; typing arbitrary text — every keysym, every
// modifier, characters that need a temporary keymap entry — is the part
// xdotool spent years getting right, and one dependency is cheaper than that.
func (s *Server) input(in api.Input) error {
	btn, err := buttonNum(in.Button)
	if err != nil {
		return err
	}
	var args []string
	moveTo := func(x, y *int) error {
		if x == nil && y == nil {
			return nil
		}
		if x == nil || y == nil {
			return errors.New("x and y go together")
		}
		args = append(args, "mousemove", "--sync", strconv.Itoa(*x), strconv.Itoa(*y))
		return nil
	}
	needXY := func() error {
		if in.X == nil || in.Y == nil {
			return fmt.Errorf("%s needs x and y", in.Action)
		}
		return nil
	}
	// Modifiers held around a click: shift+click to extend a selection,
	// ctrl+click to open a link in a new tab.
	var mods []string
	if m := strings.TrimSpace(in.Modifiers); m != "" {
		mods = strings.Split(strings.ReplaceAll(m, " ", "+"), "+")
	}
	click := func(extra ...string) {
		if len(mods) > 0 {
			args = append(args, append([]string{"keydown"}, strings.Join(mods, "+"))...)
		}
		args = append(args, "click")
		args = append(args, extra...)
		args = append(args, btn)
		if len(mods) > 0 {
			args = append(args, "keyup", strings.Join(mods, "+"))
		}
	}

	switch in.Action {
	case api.InputMove:
		if err := needXY(); err != nil {
			return err
		}
		if err := moveTo(in.X, in.Y); err != nil {
			return err
		}
	case api.InputClick:
		if err := moveTo(in.X, in.Y); err != nil {
			return err
		}
		click()
	case api.InputDblClick:
		if err := moveTo(in.X, in.Y); err != nil {
			return err
		}
		click("--repeat", "2", "--delay", "80")
	case api.InputTripleClick:
		if err := moveTo(in.X, in.Y); err != nil {
			return err
		}
		click("--repeat", "3", "--delay", "80")
	case api.InputDown:
		if err := moveTo(in.X, in.Y); err != nil {
			return err
		}
		args = append(args, "mousedown", btn)
	case api.InputUp:
		if err := moveTo(in.X, in.Y); err != nil {
			return err
		}
		args = append(args, "mouseup", btn)
	case api.InputScroll:
		if err := moveTo(in.X, in.Y); err != nil {
			return err
		}
		if in.DX == 0 && in.DY == 0 {
			return errors.New("scroll needs dx or dy")
		}
		if len(mods) > 0 {
			args = append(args, "keydown", strings.Join(mods, "+"))
		}
		if in.DY != 0 {
			b := "5" // wheel down
			if in.DY < 0 {
				b = "4"
			}
			args = append(args, "click", "--repeat", strconv.Itoa(abs(in.DY)), "--delay", "30", b)
		}
		if in.DX != 0 {
			b := "7" // wheel right
			if in.DX < 0 {
				b = "6"
			}
			args = append(args, "click", "--repeat", strconv.Itoa(abs(in.DX)), "--delay", "30", b)
		}
		if len(mods) > 0 {
			args = append(args, "keyup", strings.Join(mods, "+"))
		}
	case api.InputType:
		if in.Text == "" {
			return errors.New("type needs text")
		}
		// --file - takes the text on stdin, so nothing in it is ever mistaken
		// for an option and there is no argument length to run into.
		return s.xdotool([]string{"type", "--clearmodifiers", "--delay", "8", "--file", "-"}, in.Text)
	case api.InputKey:
		if strings.TrimSpace(in.Keys) == "" {
			return errors.New("key needs keys, such as ctrl+l or Return")
		}
		args = append(args, "key", "--clearmodifiers", "--delay", "40")
		args = append(args, strings.Fields(in.Keys)...)
	case api.InputHold:
		keys := strings.Fields(in.Keys)
		if len(keys) == 0 {
			return errors.New("hold needs keys")
		}
		ms := in.DurationMS
		if ms <= 0 {
			ms = 500
		}
		if ms > 30000 {
			ms = 30000
		}
		args = append(args, "keydown")
		args = append(args, keys...)
		args = append(args, "sleep", strconv.FormatFloat(float64(ms)/1000, 'f', 3, 64), "keyup")
		args = append(args, keys...)
	case api.InputDrag:
		if err := needXY(); err != nil {
			return err
		}
		if in.ToX == nil || in.ToY == nil {
			return errors.New("drag needs to_x and to_y")
		}
		if err := moveTo(in.X, in.Y); err != nil {
			return err
		}
		// Moved in two steps: a drag that jumps straight to its end is not
		// recognised as a drag by a good share of toolkits.
		midX, midY := (*in.X+*in.ToX)/2, (*in.Y+*in.ToY)/2
		args = append(args, "mousedown", btn, "sleep", "0.1",
			"mousemove", "--sync", strconv.Itoa(midX), strconv.Itoa(midY), "sleep", "0.1",
			"mousemove", "--sync", strconv.Itoa(*in.ToX), strconv.Itoa(*in.ToY), "sleep", "0.1",
			"mouseup", btn)
	default:
		return fmt.Errorf("unknown input action %q", in.Action)
	}
	return s.xdotool(args, "")
}

func (s *Server) xdotool(args []string, stdin string) error {
	cmd := exec.Command("xdotool", args...)
	cmd.Env = s.env()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("xdotool %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}

func buttonNum(b string) (string, error) {
	switch strings.ToLower(b) {
	case "", "left", "1":
		return "1", nil
	case "middle", "2":
		return "2", nil
	case "right", "3":
		return "3", nil
	}
	return "", fmt.Errorf("unknown button %q: left, middle or right", b)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
