package screen

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=11.0
#cgo LDFLAGS: -framework Cocoa -framework AVFoundation -framework CoreMedia -framework ImageIO -framework QuartzCore
#include "screen_darwin.h"
*/
import "C"

import (
	"runtime/cgo"
	"unsafe"

	"github.com/nsgo-dev/nsgo/core"

	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/controlball"
	"github.com/i5shuyi/remote-go/internal/keymap"
)

type macView struct {
	viewBase
	ball controlball.Position
	cx   *core.NativeContext
	h    cgo.Handle
	ptr  unsafe.Pointer // retained RGScreenView
}

func newView(cx *core.NativeContext) core.NativeComponent {
	v := &macView{cx: cx, ball: controlball.TopRight()}
	v.h = cgo.NewHandle(v)
	v.ptr = C.rg_screen_new(C.uintptr_t(v.h))
	return v
}

func (v *macView) View() any { return uintptr(v.ptr) }

func (v *macView) SetProp(name string, value any) {
	if name == "link" {
		id, _ := value.(string)
		v.setLink(v, id)
	}
}

func (v *macView) Command(name string, args any) {
	if name == "focus" {
		C.rg_screen_focus(v.ptr)
	}
}

func (v *macView) Close() {
	v.unlink(v)
	C.rg_screen_free(v.ptr)
	v.h.Delete()
}

func (v *macView) frame(kind byte, data []byte) bool {
	if len(data) == 0 {
		return true
	}
	p, n := (*C.uint8_t)(unsafe.Pointer(&data[0])), C.int(len(data))
	switch kind {
	case client.FrameJPEG:
		C.rg_screen_jpeg(v.ptr, p, n)
	case client.FrameH264Key, client.FrameH264Diff:
		key := C.int(0)
		if kind == client.FrameH264Key {
			key = 1
		}
		return C.rg_screen_h264(v.ptr, p, n, key) == 0
	}
	return true
}

func (v *macView) reset() { C.rg_screen_reset(v.ptr) }

//export rgScreenEvent
func rgScreenEvent(h C.uintptr_t, kind, a C.int, x, y C.double) {
	v, ok := cgo.Handle(h).Value().(*macView)
	if !ok {
		return
	}
	switch kind {
	case 1:
		v.move(float64(x), float64(y))
	case 2, 3:
		v.button(int(a), kind == 2, float64(x), float64(y))
	case 4:
		v.wheel(float64(x), float64(y))
	case 5, 6:
		v.key(keymap.FromMac(uint16(a)), kind == 5)
	case 7:
		v.blur()
	case 8:
		v.cx.Emit(ControlsEvent, core.Point{X: core.Points(x), Y: core.Points(y)})
	}
}

// rgBallLayout returns the frame origin for either a resize or a pointer move.
//
//export rgBallLayout
func rgBallLayout(h C.uintptr_t, w, height C.double, move, snap C.int, x, y *C.double) {
	v := cgo.Handle(h).Value().(*macView)
	if move != 0 {
		v.ball = v.ball.Move(float64(*x), float64(*y), float64(w), float64(height), snap != 0)
	}
	ox, oy := v.ball.Origin(float64(w), float64(height))
	*x, *y = C.double(ox), C.double(oy)
}
