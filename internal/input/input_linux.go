//go:build linux && !android

package input

// Linux injection goes through the X server's XTEST extension on X11,
// and through the portal's RemoteDesktop session on Wayland
// (input_portal_linux.go). Keys are evdev
// keycodes + 8, the keycodes of every modern X server, which keeps the
// remote keyboard layout-independent. Text is typed by binding each
// character's keysym to a spare keycode, as xdotool does.

import (
	"fmt"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"

	"github.com/i5shuyi/remote-go/internal/keymap"
	"github.com/i5shuyi/remote-go/internal/portal"
)

const (
	xKeyPress      = 2
	xKeyRelease    = 3
	xButtonPress   = 4
	xButtonRelease = 5
	xMotionNotify  = 6

	// Browser wheel deltas are about 100 px per notch; X scrolls by notch
	// (buttons 4–7).
	wheelStep = 100.0

	keysymShiftL = 0xffe1
)

type xInjector struct {
	mu     sync.Mutex
	c      *xgb.Conn
	root   xproto.Window
	h      held
	x, y   float64
	wx, wy float64 // wheel remainders

	minKey, maxKey xproto.Keycode
	spares         []xproto.Keycode // keycodes without keysyms, for Text
	spareUsed      bool
}

func newInjector() (Injector, error) {
	if portal.Wayland() {
		portal.Prepare(true)
		return &portalInjector{h: newHeld()}, nil
	}
	c, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("%w：无法连接 X 服务器（%v）", ErrUnsupported, err)
	}
	if err := xtest.Init(c); err != nil {
		c.Close()
		return nil, fmt.Errorf("%w：X 服务器没有 XTEST 扩展", ErrUnsupported)
	}
	setup := xproto.Setup(c)
	in := &xInjector{c: c, root: setup.DefaultScreen(c).Root, h: newHeld(),
		minKey: setup.MinKeycode, maxKey: setup.MaxKeycode}
	// Nobody waits on replies-less requests; drain errors and events so
	// xgb's queue never fills.
	go func() {
		for {
			if ev, err := c.WaitForEvent(); ev == nil && err == nil {
				return
			}
		}
	}()
	return in, nil
}

func (in *xInjector) fake(typ, detail byte, x, y float64) {
	xtest.FakeInput(in.c, typ, detail, 0, in.root, int16(x), int16(y), 0)
}

func (in *xInjector) MouseMove(x, y float64) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.x, in.y = x, y
	in.fake(xMotionNotify, 0, x, y)
	in.c.Sync()
}

func (in *xInjector) MouseButton(button int, down bool, x, y float64) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.button(button, down, x, y)
	in.c.Sync()
}

func (in *xInjector) button(button int, down bool, x, y float64) {
	var detail byte
	switch button {
	case ButtonLeft:
		detail = 1
	case ButtonMiddle:
		detail = 2
	case ButtonRight:
		detail = 3
	default:
		return
	}
	in.h.buttons[button] = down
	in.x, in.y = x, y
	in.fake(xMotionNotify, 0, x, y)
	in.fake(pick(down, xButtonPress, xButtonRelease), detail, 0, 0)
}

func (in *xInjector) Wheel(dx, dy float64) {
	in.mu.Lock()
	defer in.mu.Unlock()
	click := func(acc *float64, d float64, neg, pos byte) {
		*acc += d
		for ; *acc >= wheelStep; *acc -= wheelStep {
			in.fake(xButtonPress, pos, 0, 0)
			in.fake(xButtonRelease, pos, 0, 0)
		}
		for ; *acc <= -wheelStep; *acc += wheelStep {
			in.fake(xButtonPress, neg, 0, 0)
			in.fake(xButtonRelease, neg, 0, 0)
		}
	}
	click(&in.wy, dy, 4, 5) // up, down
	click(&in.wx, dx, 6, 7) // left, right
	in.c.Sync()
}

func (in *xInjector) Key(code string, down bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.key(code, down)
	in.c.Sync()
}

func (in *xInjector) key(code string, down bool) {
	ev, ok := keymap.ToEvdev(code)
	if !ok {
		return
	}
	in.h.keys[code] = down
	in.fake(pick(down, xKeyPress, xKeyRelease), byte(ev+8), 0, 0)
}

// keysymOf returns the X keysym that types r.
func keysymOf(r rune) xproto.Keysym {
	switch r {
	case '\n', '\r':
		return 0xff0d // Return
	case '\t':
		return 0xff09 // Tab
	case '\b':
		return 0xff08 // BackSpace
	}
	if r >= 0x20 && r <= 0x7e || r >= 0xa0 && r <= 0xff {
		return xproto.Keysym(r) // Latin-1 keysyms are the code points
	}
	return xproto.Keysym(0x01000000 | r)
}

// Text types s. Characters the current layout has on a key (alone or with
// Shift) use that key; others are bound to spare keycodes, a batch at a
// time, with pauses so clients reload the keymap before the keys arrive.
func (in *xInjector) Text(s string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	r, err := xproto.GetKeyboardMapping(in.c, in.minKey, byte(in.maxKey-in.minKey+1)).Reply()
	if err != nil || r.KeysymsPerKeycode == 0 {
		return
	}
	per := int(r.KeysymsPerKeycode)
	type place struct {
		key   xproto.Keycode
		shift bool
	}
	layout := map[xproto.Keysym]place{}
	in.spares = in.spares[:0]
	for i := 0; i*per < len(r.Keysyms); i++ {
		row := r.Keysyms[i*per : (i+1)*per]
		kc := in.minKey + xproto.Keycode(i)
		empty := true
		for col, ks := range row {
			if ks != 0 {
				empty = false
				if _, seen := layout[ks]; !seen && col < 2 {
					layout[ks] = place{kc, col == 1}
				}
			}
		}
		if empty {
			in.spares = append(in.spares, kc)
		}
	}
	shiftKey := layout[keysymShiftL].key

	tap := func(p place) {
		shift := p.shift && shiftKey != 0 && !in.h.keys["ShiftLeft"] && !in.h.keys["ShiftRight"]
		if shift {
			in.fake(xKeyPress, byte(shiftKey), 0, 0)
		}
		in.fake(xKeyPress, byte(p.key), 0, 0)
		in.fake(xKeyRelease, byte(p.key), 0, 0)
		if shift {
			in.fake(xKeyRelease, byte(shiftKey), 0, 0)
		}
	}
	runes := []rune(s)
	for len(runes) > 0 {
		// Type what the layout has, up to the first character it lacks.
		ks := keysymOf(runes[0])
		if p, ok := layout[ks]; ok && p.key != 0 {
			tap(p)
			runes = runes[1:]
			continue
		}
		if len(in.spares) == 0 {
			runes = runes[1:] // no room to bind it
			continue
		}
		// Bind the next run of missing characters to the spares.
		bound := map[xproto.Keysym]xproto.Keycode{}
		n := 0
		for n < len(runes) {
			ks := keysymOf(runes[n])
			if _, ok := layout[ks]; ok {
				break
			}
			if _, ok := bound[ks]; !ok {
				if len(bound) == len(in.spares) {
					break
				}
				kc := in.spares[len(bound)]
				bound[ks] = kc
				row := make([]xproto.Keysym, per)
				for i := range row {
					row[i] = ks
				}
				xproto.ChangeKeyboardMapping(in.c, 1, kc, byte(per), row)
			}
			n++
		}
		in.spareUsed = true
		in.c.Sync()
		time.Sleep(25 * time.Millisecond)
		for _, r := range runes[:n] {
			tap(place{key: bound[keysymOf(r)]})
		}
		in.c.Sync()
		time.Sleep(25 * time.Millisecond)
		runes = runes[n:]
	}
	in.c.Sync()
}

func (in *xInjector) ReleaseAll() {
	in.mu.Lock()
	defer in.mu.Unlock()
	for b, on := range in.h.buttons {
		if on {
			in.button(b, false, in.x, in.y)
		}
	}
	for k, on := range in.h.keys {
		if on {
			in.key(k, false)
		}
	}
	if in.spareUsed {
		// Give the spare keycodes back their empty rows.
		r, err := xproto.GetKeyboardMapping(in.c, in.minKey, 1).Reply()
		if err == nil && r.KeysymsPerKeycode > 0 {
			empty := make([]xproto.Keysym, r.KeysymsPerKeycode)
			for _, kc := range in.spares {
				xproto.ChangeKeyboardMapping(in.c, 1, kc, r.KeysymsPerKeycode, empty)
			}
		}
		in.spareUsed = false
	}
	in.c.Sync()
}

func pick(c bool, a, b byte) byte {
	if c {
		return a
	}
	return b
}
