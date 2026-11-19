//go:build windows && (amd64 || arm64)

package screen

// Windows remote-screen view: a child window of NSGo's, painting decoded
// frames with GDI (letterboxed, halftone-scaled) and taking raw mouse and
// keyboard input by scancode.

import (
	"bytes"
	"image"
	"image/draw"
	"image/jpeg"
	"sync"
	"syscall"
	"unsafe"

	"github.com/nsgo-dev/nsgo/core"

	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/encoder"
	"github.com/i5shuyi/remote-go/internal/keymap"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pRegisterClassEx  = user32.NewProc("RegisterClassExW")
	pCreateWindowEx   = user32.NewProc("CreateWindowExW")
	pDestroyWindow    = user32.NewProc("DestroyWindow")
	pDefWindowProc    = user32.NewProc("DefWindowProcW")
	pBeginPaint       = user32.NewProc("BeginPaint")
	pEndPaint         = user32.NewProc("EndPaint")
	pGetClientRect    = user32.NewProc("GetClientRect")
	pFillRect         = user32.NewProc("FillRect")
	pInvalidateRect   = user32.NewProc("InvalidateRect")
	pSetFocus         = user32.NewProc("SetFocus")
	pSetCapture       = user32.NewProc("SetCapture")
	pReleaseCapture   = user32.NewProc("ReleaseCapture")
	pLoadCursor       = user32.NewProc("LoadCursorW")
	pGetModuleHandle  = kernel32.NewProc("GetModuleHandleW")
	pStretchDIBits    = gdi32.NewProc("StretchDIBits")
	pSetStretchBltMod = gdi32.NewProc("SetStretchBltMode")
	pSetBrushOrgEx    = gdi32.NewProc("SetBrushOrgEx")
	pGetStockObject   = gdi32.NewProc("GetStockObject")
)

const (
	wsPopup        = 0x80000000
	wsClipSiblings = 0x04000000

	wmDestroy     = 0x0002
	wmPaint       = 0x000F
	wmEraseBkgnd  = 0x0014
	wmKillFocus   = 0x0008
	wmGetDlgCode  = 0x0087
	wmKeyDown     = 0x0100
	wmKeyUp       = 0x0101
	wmChar        = 0x0102
	wmSysKeyDown  = 0x0104
	wmSysKeyUp    = 0x0105
	wmSysChar     = 0x0106
	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmLButtonDbl  = 0x0203
	wmRButtonDown = 0x0204
	wmRButtonUp   = 0x0205
	wmRButtonDbl  = 0x0206
	wmMButtonDown = 0x0207
	wmMButtonUp   = 0x0208
	wmMButtonDbl  = 0x0209
	wmMouseWheel  = 0x020A
	wmMouseHWheel = 0x020E

	dlgcWantAllKeys = 0x0004
	halftone        = 4
	blackBrush      = 4
	idcArrow        = 32512
	srcCopy         = 0x00CC0020
)

type wndClassEx struct {
	size, style            uint32
	proc                   uintptr
	clsExtra, wndExtra     int32
	instance, icon, cursor uintptr
	background             uintptr
	menuName, className    *uint16
	iconSm                 uintptr
}

type rect struct{ left, top, right, bottom int32 }

type paintStruct struct {
	hdc       uintptr
	erase     int32
	paint     rect
	restore   int32
	incUpdate int32
	reserved  [32]byte
}

type bitmapInfoHeader struct {
	size                   uint32
	width, height          int32
	planes, bitCount       uint16
	compression, sizeImage uint32
	xPels, yPels           int32
	clrUsed, clrImportant  uint32
}

var (
	views     = map[uintptr]*winView{} // by hwnd; UI thread only
	className *uint16
)

type winView struct {
	viewBase
	hwnd, ball, parent uintptr
	controls           ballControl
	cx                 *core.NativeContext

	mu     sync.Mutex // guards pix, w, h, dec
	pix    []byte     // BGRA
	w, h   int
	dec    *encoder.H264Decoder
	decErr bool
}

func newView(cx *core.NativeContext) core.NativeComponent {
	instance, _, _ := pGetModuleHandle.Call(0)
	if className == nil {
		className, _ = syscall.UTF16PtrFromString("RemoteGoScreen")
		cursor, _, _ := pLoadCursor.Call(0, idcArrow)
		wc := wndClassEx{proc: syscall.NewCallback(wndProc), instance: instance, cursor: cursor, className: className}
		wc.size = uint32(unsafe.Sizeof(wc))
		pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	}
	v := &winView{cx: cx, controls: newBallControl()}
	// A hidden popup until NSGo makes it a child of its window.
	v.hwnd, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(className)), 0, wsPopup|wsClipSiblings|wsClipChildren,
		0, 0, 640, 360, 0, 0, instance, 0)
	if v.hwnd == 0 {
		return nil
	}
	views[v.hwnd] = v
	v.createControls(instance)
	return v
}

func (v *winView) View() any { return v.hwnd }

func (v *winView) SetProp(name string, value any) {
	if name == "link" {
		id, _ := value.(string)
		v.setLink(v, id)
	}
}

func (v *winView) Command(name string, args any) {
	if name == "focus" {
		v.focusScreen()
	}
}

func (v *winView) Close() {
	if v.parent != 0 {
		pRemoveWindowSubclass.Call(v.parent, parentFocusProc, v.hwnd)
	}
	v.unlink(v)
	v.mu.Lock()
	if v.dec != nil {
		v.dec.Close()
		v.dec = nil
	}
	v.mu.Unlock()
	delete(views, v.hwnd)
	pDestroyWindow.Call(v.hwnd)
}

func (v *winView) frame(kind byte, data []byte) bool {
	switch kind {
	case client.FrameJPEG:
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return true
		}
		b := img.Bounds()
		rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(rgba, rgba.Rect, img, b.Min, draw.Src)
		for i := 0; i+3 < len(rgba.Pix); i += 4 { // RGBA → BGRA
			rgba.Pix[i], rgba.Pix[i+2] = rgba.Pix[i+2], rgba.Pix[i]
		}
		v.show(rgba.Pix, b.Dx(), b.Dy(), false)
		return true
	case client.FrameH264Key, client.FrameH264Diff:
		v.mu.Lock()
		if v.dec == nil && !v.decErr {
			d, err := encoder.NewH264Decoder(func(p *encoder.Picture) { v.show(p.Pix, p.Width, p.Height, true) })
			if err != nil {
				v.decErr = true
			} else {
				v.dec = d
			}
		}
		d := v.dec
		v.mu.Unlock()
		if d == nil {
			return false
		}
		return d.Decode(data)
	}
	return true
}

func (v *winView) reset() {
	v.mu.Lock()
	d := v.dec
	v.mu.Unlock()
	if d != nil {
		d.Flush()
	}
}

// show copies a picture (BGRA) for the next paint.
func (v *winView) show(pix []byte, w, h int, copyPix bool) {
	v.mu.Lock()
	if copyPix {
		if len(v.pix) != len(pix) {
			v.pix = make([]byte, len(pix))
		}
		copy(v.pix, pix)
	} else {
		v.pix = pix
	}
	v.w, v.h = w, h
	v.mu.Unlock()
	pInvalidateRect.Call(v.hwnd, 0, 0)
}

// videoRect is where the picture is drawn in a client area of cw×ch.
func (v *winView) videoRect(cw, ch int32) (x, y, w, h int32) {
	v.mu.Lock()
	vw, vh := v.w, v.h
	v.mu.Unlock()
	if vw <= 0 || vh <= 0 {
		return 0, 0, cw, ch
	}
	s := min(float64(cw)/float64(vw), float64(ch)/float64(vh))
	w, h = int32(float64(vw)*s+0.5), int32(float64(vh)*s+0.5)
	return (cw - w) / 2, (ch - h) / 2, w, h
}

func (v *winView) paint() {
	var ps paintStruct
	hdc, _, _ := pBeginPaint.Call(v.hwnd, uintptr(unsafe.Pointer(&ps)))
	defer pEndPaint.Call(v.hwnd, uintptr(unsafe.Pointer(&ps)))
	var rc rect
	pGetClientRect.Call(v.hwnd, uintptr(unsafe.Pointer(&rc)))
	black, _, _ := pGetStockObject.Call(blackBrush)
	x, y, w, h := v.videoRect(rc.right, rc.bottom)
	bars := []rect{{0, 0, rc.right, y}, {0, y + h, rc.right, rc.bottom}, {0, y, x, y + h}, {x + w, y, rc.right, y + h}}
	for i := range bars {
		if bars[i].right > bars[i].left && bars[i].bottom > bars[i].top {
			pFillRect.Call(hdc, uintptr(unsafe.Pointer(&bars[i])), black)
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.pix) < v.w*v.h*4 || v.w == 0 {
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), black)
		return
	}
	bi := bitmapInfoHeader{width: int32(v.w), height: -int32(v.h), planes: 1, bitCount: 32}
	bi.size = uint32(unsafe.Sizeof(bi))
	pSetStretchBltMod.Call(hdc, halftone)
	pSetBrushOrgEx.Call(hdc, 0, 0, 0)
	pStretchDIBits.Call(hdc, uintptr(x), uintptr(y), uintptr(w), uintptr(h), 0, 0, uintptr(v.w), uintptr(v.h),
		uintptr(unsafe.Pointer(&v.pix[0])), uintptr(unsafe.Pointer(&bi)), 0, srcCopy)
}

func (v *winView) norm(lParam uintptr) (float64, float64) {
	px, py := int32(int16(lParam&0xffff)), int32(int16(lParam>>16&0xffff))
	var rc rect
	pGetClientRect.Call(v.hwnd, uintptr(unsafe.Pointer(&rc)))
	x, y, w, h := v.videoRect(rc.right, rc.bottom)
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	return float64(px-x) / float64(w), float64(py-y) / float64(h)
}

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	v := views[hwnd]
	if v == nil {
		r, _, _ := pDefWindowProc.Call(hwnd, msg, wParam, lParam)
		return r
	}
	switch msg {
	case wmFocusScreen:
		if focused, _, _ := pGetFocus.Call(); focused == v.parent {
			v.focusScreen()
		}
		return 0
	case 0x0005, wmDPIChangedAfterParent: // WM_SIZE, or moved to a different DPI
		v.layoutControls()
	case wmCommand:
		if lParam == v.ball && v.ball != 0 && wParam>>16 == 0 { // BN_CLICKED
			v.openControls()
			return 0
		}
	case wmDrawItem:
		if v.drawControls(lParam) {
			return 1
		}
	case wmPaint:
		if v.parent == 0 {
			v.watchParentFocus()
		}
		v.paint()
		return 0
	case wmEraseBkgnd:
		return 1
	case wmGetDlgCode:
		return dlgcWantAllKeys
	case wmMouseMove:
		v.move(v.norm(lParam))
		return 0
	case wmLButtonDown, wmLButtonDbl, wmRButtonDown, wmRButtonDbl, wmMButtonDown, wmMButtonDbl:
		pSetFocus.Call(hwnd)
		pSetCapture.Call(hwnd)
		x, y := v.norm(lParam)
		v.button(buttonOf(msg), true, x, y)
		return 0
	case wmLButtonUp, wmRButtonUp, wmMButtonUp:
		pReleaseCapture.Call()
		x, y := v.norm(lParam)
		v.button(buttonOf(msg), false, x, y)
		return 0
	case wmMouseWheel:
		v.wheel(0, -float64(int16(wParam>>16))*100/120)
		return 0
	case wmMouseHWheel:
		v.wheel(float64(int16(wParam>>16))*100/120, 0)
		return 0
	case wmKeyDown, wmSysKeyDown, wmKeyUp, wmSysKeyUp:
		scan := uint16(lParam >> 16 & 0xff)
		ext := lParam>>24&1 == 1
		v.key(keymap.FromWinScan(scan, ext), msg == wmKeyDown || msg == wmSysKeyDown)
		return 0
	case wmChar, wmSysChar:
		return 0
	case wmKillFocus:
		v.blur()
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, msg, wParam, lParam)
	return r
}

func buttonOf(msg uintptr) int {
	switch msg {
	case wmRButtonDown, wmRButtonUp, wmRButtonDbl:
		return 2
	case wmMButtonDown, wmMButtonUp, wmMButtonDbl:
		return 1
	}
	return 0
}
