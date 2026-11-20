package keymap

// FromEvdev returns the code for a Linux evdev keycode (KEY_*), or "".
// X11 and GDK hardware keycodes are the evdev code plus 8.
func FromEvdev(code uint16) string { return evdev[code] }

// ToEvdev returns the evdev keycode for a code.
func ToEvdev(code string) (uint16, bool) {
	c, ok := evdevCodes[code]
	return c, ok
}

// evdev shares set-1 scancodes for the main block (KEY_ESC = 1 …
// KEY_F12 = 88); the keys Windows prefixes with E0 have codes of their own.
var evdev = func() map[uint16]string {
	m := make(map[uint16]string, len(win)+32)
	for scan, code := range win {
		m[scan] = code
	}
	for c, code := range map[uint16]string{
		96: "NumpadEnter", 97: "ControlRight", 98: "NumpadDivide", 99: "PrintScreen",
		100: "AltRight", 102: "Home", 103: "ArrowUp", 104: "PageUp", 105: "ArrowLeft",
		106: "ArrowRight", 107: "End", 108: "ArrowDown", 109: "PageDown", 110: "Insert",
		111: "Delete", 117: "NumpadEqual", 119: "Pause", 121: "NumpadComma",
		125: "MetaLeft", 126: "MetaRight", 127: "ContextMenu",
		89: "IntlRo", 124: "IntlYen", 92: "Convert", 94: "NonConvert", 93: "KanaMode",
		183: "F13", 184: "F14", 185: "F15", 186: "F16", 187: "F17", 188: "F18",
		189: "F19", 190: "F20", 191: "F21", 192: "F22", 193: "F23", 194: "F24",
	} {
		m[c] = code
	}
	return m
}()

var evdevCodes = func() map[string]uint16 {
	m := make(map[string]uint16, len(evdev))
	for c, code := range evdev {
		m[code] = c
	}
	return m
}()
