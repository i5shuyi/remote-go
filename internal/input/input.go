// Package input injects mouse and keyboard events on the host machine.
//
// Coordinates are absolute screen coordinates in the same space as
// screenshot.GetDisplayBounds. Keys are identified by the browser's
// KeyboardEvent.code (e.g. "KeyA", "Enter", "ShiftLeft"), which is layout
// independent, and mapped to a platform keycode in each implementation.
package input

import "errors"

// Mouse buttons, numbered as in the browser's MouseEvent.button.
const (
	ButtonLeft   = 0
	ButtonMiddle = 1
	ButtonRight  = 2
)

type Injector interface {
	MouseMove(x, y float64)
	MouseButton(button int, down bool, x, y float64)
	// Wheel takes browser-style pixel deltas (positive dy = scroll down).
	Wheel(dx, dy float64)
	Key(code string, down bool)
	// Text types s as Unicode characters, independent of the keyboard
	// layout: what a phone's soft keyboard (or an IME) produces.
	Text(s string)
	// ReleaseAll lifts any held buttons/keys, e.g. when a viewer disconnects.
	ReleaseAll()
}

var ErrUnsupported = errors.New("当前平台暂不支持输入注入")

// New returns the platform injector. It is implemented per OS.
func New() (Injector, error) { return newInjector() }

// held tracks pressed keys/buttons so ReleaseAll can undo them.
type held struct {
	keys    map[string]bool
	buttons map[int]bool
}

func newHeld() held { return held{keys: map[string]bool{}, buttons: map[int]bool{}} }
