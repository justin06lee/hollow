package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const defaultFPS = 10

// recorder is one ffmpeg capturing the display, at most one at a time.
type recorder struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	path string
	done chan error
}

func (r *recorder) start(displayName string, w, h, fps int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil {
		return errors.New("already recording; stop that one first")
	}
	if fps <= 0 {
		fps = defaultFPS
	}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("hollow-rec-%d.mp4", time.Now().UnixNano()))
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error", "-nostdin",
		"-f", "x11grab", "-framerate", strconv.Itoa(fps),
		"-video_size", fmt.Sprintf("%dx%d", w, h), "-draw_mouse", "1",
		"-i", displayName,
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		path)
	log, _ := os.Create(path + ".log")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		if log != nil {
			log.Close()
		}
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		if log != nil {
			log.Close()
		}
	}()
	// ffmpeg that dies at once — no such display, no encoder — should be
	// reported now, not when the recording is stopped.
	select {
	case err := <-done:
		msg, _ := os.ReadFile(path + ".log")
		os.Remove(path)
		os.Remove(path + ".log")
		return fmt.Errorf("ffmpeg exited at once: %v: %s", err, string(msg))
	case <-time.After(500 * time.Millisecond):
	}
	r.cmd, r.path, r.done = cmd, path, done
	return nil
}

// stop ends the recording and returns the finished file's path. The caller
// removes it once it has been sent.
func (r *recorder) stop() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd == nil {
		return "", errors.New("not recording")
	}
	cmd, path, done := r.cmd, r.path, r.done
	r.cmd, r.path, r.done = nil, "", nil

	// SIGINT is how ffmpeg is asked to finish: it writes the trailer and
	// exits. SIGKILL would leave an unplayable file.
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return "", errors.New("ffmpeg did not finish the file in time")
	}
	os.Remove(path + ".log")
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		os.Remove(path)
		return "", errors.New("the recording is empty")
	}
	return path, nil
}

func (r *recorder) recording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmd != nil
}
