// Package api is the wire format hollow speaks: what a desk is, what you can
// ask one to do, and what comes back.
//
// It is shared by the host daemon, the guest agent, and every client — which
// is the point. The host mostly forwards a request to the agent inside the
// right VM, so the two speak the same shapes, and a client (bangboo, or the
// hollow CLI) only ever has to learn one vocabulary.
package api

import (
	"encoding/json"
	"time"
)

// Version is the API prefix. Bump it when a shape changes incompatibly.
const Version = "v1"

// DefaultPort is where a host listens unless told otherwise.
const DefaultPort = 7070

// AgentPort is where the guest agent listens inside a VM. The host forwards a
// loopback port of its own to it; nothing outside the VM sees this number.
const AgentPort = 7000

// Hello is the one thing a host says without a token: that it is a hollow,
// which one, and which version. It is how a client scanning a mesh finds
// hosts, and how it picks which of a host's addresses answers.
type Hello struct {
	Hollow string `json:"hollow"` // version
	Name   string `json:"name"`   // hostname
}

// Status is what the host says about itself.
type Status struct {
	Version string   `json:"version"`
	Name    string   `json:"name"`
	OS      string   `json:"os"`
	Arch    string   `json:"arch"`
	Backend string   `json:"backend"`
	Ready   bool     `json:"ready"`             // the backend can start VMs here
	Problem string   `json:"problem,omitempty"` // why not, when it cannot
	CPUs    int      `json:"cpus"`
	MemMB   int      `json:"mem_mb"`  // total
	FreeMB  int      `json:"free_mb"` // available for new desks
	URLs    []string `json:"urls"`    // where clients can reach this host
	Desks   int      `json:"desks"`
	Images  []Image  `json:"images"`
}

// Image states.
const (
	ImageMissing      = "missing"      // never pulled
	ImagePulling      = "pulling"      // downloading the base
	ImageProvisioning = "provisioning" // first boot: installing the desktop and the agent hook
	ImageReady        = "ready"
	ImageFailed       = "failed"
)

// Image is a golden image for one guest OS: the thing every desk of that OS
// is cloned from.
type Image struct {
	OS       string    `json:"os"`
	State    string    `json:"state"`
	Progress string    `json:"progress,omitempty"`
	Error    string    `json:"error,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Updated  time.Time `json:"updated,omitempty"`

	// Recipe is the version of the provisioning script the image was built
	// with. Outdated means this hollow has a newer one: the image still
	// works, but desks from it lack whatever the new recipe added, and
	// pulling again brings it up to date.
	Recipe   int  `json:"recipe,omitempty"`
	Outdated bool `json:"outdated,omitempty"`
}

// DeskSpec is what a client asks for. Everything but OS has a default.
type DeskSpec struct {
	OS     string `json:"os"`
	Name   string `json:"name,omitempty"`
	MemMB  int    `json:"mem_mb,omitempty"`
	CPUs   int    `json:"cpus,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// Desk states.
const (
	DeskBooting = "booting" // VM started, agent not answering yet
	DeskReady   = "ready"   // agent answering; the desk can be driven
	DeskStopped = "stopped" // VM exited
	DeskFailed  = "failed"
)

// Desk is one running computer: a VM with a display, owned by one bot.
type Desk struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	OS       string    `json:"os"`
	State    string    `json:"state"`
	MemMB    int       `json:"mem_mb"`
	CPUs     int       `json:"cpus"`
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"`
	Error    string    `json:"error,omitempty"`
}

// Input actions.
const (
	InputMove        = "move"        // move the pointer to X,Y
	InputClick       = "click"       // click Button at X,Y (or where the pointer is)
	InputDblClick    = "dblclick"    // double-click
	InputTripleClick = "tripleclick" // triple-click: select a line or paragraph
	InputDown        = "down"        // press Button and hold
	InputUp          = "up"          // release Button
	InputScroll      = "scroll"      // scroll by DX,DY notches at X,Y
	InputType        = "type"        // type Text
	InputKey         = "key"         // press a chord such as ctrl+l or Return
	InputHold        = "hold"        // hold Keys down for DurationMS
	InputDrag        = "drag"        // press at X,Y, move to ToX,ToY, release
)

// Input is one thing done to the desk's pointer or keyboard.
//
// X and Y are pointers so that leaving them out means "where the pointer is"
// rather than the top-left corner. Keys use xdotool's names: Return, Tab,
// Escape, BackSpace, Page_Down, ctrl+l, alt+F4, super.
type Input struct {
	Action     string `json:"action"`
	X          *int   `json:"x,omitempty"`
	Y          *int   `json:"y,omitempty"`
	ToX        *int   `json:"to_x,omitempty"`
	ToY        *int   `json:"to_y,omitempty"`
	Button     string `json:"button,omitempty"` // left (default), middle, right
	Text       string `json:"text,omitempty"`
	Keys       string `json:"keys,omitempty"`
	Modifiers  string `json:"modifiers,omitempty"` // held during a click or scroll: "shift", "ctrl+shift"
	DX         int    `json:"dx,omitempty"`
	DY         int    `json:"dy,omitempty"`
	DurationMS int    `json:"duration_ms,omitempty"` // for hold
}

// Cursor is where the pointer is.
type Cursor struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Exec runs a program on the desk, as the desk's user, with the display set.
type Exec struct {
	Cmd       string   `json:"cmd"`
	Args      []string `json:"args,omitempty"`
	Shell     bool     `json:"shell,omitempty"` // run Cmd through the shell instead
	Stdin     string   `json:"stdin,omitempty"`
	Dir       string   `json:"dir,omitempty"`
	Env       []string `json:"env,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitempty"` // 0 means the agent's default
	Detach    bool     `json:"detach,omitempty"`     // start it and return at once
}

// ExecResult is what came back.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Code     int    `json:"code"`
	TimedOut bool   `json:"timed_out,omitempty"`
	PID      int    `json:"pid,omitempty"` // for Detach
}

// Health is what the agent reports about the desk it is on.
type Health struct {
	Display  string   `json:"display"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	Uptime   float64  `json:"uptime_s"`
	Agent    string   `json:"agent"`
	Features []string `json:"features,omitempty"` // browser, windows, clipboard
}

// Record asks for a screen recording. FPS 0 means the default.
type Record struct {
	FPS int `json:"fps,omitempty"`
}

// Window is one top-level window on the desk.
type Window struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Class  string `json:"class,omitempty"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Active bool   `json:"active,omitempty"`
}

// WindowAction is done to one window: activate (raise and focus) or close.
type WindowAction struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

// Clipboard is the desk's clipboard text.
type Clipboard struct {
	Text string `json:"text"`
}

// BrowserOpen navigates the desk's browser, starting it if it is not running.
type BrowserOpen struct {
	URL    string `json:"url"`
	NewTab bool   `json:"new_tab,omitempty"`
}

// BrowserRead asks for the page as text: a window of its visible text, and
// the elements on it that can be clicked or typed into.
type BrowserRead struct {
	Offset      int `json:"offset,omitempty"`       // into the page text, in characters
	MaxChars    int `json:"max_chars,omitempty"`    // 0: the agent's default
	MaxElements int `json:"max_elements,omitempty"` // 0: the agent's default
}

// Element is one interactive thing on a page. Index is what browser click
// and type take; it is valid until the next read. X and Y are the element's
// centre in screen pixels, for when a pointer click is wanted instead.
type Element struct {
	Index    int    `json:"index"`
	Kind     string `json:"kind"` // link, button, input, textarea, select, checkbox, ...
	Label    string `json:"label"`
	Value    string `json:"value,omitempty"`
	Href     string `json:"href,omitempty"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Visible  bool   `json:"visible"` // inside the viewport right now
	Disabled bool   `json:"disabled,omitempty"`
}

// BrowserState is the page, as text.
type BrowserState struct {
	URL        string    `json:"url"`
	Title      string    `json:"title"`
	Text       string    `json:"text"`
	Offset     int       `json:"offset"`
	TotalChars int       `json:"total_chars"`
	Elements   []Element `json:"elements"`
	More       int       `json:"more_elements,omitempty"` // elements not listed
	ScrollY    int       `json:"scroll_y"`
	PageHeight int       `json:"page_height"`
	ViewHeight int       `json:"view_height"`
	Tabs       int       `json:"tabs"`
}

// BrowserClick clicks an element from the last read.
type BrowserClick struct {
	Index int `json:"index"`
}

// BrowserType types into an element from the last read.
type BrowserType struct {
	Index  int    `json:"index"`
	Text   string `json:"text"`
	Clear  bool   `json:"clear,omitempty"`  // replace what is there
	Submit bool   `json:"submit,omitempty"` // press Enter afterwards
}

// BrowserEval runs JavaScript in the page.
type BrowserEval struct {
	JS string `json:"js"`
}

// BrowserEvalResult is its value, as JSON.
type BrowserEvalResult struct {
	Value json.RawMessage `json:"value"`
}

// BrowserResult says what an action did to the page.
type BrowserResult struct {
	URL       string `json:"url"`
	Navigated bool   `json:"navigated,omitempty"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}
