package agent

import (
	"fmt"
	"image"
	"image/draw"
	"io"
	"log"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

// display is one connection to the X server, made lazily and remade if it
// breaks. The agent starts before Xvfb has finished coming up, and a display
// restarted underneath us should not take the agent down with it.
type display struct {
	name string

	mu     sync.Mutex
	conn   *xgb.Conn
	screen *xproto.ScreenInfo
	xfixes bool
}

func init() {
	// xgb narrates its search for an .Xauthority file on every connection.
	// Xvfb here runs with access control off, so there is nothing to find,
	// and nothing to say about it.
	xgb.Logger = log.New(io.Discard, "", 0)
}

func (d *display) connect() error {
	if d.conn != nil {
		return nil
	}
	c, err := xgb.NewConnDisplay(d.name)
	if err != nil {
		return err
	}
	d.conn = c
	d.screen = xproto.Setup(c).DefaultScreen(c)
	d.xfixes = false
	if err := xfixes.Init(c); err == nil {
		if _, err := xfixes.QueryVersion(c, 5, 0).Reply(); err == nil {
			d.xfixes = true
		}
	}
	return nil
}

func (d *display) drop() {
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

// Size is the root window's size in pixels.
func (d *display) Size() (int, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.connect(); err != nil {
		return 0, 0, err
	}
	geo, err := xproto.GetGeometry(d.conn, xproto.Drawable(d.screen.Root)).Reply()
	if err != nil {
		d.drop()
		return 0, 0, err
	}
	return int(geo.Width), int(geo.Height), nil
}

// Capture grabs the whole screen, with the pointer drawn where it is.
//
// GetImage does not include the cursor, and a screenshot without one is
// strictly worse for the reader we have in mind: an agent deciding where it
// just clicked. XFixes knows the cursor's image and position, so it is
// composited on afterwards.
func (d *display) Capture() (*image.RGBA, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.connect(); err != nil {
		return nil, err
	}
	root := xproto.Drawable(d.screen.Root)
	geo, err := xproto.GetGeometry(d.conn, root).Reply()
	if err != nil {
		d.drop()
		return nil, err
	}
	w, h := int(geo.Width), int(geo.Height)
	reply, err := xproto.GetImage(d.conn, xproto.ImageFormatZPixmap, root, 0, 0, geo.Width, geo.Height, 0xffffffff).Reply()
	if err != nil {
		d.drop()
		return nil, err
	}
	if reply.Depth != 24 && reply.Depth != 32 {
		return nil, fmt.Errorf("screen depth %d is not supported; run the display at 24", reply.Depth)
	}
	n := w * h
	if len(reply.Data) < n*4 {
		return nil, fmt.Errorf("short image: %d bytes for %dx%d", len(reply.Data), w, h)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	src, pix := reply.Data, img.Pix
	// ZPixmap at depth 24 or 32 is four bytes per pixel, B G R X.
	for i := 0; i < n; i++ {
		pix[i*4+0] = src[i*4+2]
		pix[i*4+1] = src[i*4+1]
		pix[i*4+2] = src[i*4+0]
		pix[i*4+3] = 0xff
	}
	if d.xfixes {
		d.drawCursor(img)
	}
	return img, nil
}

func (d *display) drawCursor(img *image.RGBA) {
	cur, err := xfixes.GetCursorImage(d.conn).Reply()
	if err != nil || cur.Width == 0 || cur.Height == 0 {
		return
	}
	cw, ch := int(cur.Width), int(cur.Height)
	if len(cur.CursorImage) < cw*ch {
		return
	}
	src := image.NewRGBA(image.Rect(0, 0, cw, ch))
	// The cursor arrives as premultiplied ARGB, which is what image.RGBA
	// stores, so the channels just need reordering.
	for i := 0; i < cw*ch; i++ {
		p := cur.CursorImage[i]
		src.Pix[i*4+0] = uint8(p >> 16)
		src.Pix[i*4+1] = uint8(p >> 8)
		src.Pix[i*4+2] = uint8(p)
		src.Pix[i*4+3] = uint8(p >> 24)
	}
	ox, oy := int(cur.X)-int(cur.Xhot), int(cur.Y)-int(cur.Yhot)
	draw.Draw(img, image.Rect(ox, oy, ox+cw, oy+ch), src, image.Point{}, draw.Over)
}
