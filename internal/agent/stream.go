package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/justin06lee/hollow/api"
	xdraw "golang.org/x/image/draw"
)

// The stream is the live view: the screen as a run of JPEG frames over a
// WebSocket, and a watcher's pointer and keyboard coming back the other way.
//
// A frame is sent only when the screen changed, and the next is captured
// only once the last has gone out, so a slow link gets fewer frames rather
// than a growing queue of stale ones.

const (
	defaultStreamFPS     = 12
	maxStreamFPS         = 30
	defaultStreamQuality = 70
	// An unchanged screen is sent again this often anyway, so a viewer that
	// missed a frame is never wrong for long.
	streamRefresh = 5 * time.Second
	// Pasted text longer than this is put on the desk's clipboard rather
	// than typed: typing it would take longer than anyone would wait.
	maxPasteTyped = 2000
)

func queryInt(r *http.Request, key string, def, lo, hi int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	fps := queryInt(r, "fps", defaultStreamFPS, 1, maxStreamFPS)
	quality := queryInt(r, "quality", defaultStreamQuality, 10, 95)
	maxW := queryInt(r, "max_w", 0, 0, 7680)
	viewOnly := r.URL.Query().Get("input") == "0"

	// Whoever got this far holds the desk's key, which only the host has;
	// where the page came from is the host's to check, not ours.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var wmu sync.Mutex
	send := func(typ websocket.MessageType, data []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		wctx, done := context.WithTimeout(ctx, 15*time.Second)
		defer done()
		return c.Write(wctx, typ, data)
	}
	sendJSON := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return send(websocket.MessageText, b)
	}

	go func() {
		defer cancel()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var in api.StreamInput
			if json.Unmarshal(data, &in) != nil {
				continue
			}
			if viewOnly {
				continue
			}
			if err := s.streamInput(in); err != nil {
				_ = sendJSON(map[string]string{"type": "error", "error": err.Error()})
			}
		}
	}()

	tick := time.NewTicker(time.Second / time.Duration(fps))
	defer tick.Stop()
	var (
		hello    api.StreamHello
		last     []byte
		lastSent time.Time
		buf      bytes.Buffer
		scaled   *image.RGBA
	)
	for {
		select {
		case <-ctx.Done():
			c.Close(websocket.StatusNormalClosure, "")
			return
		case <-tick.C:
		}
		img, err := s.disp.Capture(true)
		if err != nil {
			continue // the display is restarting; try on the next tick
		}
		b := img.Bounds()
		fw, fh := b.Dx(), b.Dy()
		if maxW > 0 && fw > maxW {
			fw, fh = maxW, fh*maxW/fw
		}
		if hello.ScreenW != b.Dx() || hello.ScreenH != b.Dy() || hello.FrameW != fw || hello.FrameH != fh {
			hello = api.StreamHello{Type: "hello", ScreenW: b.Dx(), ScreenH: b.Dy(), FrameW: fw, FrameH: fh}
			if sendJSON(hello) != nil {
				return
			}
			last, scaled = nil, nil
		}
		if last != nil && bytes.Equal(last, img.Pix) && time.Since(lastSent) < streamRefresh {
			continue
		}
		last = append(last[:0], img.Pix...)
		var frame image.Image = img
		if fw != b.Dx() {
			if scaled == nil {
				scaled = image.NewRGBA(image.Rect(0, 0, fw, fh))
			}
			xdraw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), img, b, xdraw.Src, nil)
			frame = scaled
		}
		buf.Reset()
		if err := jpeg.Encode(&buf, frame, &jpeg.Options{Quality: quality}); err != nil {
			continue
		}
		if send(websocket.MessageBinary, buf.Bytes()) != nil {
			return
		}
		lastSent = time.Now()
	}
}

// streamInput does what a watcher did.
func (s *Server) streamInput(in api.StreamInput) error {
	button := func() int {
		if in.B >= 1 && in.B <= 3 {
			return in.B
		}
		return 1
	}
	switch in.T {
	case "move":
		return s.disp.FakeMotion(in.X, in.Y)
	case "down", "up":
		if err := s.disp.FakeMotion(in.X, in.Y); err != nil {
			return err
		}
		return s.disp.FakeButton(button(), in.T == "down")
	case "wheel":
		if err := s.disp.FakeMotion(in.X, in.Y); err != nil {
			return err
		}
		for _, axis := range []struct{ n, neg, pos int }{{in.DY, 4, 5}, {in.DX, 6, 7}} {
			b := axis.pos
			if axis.n < 0 {
				b = axis.neg
			}
			for i := 0; i < abs(axis.n) && i < 20; i++ {
				if err := s.disp.FakeButton(b, true); err != nil {
					return err
				}
				if err := s.disp.FakeButton(b, false); err != nil {
					return err
				}
			}
		}
		return nil
	case "key":
		return s.input(api.Input{Action: api.InputKey, Keys: in.Keys})
	case "type":
		if in.Text == "" {
			return nil
		}
		return s.input(api.Input{Action: api.InputType, Text: in.Text})
	case "paste":
		if in.Text == "" {
			return nil
		}
		// On the clipboard as well, so ctrl+v works where typing does not.
		clipErr := s.clipboardSet(in.Text)
		if len(in.Text) > maxPasteTyped {
			if clipErr != nil {
				return clipErr
			}
			return errors.New("that is long to type, so it is on the desk's clipboard: press ctrl+v (ctrl+shift+v in a terminal)")
		}
		return s.input(api.Input{Action: api.InputType, Text: in.Text})
	}
	return errors.New("unknown input " + strconv.Quote(in.T))
}
