//go:build windows && (amd64 || arm64)

package screen

import (
	"math"
	"syscall"
	"unsafe"

	"github.com/nsgo-dev/nsgo/core"

	"github.com/i5shuyi/remote-go/internal/controlball"
)

// The video is a child HWND above NSGo's drawing surface. Put the button
// inside that HWND so it stays visible and takes its own mouse events.
var (
	comctl32              = syscall.NewLazyDLL("comctl32.dll")
	pSetWindowSubclass    = comctl32.NewProc("SetWindowSubclass")
	pRemoveWindowSubclass = comctl32.NewProc("RemoveWindowSubclass")
	pDefSubclassProc      = comctl32.NewProc("DefSubclassProc")
	pGetParent            = user32.NewProc("GetParent")
	pGetFocus             = user32.NewProc("GetFocus")
	pGetForegroundWindow  = user32.NewProc("GetForegroundWindow")
	pIsWindowVisible      = user32.NewProc("IsWindowVisible")
	pPostMessage          = user32.NewProc("PostMessageW")
	pGetDpiForWindow      = user32.NewProc("GetDpiForWindow")
	pMoveWindow           = user32.NewProc("MoveWindow")
	pSetWindowRgn         = user32.NewProc("SetWindowRgn")
	pCreateEllipticRgn    = gdi32.NewProc("CreateEllipticRgn")
	pDeleteObject         = gdi32.NewProc("DeleteObject")
	pSetDCBrushColor      = gdi32.NewProc("SetDCBrushColor")
	pFillRgn              = gdi32.NewProc("FillRgn")

	pMapWindowPoints = user32.NewProc("MapWindowPoints")
	ballProc         = syscall.NewCallback(ballWindowProc)
	parentFocusProc  = syscall.NewCallback(parentFocus)
)

const (
	wsClipChildren          = 0x02000000
	wmSetFocus              = 0x0007
	wmDrawItem              = 0x002b
	wmCommand               = 0x0111
	wmDPIChangedAfterParent = 0x02e3
	wmFocusScreen           = 0x8001 // private message on the video HWND
)

// A secondary window can become active before or after its video is
// mounted. Restore focus on either path, after Windows finishes activation.
func (v *winView) watchParentFocus() {
	parent, _, _ := pGetParent.Call(v.hwnd)
	if parent == 0 {
		return
	}
	v.parent = parent
	pSetWindowSubclass.Call(parent, parentFocusProc, v.hwnd, 0)
	v.layoutControls()
	pPostMessage.Call(v.hwnd, wmFocusScreen, 0, 0)
}

func parentFocus(hwnd, msg, wp, lp, child, data uintptr) uintptr {
	if msg == wmSetFocus {
		pPostMessage.Call(child, wmFocusScreen, 0, 0)
	}
	r, _, _ := pDefSubclassProc.Call(hwnd, msg, wp, lp)
	return r
}

func (v *winView) focusScreen() {
	active, _, _ := pGetForegroundWindow.Call()
	visible, _, _ := pIsWindowVisible.Call(v.hwnd)
	if active == v.parent && active != 0 && visible != 0 {
		pSetFocus.Call(v.hwnd)
	}
}

func (v *winView) createControls(instance uintptr) {
	class, _ := syscall.UTF16PtrFromString("BUTTON")
	title, _ := syscall.UTF16PtrFromString("会话控制")
	const childVisibleOwnerDraw = 0x40000000 | 0x10000000 | 0xb
	v.ball, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)),
		childVisibleOwnerDraw, 0, 0, controlball.Size, controlball.Size, v.hwnd, 1, instance, 0)
	pSetWindowSubclass.Call(v.ball, ballProc, v.hwnd, 0)
	v.layoutControls()
}

type ballControl struct {
	position           controlball.Position
	down, grab         core.Point
	tracking, dragging bool
	diameter           uintptr
}

func newBallControl() ballControl { return ballControl{position: controlball.TopRight()} }

func (v *winView) ballArea() (width, height, scale float64) {
	dpi, _, _ := pGetDpiForWindow.Call(v.hwnd)
	if dpi == 0 {
		dpi = 96
	}
	scale = float64(dpi) / 96
	var bounds rect
	pGetClientRect.Call(v.hwnd, uintptr(unsafe.Pointer(&bounds)))
	return float64(bounds.right) / scale, float64(bounds.bottom) / scale, scale
}

func (v *winView) layoutControls() {
	if v.ball == 0 {
		return
	}
	width, height, scale := v.ballArea()
	x, y := v.controls.position.Origin(width, height)
	size := uintptr(math.Round(controlball.Size * scale))
	pMoveWindow.Call(v.ball, uintptr(math.Round(x*scale)), uintptr(math.Round(y*scale)), size, size, 1)
	if size == v.controls.diameter {
		return
	}
	rgn, _, _ := pCreateEllipticRgn.Call(0, 0, size, size)
	if ok, _, _ := pSetWindowRgn.Call(v.ball, rgn, 1); ok == 0 {
		pDeleteObject.Call(rgn)
	} else {
		v.controls.diameter = size
	}
}

type winPoint struct{ x, y int32 }

func (v *winView) openControls() {
	_, _, scale := v.ballArea()
	at := winPoint{y: int32(v.controls.diameter) + int32(math.Round(4*scale))}
	pMapWindowPoints.Call(v.ball, v.parent, uintptr(unsafe.Pointer(&at)), 1)
	v.blur()
	v.cx.Emit(ControlsEvent, core.Point{X: core.Points(float64(at.x) / scale), Y: core.Points(float64(at.y) / scale)})
}

func ballWindowProc(hwnd, msg, wp, lp, parent, data uintptr) uintptr {
	if v := views[parent]; v != nil && hwnd == v.ball {
		switch msg {
		case wmLButtonDown, wmLButtonDbl, wmMouseMove, wmLButtonUp:
			v.ballPointer(msg, lp)
			return 0
		case 0x0215: // WM_CAPTURECHANGED, including a cancelled drag
			v.controls.tracking = false
		}
	}
	r, _, _ := pDefSubclassProc.Call(hwnd, msg, wp, lp)
	return r
}

func (v *winView) ballPointer(msg, lp uintptr) {
	b := &v.controls
	start := msg == wmLButtonDown || msg == wmLButtonDbl
	if !start && !b.tracking {
		return
	}
	width, height, scale := v.ballArea()
	pt := winPoint{int32(int16(lp)), int32(int16(lp >> 16))}
	grab := core.Point{X: core.Points(float64(pt.x) / scale), Y: core.Points(float64(pt.y) / scale)}
	pMapWindowPoints.Call(v.ball, v.hwnd, uintptr(unsafe.Pointer(&pt)), 1)
	x, y := float64(pt.x)/scale, float64(pt.y)/scale
	if start {
		b.down = core.Point{X: core.Points(x), Y: core.Points(y)}
		b.grab = grab
		b.tracking, b.dragging = true, false
		pSetCapture.Call(v.ball)
		return
	}
	if math.Hypot(x-float64(b.down.X), y-float64(b.down.Y)) >= controlball.DragThreshold {
		b.dragging = true
	}
	released := msg == wmLButtonUp
	if b.dragging {
		b.position = b.position.Move(x-float64(b.grab.X), y-float64(b.grab.Y), width, height, released)
		v.layoutControls()
	}
	if released {
		b.tracking = false
		pReleaseCapture.Call()
		if !b.dragging && grab.X >= 0 && grab.Y >= 0 && grab.X < controlball.Size && grab.Y < controlball.Size {
			v.openControls()
		}
	}
}

type drawItem struct {
	controlType, controlID, itemID, action, state uint32
	window, dc                                    uintptr
	bounds                                        rect
	data                                          uintptr
}

func (v *winView) drawControls(param uintptr) bool {
	item := (*drawItem)(unsafe.Pointer(param))
	if item.window != v.ball || v.ball == 0 {
		return false
	}
	const dcBrush = 18
	brush, _, _ := pGetStockObject.Call(dcBrush)
	color := uintptr(0x303030)
	if item.state&1 != 0 {
		color = 0x555555
	}
	old, _, _ := pSetDCBrushColor.Call(item.dc, 0xb0b0b0)
	defer pSetDCBrushColor.Call(item.dc, old)
	pFillRect.Call(item.dc, uintptr(unsafe.Pointer(&item.bounds)), brush)
	size := uintptr(item.bounds.right - item.bounds.left)
	ellipse := func(x, y, w uintptr) {
		r, _, _ := pCreateEllipticRgn.Call(x, y, x+w, y+w)
		pFillRgn.Call(item.dc, r, brush)
		pDeleteObject.Call(r)
	}
	pSetDCBrushColor.Call(item.dc, color)
	ellipse(1, 1, size-2)
	pSetDCBrushColor.Call(item.dc, 0xffffff)
	d := size / 10
	for _, center := range []uintptr{size / 3, size / 2, 2 * size / 3} {
		ellipse(center-d/2, (size-d)/2, d)
	}
	return true
}
