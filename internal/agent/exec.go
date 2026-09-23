package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/justin06lee/hollow/api"
)

const (
	defaultExecTimeout = 60 * time.Second
	maxExecTimeout     = 10 * time.Minute
	maxOutput          = 4 << 20 // per stream; more than this is a file, not a result
)

func (s *Server) exec(req api.Exec) (api.ExecResult, error) {
	if req.Cmd == "" {
		return api.ExecResult{}, errors.New("cmd is required")
	}
	var name string
	var args []string
	if req.Shell {
		name = "/bin/sh"
		args = append([]string{"-c", req.Cmd, "sh"}, req.Args...)
	} else {
		name, args = req.Cmd, req.Args
	}

	if req.Detach {
		cmd := exec.Command(name, args...)
		s.prepare(cmd, req)
		cmd.Stdin = nil
		null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			return api.ExecResult{}, err
		}
		defer null.Close()
		cmd.Stdout, cmd.Stderr = null, null
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return api.ExecResult{}, err
		}
		pid := cmd.Process.Pid
		go cmd.Wait() // reap it, so it never lingers as a zombie
		return api.ExecResult{PID: pid}, nil
	}

	timeout := defaultExecTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if timeout > maxExecTimeout {
		timeout = maxExecTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	s.prepare(cmd, req)
	if req.Stdin != "" {
		cmd.Stdin = bytes.NewReader([]byte(req.Stdin))
	}
	var stdout, stderr limitedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A child that has spawned its own children would otherwise keep the
	// pipes open, and Wait, forever.
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	err := cmd.Run()
	res := api.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.Code = -1
		return res, nil
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		res.Code = 0
	case errors.As(err, &exit):
		res.Code = exit.ExitCode()
	default:
		return res, fmt.Errorf("start %s: %w", name, err)
	}
	return res, nil
}

func (s *Server) prepare(cmd *exec.Cmd, req api.Exec) {
	cmd.Env = append(s.env(), req.Env...)
	cmd.Dir = req.Dir
	if cmd.Dir == "" {
		cmd.Dir = s.home
	}
}

// env is the environment every program on the desk runs with: the agent's
// own, plus the display, so a bot's `chromium` opens on the screen it can see.
func (s *Server) env() []string {
	env := os.Environ()
	env = append(env, "DISPLAY="+s.disp.name)
	if s.home != "" {
		env = append(env, "HOME="+s.home)
	}
	return env
}

type limitedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := maxOutput - b.buf.Len()
	if room <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string {
	if b.truncated {
		return b.buf.String() + "\n[output truncated]"
	}
	return b.buf.String()
}

func lookPath(name string) (string, error) { return exec.LookPath(name) }
