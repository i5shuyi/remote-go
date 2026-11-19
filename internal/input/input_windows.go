//go:build windows

package input

import (
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procSendInput        = user32.NewProc("SendInput")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procSetDPIAware      = user32.NewProc("SetProcessDPIAware")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseMove       = 0x0001
	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	mouseRightDown  = 0x0008
	mouseRightUp    = 0x0010
	mouseMiddleDown = 0x0020
	mouseMiddleUp   = 0x0040
	mouseWheel      = 0x0800
	mouseHWheel     = 0x1000
	mouseVirtual    = 0x4000
	mouseAbsolute   = 0x8000

	keyExtended = 0x0001
	keyUp       = 0x0002
	keyUnicode  = 0x0004

	smXVirtual  = 76
	smYVirtual  = 77
	smCXVirtual = 78
	smCYVirtual = 79
)

// mouseInput / keybdInput are full INPUT structs (40 bytes on amd64/arm64).
type mouseInput struct {
	typ       uint32
	_         uint32
	dx, dy    int32
	mouseData uint32
	flags     uint32
	time      uint32
	extra     uintptr
}

type keybdInput struct {
	typ   uint32
	_     uint32
	vk    uint16
	scan  uint16
	flags uint32
	time  uint32
	extra uintptr
	_     [8]byte
}

type winInjector struct {
	mu   sync.Mutex
	h    held
	x, y float64
}

func newInjector() (Injector, error) {
	// Make GetSystemMetrics / screenshot bounds use physical pixels.
	procSetDPIAware.Call()
	return &winInjector{h: newHeld()}, nil
}

func metric(i int) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(i))
	return int32(r)
}

func (w *winInjector) send(in unsafe.Pointer, size uintptr) {
	procSendInput.Call(1, uintptr(in), size)
}

func (w *winInjector) mouse(flags uint32, x, y float64, data int32) {
	vx, vy := float64(metric(smXVirtual)), float64(metric(smYVirtual))
	vw, vh := float64(metric(smCXVirtual)), float64(metric(smCYVirtual))
	in := mouseInput{typ: inputMouse, flags: flags | mouseAbsolute | mouseVirtual | mouseMove,
		mouseData: uint32(data)}
	if vw > 1 && vh > 1 {
		in.dx = int32((x - vx) * 65535 / (vw - 1))
		in.dy = int32((y - vy) * 65535 / (vh - 1))
	}
	w.send(unsafe.Pointer(&in), unsafe.Sizeof(in))
}

func (w *winInjector) MouseMove(x, y float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.x, w.y = x, y
	w.mouse(0, x, y, 0)
}

func (w *winInjector) MouseButton(button int, down bool, x, y float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.button(button, down, x, y)
}

func (w *winInjector) button(button int, down bool, x, y float64) {
	var f uint32
	switch button {
	case ButtonLeft:
		f = pickU(down, mouseLeftDown, mouseLeftUp)
	case ButtonRight:
		f = pickU(down, mouseRightDown, mouseRightUp)
	case ButtonMiddle:
		f = pickU(down, mouseMiddleDown, mouseMiddleUp)
	default:
		return
	}
	w.h.buttons[button] = down
	w.x, w.y = x, y
	w.mouse(f, x, y, 0)
}

func (w *winInjector) Wheel(dx, dy float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Browser pixel delta ~100 per notch; Windows WHEEL_DELTA is 120.
	if dy != 0 {
		w.mouse(mouseWheel, w.x, w.y, int32(-dy*1.2))
	}
	if dx != 0 {
		w.mouse(mouseHWheel, w.x, w.y, int32(dx*1.2))
	}
}

func (w *winInjector) Key(code string, down bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.key(code, down)
}

func (w *winInjector) key(code string, down bool) {
	vk, ok := winKeys[code]
	if !ok {
		return
	}
	w.h.keys[code] = down
	in := keybdInput{typ: inputKeyboard, vk: vk}
	if !down {
		in.flags |= keyUp
	}
	if winExtended[code] {
		in.flags |= keyExtended
	}
	w.send(unsafe.Pointer(&in), unsafe.Sizeof(in))
}

// Text sends each UTF-16 unit as a KEYEVENTF_UNICODE press and release.
func (w *winInjector) Text(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, u := range utf16.Encode([]rune(s)) {
		if u == '\n' {
			u = '\r'
		}
		for _, f := range []uint32{keyUnicode, keyUnicode | keyUp} {
			in := keybdInput{typ: inputKeyboard, scan: u, flags: f}
			w.send(unsafe.Pointer(&in), unsafe.Sizeof(in))
		}
	}
}

func (w *winInjector) ReleaseAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for b, on := range w.h.buttons {
		if on {
			w.button(b, false, w.x, w.y)
		}
	}
	for k, on := range w.h.keys {
		if on {
			w.key(k, false)
		}
	}
}

func pickU(c bool, a, b uint32) uint32 {
	if c {
		return a
	}
	return b
}

var winExtended = map[string]bool{
	"ArrowLeft": true, "ArrowRight": true, "ArrowUp": true, "ArrowDown": true,
	"Home": true, "End": true, "PageUp": true, "PageDown": true, "Insert": true, "Delete": true,
	"ControlRight": true, "AltRight": true, "MetaLeft": true, "MetaRight": true,
	"NumpadEnter": true, "NumpadDivide": true, "ContextMenu": true,
}

var winKeys = func() map[string]uint16 {
	m := map[string]uint16{
		"Backspace": 0x08, "Tab": 0x09, "Enter": 0x0D, "NumpadEnter": 0x0D, "Escape": 0x1B,
		"Space": 0x20, "PageUp": 0x21, "PageDown": 0x22, "End": 0x23, "Home": 0x24,
		"ArrowLeft": 0x25, "ArrowUp": 0x26, "ArrowRight": 0x27, "ArrowDown": 0x28,
		"PrintScreen": 0x2C, "Insert": 0x2D, "Delete": 0x2E, "CapsLock": 0x14,
		"ShiftLeft": 0xA0, "ShiftRight": 0xA1, "ControlLeft": 0xA2, "ControlRight": 0xA3,
		"AltLeft": 0xA4, "AltRight": 0xA5, "MetaLeft": 0x5B, "MetaRight": 0x5C, "ContextMenu": 0x5D,
		"Semicolon": 0xBA, "Equal": 0xBB, "Comma": 0xBC, "Minus": 0xBD, "Period": 0xBE,
		"Slash": 0xBF, "Backquote": 0xC0, "BracketLeft": 0xDB, "Backslash": 0xDC,
		"BracketRight": 0xDD, "Quote": 0xDE, "IntlBackslash": 0xE2,
		"NumpadMultiply": 0x6A, "NumpadAdd": 0x6B, "NumpadSubtract": 0x6D,
		"NumpadDecimal": 0x6E, "NumpadDivide": 0x6F, "NumLock": 0x90, "ScrollLock": 0x91, "Pause": 0x13,
	}
	for c := 'A'; c <= 'Z'; c++ {
		m["Key"+string(c)] = uint16(c)
	}
	for d := '0'; d <= '9'; d++ {
		m["Digit"+string(d)] = uint16(d)
		m["Numpad"+string(d)] = uint16(0x60 + d - '0')
	}
	for i := 1; i <= 12; i++ {
		m["F"+itoa(i)] = uint16(0x70 + i - 1)
	}
	return m
}()

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return "1" + string(rune('0'+i-10))
}
