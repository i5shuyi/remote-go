//go:build linux && !android

package input

import (
	"sync"

	"github.com/i5shuyi/remote-go/internal/keymap"
	"github.com/i5shuyi/remote-go/internal/portal"
)

// evdev pointer buttons.
const (
	btnLeft   = 0x110
	btnRight  = 0x111
	btnMiddle = 0x112
)

// portalInjector sends input through the RemoteDesktop portal session.
// Until the person allows the session, input is dropped. Coordinates are
// the compositor's logical ones, which display.Bounds reports on Wayland.
type portalInjector struct {
	mu   sync.Mutex
	h    held
	x, y float64
}

func (in *portalInjector) session() *portal.Session {
	if s := portal.Current(); s != nil && s.Input() {
		return s
	}
	return nil
}

// move points at (x, y) on the stream that holds it.
func (in *portalInjector) move(s *portal.Session, x, y float64) {
	streams := s.Streams()
	if len(streams) == 0 {
		return
	}
	st := streams[0]
	for _, c := range streams {
		b := c.Bounds
		if x >= float64(b.Min.X) && x < float64(b.Max.X) && y >= float64(b.Min.Y) && y < float64(b.Max.Y) {
			st = c
			break
		}
	}
	b := st.Bounds
	lx := min(max(x-float64(b.Min.X), 0), float64(max(b.Dx()-1, 0)))
	ly := min(max(y-float64(b.Min.Y), 0), float64(max(b.Dy()-1, 0)))
	s.Motion(st.Node, lx, ly)
}

func (in *portalInjector) MouseMove(x, y float64) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.x, in.y = x, y
	if s := in.session(); s != nil {
		in.move(s, x, y)
	}
}

func (in *portalInjector) MouseButton(button int, down bool, x, y float64) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if s := in.session(); s != nil {
		in.move(s, x, y)
		in.button(s, button, down)
	}
	in.x, in.y = x, y
}

func (in *portalInjector) button(s *portal.Session, button int, down bool) {
	var code int32
	switch button {
	case ButtonLeft:
		code = btnLeft
	case ButtonMiddle:
		code = btnMiddle
	case ButtonRight:
		code = btnRight
	default:
		return
	}
	in.h.buttons[button] = down
	s.Button(code, down)
}

// Wheel scrolls smoothly. Compositors count 10 continuous units as one
// wheel step (libinput's convention), where the protocol's ~100 px is one.
func (in *portalInjector) Wheel(dx, dy float64) {
	if s := in.session(); s != nil && (dx != 0 || dy != 0) {
		s.Axis(dx*portalWheelScale, dy*portalWheelScale)
	}
}

const portalWheelScale = 10.0 / wheelStep

func (in *portalInjector) Key(code string, down bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if s := in.session(); s != nil {
		in.key(s, code, down)
	}
}

func (in *portalInjector) key(s *portal.Session, code string, down bool) {
	ev, ok := keymap.ToEvdev(code)
	if !ok {
		return
	}
	in.h.keys[code] = down
	s.Key(int32(ev), down)
}

// Text sends each character as a keysym; the compositor types it with a
// key of the current layout, so characters the layout lacks are skipped.
func (in *portalInjector) Text(t string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	s := in.session()
	if s == nil {
		return
	}
	for _, r := range t {
		sym := int32(keysymOf(r))
		s.Keysym(sym, true)
		s.Keysym(sym, false)
	}
}

func (in *portalInjector) ReleaseAll() {
	in.mu.Lock()
	defer in.mu.Unlock()
	s := in.session()
	if s == nil {
		in.h = newHeld()
		return
	}
	for b, on := range in.h.buttons {
		if on {
			in.button(s, b, false)
		}
	}
	for k, on := range in.h.keys {
		if on {
			in.key(s, k, false)
		}
	}
}
