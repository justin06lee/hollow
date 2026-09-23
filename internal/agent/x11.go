package agent

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io"
	"log"
	"strconv"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
	"github.com/justin06lee/hollow/api"
)

func init() {
	// xgb narrates its search for an .Xauthority file on every connection.
	// Xvfb here runs with access control off, so there is nothing to find,
	// and nothing to say about it.
	xgb.Logger = log.New(io.Discard, "", 0)
}

// display is one connection to the X server, made lazily and remade if it
// breaks. The agent starts before Xvfb has finished coming up, and a display
// restarted underneath us should not take the agent down with it.
type display struct {
	name string

	mu     sync.Mutex
	conn   *xgb.Conn
	screen *xproto.ScreenInfo
	xfixes bool
	atoms  map[string]xproto.Atom
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
	d.atoms = map[string]xproto.Atom{}
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

// Capture grabs the whole screen, with the pointer drawn where it is unless
// cursor is false.
//
// GetImage does not include the cursor, and a screenshot without one is
// strictly worse for the reader we have in mind: an agent deciding where it
// just clicked. XFixes knows the cursor's image and position, so it is
// composited on afterwards.
func (d *display) Capture(cursor bool) (*image.RGBA, error) {
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
	if cursor && d.xfixes {
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

// Pointer is where the pointer is, in screen pixels.
func (d *display) Pointer() (int, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.connect(); err != nil {
		return 0, 0, err
	}
	r, err := xproto.QueryPointer(d.conn, d.screen.Root).Reply()
	if err != nil {
		d.drop()
		return 0, 0, err
	}
	return int(r.RootX), int(r.RootY), nil
}

func (d *display) atom(name string) (xproto.Atom, error) {
	if a, ok := d.atoms[name]; ok {
		return a, nil
	}
	r, err := xproto.InternAtom(d.conn, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0, err
	}
	d.atoms[name] = r.Atom
	return r.Atom, nil
}

func (d *display) prop(win xproto.Window, name string) (*xproto.GetPropertyReply, error) {
	a, err := d.atom(name)
	if err != nil {
		return nil, err
	}
	return xproto.GetProperty(d.conn, false, win, a, xproto.GetPropertyTypeAny, 0, 1<<16).Reply()
}

func (d *display) windowIDs(name string) []xproto.Window {
	r, err := d.prop(d.screen.Root, name)
	if err != nil || r.Format != 32 {
		return nil
	}
	var out []xproto.Window
	for i := 0; i+4 <= len(r.Value); i += 4 {
		out = append(out, xproto.Window(xgb.Get32(r.Value[i:])))
	}
	return out
}

func (d *display) title(win xproto.Window) string {
	if r, err := d.prop(win, "_NET_WM_NAME"); err == nil && len(r.Value) > 0 {
		return string(r.Value)
	}
	if r, err := d.prop(win, "WM_NAME"); err == nil && len(r.Value) > 0 {
		return string(r.Value)
	}
	return ""
}

func (d *display) class(win xproto.Window) string {
	r, err := d.prop(win, "WM_CLASS")
	if err != nil || len(r.Value) == 0 {
		return ""
	}
	// WM_CLASS is "instance\0Class\0"; the second is the one people know.
	parts := splitNul(r.Value)
	if len(parts) >= 2 {
		return parts[1]
	}
	return parts[0]
}

func splitNul(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

// Windows lists the window manager's managed windows, in stacking order
// from bottom to top, so the last one is on top.
//
// It asks the window manager rather than walking the X tree: the tree is
// full of frames, tooltips and invisible helpers, and _NET_CLIENT_LIST is
// exactly the windows a person would call windows.
func (d *display) Windows() ([]api.Window, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.connect(); err != nil {
		return nil, err
	}
	ids := d.windowIDs("_NET_CLIENT_LIST_STACKING")
	if len(ids) == 0 {
		ids = d.windowIDs("_NET_CLIENT_LIST")
	}
	var active xproto.Window
	if a := d.windowIDs("_NET_ACTIVE_WINDOW"); len(a) > 0 {
		active = a[0]
	}
	out := make([]api.Window, 0, len(ids))
	for _, win := range ids {
		attr, err := xproto.GetWindowAttributes(d.conn, win).Reply()
		if err != nil || attr.MapState != xproto.MapStateViewable {
			continue
		}
		geo, err := xproto.GetGeometry(d.conn, xproto.Drawable(win)).Reply()
		if err != nil {
			continue
		}
		pos, err := xproto.TranslateCoordinates(d.conn, win, d.screen.Root, 0, 0).Reply()
		if err != nil {
			continue
		}
		out = append(out, api.Window{
			ID:     strconv.FormatUint(uint64(win), 10),
			Title:  d.title(win),
			Class:  d.class(win),
			X:      int(pos.DstX),
			Y:      int(pos.DstY),
			Width:  int(geo.Width),
			Height: int(geo.Height),
			Active: win == active,
		})
	}
	return out, nil
}

// WindowAction activates or closes a window, the way a taskbar would: by
// asking the window manager, so that an application gets to save its work
// or ask whether to.
func (d *display) WindowAction(id, action string) error {
	n, err := strconv.ParseUint(id, 10, 32)
	if err != nil {
		return fmt.Errorf("window id %q is not a number", id)
	}
	win := xproto.Window(n)
	var msg string
	switch action {
	case "activate", "focus", "raise":
		msg = "_NET_ACTIVE_WINDOW"
	case "close":
		msg = "_NET_CLOSE_WINDOW"
	default:
		return fmt.Errorf("unknown window action %q: activate or close", action)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.connect(); err != nil {
		return err
	}
	found := false
	for _, w := range d.windowIDs("_NET_CLIENT_LIST") {
		if w == win {
			found = true
		}
	}
	if !found {
		return errors.New("no such window; list them again")
	}
	a, err := d.atom(msg)
	if err != nil {
		return err
	}
	// Source indication 2 says "a pager or taskbar asked", which window
	// managers honour without the focus-stealing checks they apply to
	// applications.
	ev := xproto.ClientMessageEvent{
		Format: 32,
		Window: win,
		Type:   a,
		Data:   xproto.ClientMessageDataUnionData32New([]uint32{0, 2, 0, 0, 0}),
	}
	if msg == "_NET_ACTIVE_WINDOW" {
		ev.Data = xproto.ClientMessageDataUnionData32New([]uint32{2, 0, 0, 0, 0})
	}
	mask := uint32(xproto.EventMaskSubstructureRedirect | xproto.EventMaskSubstructureNotify)
	return xproto.SendEventChecked(d.conn, false, d.screen.Root, mask, string(ev.Bytes())).Check()
}
