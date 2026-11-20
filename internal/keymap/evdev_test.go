package keymap

import "testing"

func TestEvdev(t *testing.T) {
	for code, want := range map[uint16]string{
		1: "Escape", 30: "KeyA", 28: "Enter", 96: "NumpadEnter", 103: "ArrowUp",
		125: "MetaLeft", 100: "AltRight", 88: "F12", 86: "IntlBackslash",
	} {
		if got := FromEvdev(code); got != want {
			t.Errorf("FromEvdev(%d) = %q, want %q", code, got, want)
		}
		if back, ok := ToEvdev(want); !ok || back != code {
			t.Errorf("ToEvdev(%q) = %d, %v, want %d", want, back, ok, code)
		}
	}
	if len(evdev) != len(evdevCodes) {
		t.Fatalf("evdev maps %d codes to %d names: a name repeats", len(evdev), len(evdevCodes))
	}
}
