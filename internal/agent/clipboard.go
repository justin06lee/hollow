package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// The clipboard is xclip's, because an X selection is owned by a running
// client: something has to stay alive holding the text after it is set, and
// xclip forks off to do exactly that.

func (s *Server) clipboardGet() (string, error) {
	if _, err := exec.LookPath("xclip"); err != nil {
		return "", errNoXclip
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-o")
	cmd.Env = s.env()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		// An empty clipboard is not an error worth reporting as one.
		if strings.Contains(msg, "target STRING not available") || strings.Contains(msg, "target UTF8_STRING not available") {
			return "", nil
		}
		return "", fmt.Errorf("xclip: %s", msg)
	}
	return out.String(), nil
}

func (s *Server) clipboardSet(text string) error {
	if _, err := exec.LookPath("xclip"); err != nil {
		return errNoXclip
	}
	cmd := exec.Command("xclip", "-selection", "clipboard", "-i")
	cmd.Env = s.env()
	cmd.Stdin = strings.NewReader(text)
	// No pipes on stdout or stderr: xclip forks a child that keeps the
	// selection, and that child would hold a pipe open — and this call —
	// until somebody else copied something.
	cmd.Stdout, cmd.Stderr = nil, nil
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("xclip: %w", err)
	}
	return nil
}

var errNoXclip = errors.New("this desk has no xclip: its image predates clipboard support — pull the image again (hollow pull linux, or phaethon sync)")
