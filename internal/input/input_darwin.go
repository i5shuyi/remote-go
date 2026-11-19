//go:build darwin && cgo

package input

/*
#cgo LDFLAGS: -framework ApplicationServices -framework CoreGraphics
#include <ApplicationServices/ApplicationServices.h>

static void rg_mouse(int type, double x, double y, int button, int clicks, unsigned long long flags) {
	CGEventRef e = CGEventCreateMouseEvent(NULL, (CGEventType)type, CGPointMake(x, y), (CGMouseButton)button);
	if (!e) return;
	if (clicks > 0) CGEventSetIntegerValueField(e, kCGMouseEventClickState, clicks);
	CGEventSetFlags(e, (CGEventFlags)flags);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

static void rg_scroll(int dy, int dx) {
	CGEventRef e = CGEventCreateScrollWheelEvent(NULL, kCGScrollEventUnitPixel, 2, dy, dx);
	if (!e) return;
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

static void rg_key(int code, int down, unsigned long long flags) {
	CGEventRef e = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)code, down ? true : false);
	if (!e) return;
	CGEventSetFlags(e, (CGEventFlags)flags);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

// rg_text types UTF-16 text in chunks of 20 (the event's limit).
static void rg_text(const UniChar *s, int n) {
	for (int i = 0; i < n; i += 20) {
		int m = n - i < 20 ? n - i : 20;
		for (int down = 1; down >= 0; down--) {
			CGEventRef e = CGEventCreateKeyboardEvent(NULL, 0, down ? true : false);
			if (!e) return;
			CGEventSetFlags(e, 0);
			CGEventKeyboardSetUnicodeString(e, m, s + i);
			CGEventPost(kCGHIDEventTap, e);
			CFRelease(e);
		}
	}
}

static int rg_ax_trusted(int prompt) {
	if (!prompt) return AXIsProcessTrusted();
	const void *keys[] = { kAXTrustedCheckOptionPrompt };
	const void *vals[] = { kCFBooleanTrue };
	CFDictionaryRef opts = CFDictionaryCreate(NULL, keys, vals, 1,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	int ok = AXIsProcessTrustedWithOptions(opts);
	CFRelease(opts);
	return ok;
}

static int rg_screen_access(int request) {
	if (CGPreflightScreenCaptureAccess()) return 1;
	if (request) return CGRequestScreenCaptureAccess();
	return 0;
}
*/
import "C"

import (
	"log"
	"math"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"
)

// CGEventType values.
const (
	evLeftDown     = 1
	evLeftUp       = 2
	evRightDown    = 3
	evRightUp      = 4
	evMoved        = 5
	evLeftDragged  = 6
	evRightDragged = 7
	evOtherDown    = 25
	evOtherUp      = 26
	evOtherDragged = 27
)

// CGEventFlags masks.
const (
	flagShift   = 0x00020000
	flagControl = 0x00040000
	flagOption  = 0x00080000
	flagCommand = 0x00100000
)

type darwinInjector struct {
	mu        sync.Mutex
	h         held
	lastX     float64
	lastY     float64
	lastClick time.Time
	lastBtn   int
	clicks    int
}

func newInjector() (Injector, error) {
	if C.rg_screen_access(1) == 0 {
		log.Printf("⚠️  未获得「屏幕录制」权限：系统设置 → 隐私与安全性 → 屏幕录制，勾选运行本程序的终端/应用后重启程序")
	}
	if C.rg_ax_trusted(1) == 0 {
		log.Printf("⚠️  未获得「辅助功能」权限：系统设置 → 隐私与安全性 → 辅助功能，勾选运行本程序的终端/应用（否则无法控制鼠标键盘）")
	}
	return &darwinInjector{h: newHeld()}, nil
}

func (d *darwinInjector) flags() C.ulonglong {
	var f uint64
	k := d.h.keys
	if k["ShiftLeft"] || k["ShiftRight"] {
		f |= flagShift
	}
	if k["ControlLeft"] || k["ControlRight"] {
		f |= flagControl
	}
	if k["AltLeft"] || k["AltRight"] {
		f |= flagOption
	}
	if k["MetaLeft"] || k["MetaRight"] {
		f |= flagCommand
	}
	return C.ulonglong(f)
}

func (d *darwinInjector) MouseMove(x, y float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastX, d.lastY = x, y
	typ, btn := evMoved, 0
	switch {
	case d.h.buttons[ButtonLeft]:
		typ = evLeftDragged
	case d.h.buttons[ButtonRight]:
		typ, btn = evRightDragged, 1
	case d.h.buttons[ButtonMiddle]:
		typ, btn = evOtherDragged, 2
	}
	C.rg_mouse(C.int(typ), C.double(x), C.double(y), C.int(btn), 0, d.flags())
}

func (d *darwinInjector) MouseButton(button int, down bool, x, y float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.button(button, down, x, y)
}

func (d *darwinInjector) button(button int, down bool, x, y float64) {
	var typ, cgBtn int
	switch button {
	case ButtonLeft:
		typ, cgBtn = pick(down, evLeftDown, evLeftUp), 0
	case ButtonRight:
		typ, cgBtn = pick(down, evRightDown, evRightUp), 1
	case ButtonMiddle:
		typ, cgBtn = pick(down, evOtherDown, evOtherUp), 2
	default:
		return
	}
	if down {
		// macOS needs an explicit click count for double/triple clicks.
		now := time.Now()
		if button == d.lastBtn && now.Sub(d.lastClick) < 400*time.Millisecond &&
			math.Abs(x-d.lastX) < 5 && math.Abs(y-d.lastY) < 5 {
			d.clicks++
		} else {
			d.clicks = 1
		}
		d.lastClick, d.lastBtn = now, button
	}
	d.lastX, d.lastY = x, y
	d.h.buttons[button] = down
	C.rg_mouse(C.int(typ), C.double(x), C.double(y), C.int(cgBtn), C.int(d.clicks), d.flags())
}

func (d *darwinInjector) Wheel(dx, dy float64) {
	C.rg_scroll(C.int(-dy), C.int(-dx))
}

func (d *darwinInjector) Key(code string, down bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.key(code, down)
}

func (d *darwinInjector) key(code string, down bool) {
	kc, ok := macKeys[code]
	if !ok {
		return
	}
	d.h.keys[code] = down
	C.rg_key(C.int(kc), boolInt(down), d.flags())
}

func (d *darwinInjector) Text(s string) {
	u := utf16.Encode([]rune(s))
	if len(u) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	C.rg_text((*C.UniChar)(unsafe.Pointer(&u[0])), C.int(len(u)))
}

func (d *darwinInjector) ReleaseAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for b, on := range d.h.buttons {
		if on {
			d.button(b, false, d.lastX, d.lastY)
		}
	}
	for k, on := range d.h.keys {
		if on {
			d.key(k, false)
		}
	}
}

func pick(c bool, a, b int) int {
	if c {
		return a
	}
	return b
}

func boolInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// macKeys maps KeyboardEvent.code to macOS virtual keycodes (kVK_*).
var macKeys = map[string]uint16{
	"KeyA": 0x00, "KeyS": 0x01, "KeyD": 0x02, "KeyF": 0x03, "KeyH": 0x04, "KeyG": 0x05,
	"KeyZ": 0x06, "KeyX": 0x07, "KeyC": 0x08, "KeyV": 0x09, "KeyB": 0x0B, "KeyQ": 0x0C,
	"KeyW": 0x0D, "KeyE": 0x0E, "KeyR": 0x0F, "KeyY": 0x10, "KeyT": 0x11, "KeyO": 0x1F,
	"KeyU": 0x20, "KeyI": 0x22, "KeyP": 0x23, "KeyL": 0x25, "KeyJ": 0x26, "KeyK": 0x28,
	"KeyN": 0x2D, "KeyM": 0x2E,
	"Digit1": 0x12, "Digit2": 0x13, "Digit3": 0x14, "Digit4": 0x15, "Digit6": 0x16,
	"Digit5": 0x17, "Digit9": 0x19, "Digit7": 0x1A, "Digit8": 0x1C, "Digit0": 0x1D,
	"Equal": 0x18, "Minus": 0x1B, "BracketRight": 0x1E, "BracketLeft": 0x21,
	"Quote": 0x27, "Semicolon": 0x29, "Backslash": 0x2A, "Comma": 0x2B, "Slash": 0x2C,
	"Period": 0x2F, "Backquote": 0x32, "IntlBackslash": 0x0A,
	"Enter": 0x24, "Tab": 0x30, "Space": 0x31, "Backspace": 0x33, "Escape": 0x35,
	"MetaRight": 0x36, "MetaLeft": 0x37, "ShiftLeft": 0x38, "CapsLock": 0x39,
	"AltLeft": 0x3A, "ControlLeft": 0x3B, "ShiftRight": 0x3C, "AltRight": 0x3D,
	"ControlRight": 0x3E,
	"F1":           0x7A, "F2": 0x78, "F3": 0x63, "F4": 0x76, "F5": 0x60, "F6": 0x61,
	"F7": 0x62, "F8": 0x64, "F9": 0x65, "F10": 0x6D, "F11": 0x67, "F12": 0x6F,
	"Home": 0x73, "PageUp": 0x74, "Delete": 0x75, "End": 0x77, "PageDown": 0x79,
	"ArrowLeft": 0x7B, "ArrowRight": 0x7C, "ArrowDown": 0x7D, "ArrowUp": 0x7E,
	"Numpad0": 0x52, "Numpad1": 0x53, "Numpad2": 0x54, "Numpad3": 0x55, "Numpad4": 0x56,
	"Numpad5": 0x57, "Numpad6": 0x58, "Numpad7": 0x59, "Numpad8": 0x5B, "Numpad9": 0x5C,
	"NumpadDecimal": 0x41, "NumpadMultiply": 0x43, "NumpadAdd": 0x45, "NumpadDivide": 0x4B,
	"NumpadEnter": 0x4C, "NumpadSubtract": 0x4E, "NumpadEqual": 0x51,
}
