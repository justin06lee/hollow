// Package api is the wire format hollow speaks: what a desk is, what you can
// ask one to do, and what comes back.
//
// It is shared by the host daemon, the guest agent, and every client — which
// is the point. The host mostly forwards a request to the agent inside the
// right VM, so the two speak the same shapes, and a client (bangboo, or the
// hollow CLI) only ever has to learn one vocabulary.
package api

import "time"

// Version is the API prefix. Bump it when a shape changes incompatibly.
const Version = "v1"

// AgentPort is where the guest agent listens inside a VM. The host forwards a
// loopback port of its own to it; nothing outside the VM sees this number.
const AgentPort = 7000

// Status is what the host says about itself.
type Status struct {
	Version string  `json:"version"`
	OS      string  `json:"os"`
	Arch    string  `json:"arch"`
	Backend string  `json:"backend"`
	Desks   int     `json:"desks"`
	Images  []Image `json:"images"`
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
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	OS      string    `json:"os"`
	State   string    `json:"state"`
	MemMB   int       `json:"mem_mb"`
	CPUs    int       `json:"cpus"`
	Width   int       `json:"width"`
	Height  int       `json:"height"`
	Created time.Time `json:"created"`
	Error   string    `json:"error,omitempty"`
}

// Input actions.
const (
	InputMove     = "move"     // move the pointer to X,Y
	InputClick    = "click"    // click Button at X,Y (or where the pointer is)
	InputDblClick = "dblclick" // double-click
	InputDown     = "down"     // press Button and hold
	InputUp       = "up"       // release Button
	InputScroll   = "scroll"   // scroll by DX,DY notches at X,Y
	InputType     = "type"     // type Text
	InputKey      = "key"      // press a chord such as ctrl+l or Return
	InputDrag     = "drag"     // press at X,Y, move to ToX,ToY, release
)

// Input is one thing done to the desk's pointer or keyboard.
//
// X and Y are pointers so that leaving them out means "where the pointer is"
// rather than the top-left corner.
type Input struct {
	Action string `json:"action"`
	X      *int   `json:"x,omitempty"`
	Y      *int   `json:"y,omitempty"`
	ToX    *int   `json:"to_x,omitempty"`
	ToY    *int   `json:"to_y,omitempty"`
	Button string `json:"button,omitempty"` // left (default), middle, right
	Text   string `json:"text,omitempty"`
	Keys   string `json:"keys,omitempty"`
	DX     int    `json:"dx,omitempty"`
	DY     int    `json:"dy,omitempty"`
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
	Display string  `json:"display"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Uptime  float64 `json:"uptime_s"`
	Agent   string  `json:"agent"`
}

// Record asks for a screen recording. FPS 0 means the default.
type Record struct {
	FPS int `json:"fps,omitempty"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}
