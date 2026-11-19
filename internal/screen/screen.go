// Package screen is the remote-screen view of the desktop app: an NSGo
// native component (core.Native{Component: screen.Component}) that each
// platform implements with its own view, decoding H.264 in hardware and
// capturing raw mouse and keyboard input:
//
//   - macOS: an NSView backed by AVSampleBufferDisplayLayer (VideoToolbox
//     decode, displayed by the compositor without copies).
//   - Windows: a child HWND fed by the Media Foundation H.264 decoder.
//
// A session and its view meet through a Link, named by the "link" prop,
// so frames go from the network goroutine straight to the view without
// crossing NSGo's engine.
package screen

import (
	"sync"
	"sync/atomic"

	"github.com/nsgo-dev/nsgo"

	"github.com/i5shuyi/remote-go/internal/client"
)

// Component is the registered name of the remote-screen view.
const Component = "remotego.screen"

// ControlsEvent opens the local session menu at the button in root-view points.
const ControlsEvent = "controls"

// Option registers the view with NSGo; pass it to nsgo.Run.
func Option() nsgo.Option { return nsgo.Component(Component, newView) }

// Input is where a view's input goes; *client.Session implements it.
type Input interface {
	MouseMove(x, y float64)
	MouseButton(b int, down bool, x, y float64)
	Wheel(dx, dy float64)
	Key(code string, down bool)
	ReleaseKeys()
	RequestKeyframe()
}

// sink is a platform view frames are drawn into.
type sink interface {
	// frame draws one frame; it reports false when the decoder needs a
	// keyframe to continue.
	frame(kind byte, data []byte) bool
	// reset drops decoder state, as when the stream restarts.
	reset()
}

// Link joins a session to the view that shows it.
type Link struct {
	id string

	in      Input // fixed
	control atomic.Bool

	mu      sync.Mutex // guards view and decoding
	view    sink
	needKey bool
}

var links sync.Map // id → *Link

// NewLink makes a link that views name by id.
func NewLink(id string, in Input) *Link {
	l := &Link{id: id, in: in, needKey: true}
	links.Store(id, l)
	return l
}

// SetControl turns input forwarding on or off (view-only sessions).
func (l *Link) SetControl(on bool) {
	l.control.Store(on)
	if !on {
		l.in.ReleaseKeys()
	}
}

// Frame hands a received frame to the view; call it in stream order.
func (l *Link) Frame(kind byte, data []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.view == nil {
		return
	}
	switch kind {
	case client.FrameH264Key:
		l.needKey = false
	case client.FrameH264Diff:
		if l.needKey {
			return
		}
	}
	if !l.view.frame(kind, data) && kind != client.FrameJPEG {
		l.needKey = true
		go l.in.RequestKeyframe()
	}
}

// Reset drops decoder state; the next H.264 frame shown is a keyframe.
func (l *Link) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.needKey = true
	if l.view != nil {
		l.view.reset()
	}
}

// Close forgets the link; its view goes blank.
func (l *Link) Close() {
	links.Delete(l.id)
	l.mu.Lock()
	l.view = nil
	l.mu.Unlock()
}

func (l *Link) attach(v sink) {
	l.mu.Lock()
	l.view, l.needKey = v, true
	l.mu.Unlock()
	go l.in.RequestKeyframe()
}

func (l *Link) detach(v sink) {
	l.mu.Lock()
	if l.view == v {
		l.view = nil
	}
	l.mu.Unlock()
}

func (l *Link) input() Input {
	if l == nil {
		return nil
	}
	if !l.control.Load() {
		return nil
	}
	return l.in
}

// viewBase is what every platform view shares: the link it shows.
type viewBase struct {
	mu   sync.Mutex
	link *Link
}

func (b *viewBase) setLink(self sink, id string) {
	var next *Link
	if v, ok := links.Load(id); ok {
		next = v.(*Link)
	}
	b.mu.Lock()
	prev := b.link
	b.link = next
	b.mu.Unlock()
	if prev == next {
		return
	}
	if prev != nil {
		prev.detach(self)
	}
	if next != nil {
		next.attach(self)
	}
}

func (b *viewBase) unlink(self sink) {
	b.mu.Lock()
	l := b.link
	b.link = nil
	b.mu.Unlock()
	if l != nil {
		l.detach(self)
		l.in.ReleaseKeys()
	}
}

func (b *viewBase) in() Input {
	b.mu.Lock()
	l := b.link
	b.mu.Unlock()
	return l.input()
}

func (b *viewBase) move(x, y float64) {
	if in := b.in(); in != nil {
		in.MouseMove(x, y)
	}
}

func (b *viewBase) button(btn int, down bool, x, y float64) {
	if in := b.in(); in != nil {
		in.MouseButton(btn, down, x, y)
	}
}

func (b *viewBase) wheel(dx, dy float64) {
	if in := b.in(); in != nil {
		in.Wheel(dx, dy)
	}
}

func (b *viewBase) key(code string, down bool) {
	if in := b.in(); in != nil && code != "" {
		in.Key(code, down)
	}
}

func (b *viewBase) blur() {
	if in := b.in(); in != nil {
		in.ReleaseKeys()
	}
}
