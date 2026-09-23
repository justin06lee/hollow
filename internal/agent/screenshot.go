package agent

import (
	"fmt"
	"image"
	"net/url"
	"strconv"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
)

// shotOptions are a screenshot request's query parameters.
//
//	x, y, w, h   a region of the screen, in screen pixels (default: all of it)
//	fit=WxH      scale the result down to fit inside WxH, keeping its shape
//	settle=MS    wait up to MS for the screen to stop changing first
//	cursor=0     leave the pointer out
type shotOptions struct {
	region image.Rectangle // zero: whole screen
	fitW   int
	fitH   int
	settle time.Duration
	cursor bool
}

func parseShot(q url.Values) (shotOptions, error) {
	o := shotOptions{cursor: q.Get("cursor") != "0"}
	if q.Get("w") != "" || q.Get("h") != "" {
		var v [4]int
		for i, k := range []string{"x", "y", "w", "h"} {
			n, err := strconv.Atoi(q.Get(k))
			if err != nil {
				return o, fmt.Errorf("region needs x, y, w and h as numbers")
			}
			v[i] = n
		}
		if v[2] <= 0 || v[3] <= 0 {
			return o, fmt.Errorf("region width and height must be positive")
		}
		o.region = image.Rect(v[0], v[1], v[0]+v[2], v[1]+v[3])
	}
	if f := q.Get("fit"); f != "" {
		ws, hs, ok := strings.Cut(f, "x")
		w, err1 := strconv.Atoi(ws)
		h, err2 := strconv.Atoi(hs)
		if !ok || err1 != nil || err2 != nil || w <= 0 || h <= 0 {
			return o, fmt.Errorf("fit wants WxH, not %q", f)
		}
		o.fitW, o.fitH = w, h
	}
	if s := q.Get("settle"); s != "" {
		ms, err := strconv.Atoi(s)
		if err != nil || ms < 0 {
			return o, fmt.Errorf("settle wants milliseconds")
		}
		if ms > 10000 {
			ms = 10000
		}
		o.settle = time.Duration(ms) * time.Millisecond
	}
	return o, nil
}

// shoot captures the screen as asked: after it settles, cropped, scaled.
// It also returns the full screen's size, which a caller needs to map
// coordinates in a scaled or cropped image back onto the screen.
func (s *Server) shoot(o shotOptions) (image.Image, image.Point, error) {
	img, err := s.disp.Capture(o.cursor)
	if err != nil {
		return nil, image.Point{}, err
	}
	if o.settle > 0 {
		img, err = s.settle(img, o)
		if err != nil {
			return nil, image.Point{}, err
		}
	}
	screen := img.Bounds().Size()
	var out image.Image = img
	if !o.region.Empty() {
		r := o.region.Intersect(img.Bounds())
		if r.Empty() {
			return nil, screen, fmt.Errorf("region %v is off the %dx%d screen", o.region, screen.X, screen.Y)
		}
		out = img.SubImage(r)
	}
	if o.fitW > 0 {
		out = fit(out, o.fitW, o.fitH)
	}
	return out, screen, nil
}

// settle waits until two captures in a row are all but identical, so that a
// screenshot taken just after a click shows where the click led rather than
// the frame before. "All but" because a blinking text cursor would otherwise
// keep a finished screen looking busy forever.
func (s *Server) settle(first *image.RGBA, o shotOptions) (*image.RGBA, error) {
	deadline := time.Now().Add(o.settle)
	prev := first
	for time.Now().Before(deadline) {
		time.Sleep(120 * time.Millisecond)
		cur, err := s.disp.Capture(o.cursor)
		if err != nil {
			return nil, err
		}
		if changedFraction(prev, cur) < 0.0015 {
			return cur, nil
		}
		prev = cur
	}
	return prev, nil
}

func changedFraction(a, b *image.RGBA) float64 {
	if a.Bounds() != b.Bounds() {
		return 1
	}
	pa, pb := a.Pix, b.Pix
	changed := 0
	for i := 0; i+3 < len(pa); i += 4 {
		if pa[i] != pb[i] || pa[i+1] != pb[i+1] || pa[i+2] != pb[i+2] {
			changed++
		}
	}
	return float64(changed) / float64(len(pa)/4)
}

// fit scales img down to fit inside w×h, or up when it is smaller — which is
// what a zoomed-in region wants: a small area made big enough to read.
func fit(img image.Image, w, h int) image.Image {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	scale := float64(w) / float64(sw)
	if s := float64(h) / float64(sh); s < scale {
		scale = s
	}
	if scale > 4 {
		scale = 4
	}
	tw, th := int(float64(sw)*scale+0.5), int(float64(sh)*scale+0.5)
	if tw == sw && th == sh {
		return img
	}
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Src, nil)
	return dst
}
