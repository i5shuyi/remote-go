package portal

import (
	"errors"
	"image"

	"github.com/godbus/dbus/v5"
)

// parseStreams reads Start's "streams", a(ua{sv}): each PipeWire node
// with its "position" and "size" in logical pixels.
func parseStreams(v dbus.Variant) ([]Stream, error) {
	var raw []struct {
		Node  uint32
		Props map[string]dbus.Variant
	}
	if v.Signature().String() == "" || v.Store(&raw) != nil || len(raw) == 0 {
		return nil, errors.New("没有选择要共享的屏幕")
	}
	streams := make([]Stream, 0, len(raw))
	x := 0
	for _, r := range raw {
		var pos, size struct{ X, Y int32 }
		if p, found := r.Props["size"]; found {
			p.Store(&size)
		}
		posKnown := false
		if p, found := r.Props["position"]; found && p.Store(&pos) == nil {
			posKnown = true
		}
		if !posKnown {
			// Lay monitors without a position side by side.
			pos.X, pos.Y = int32(x), 0
		}
		b := image.Rect(int(pos.X), int(pos.Y), int(pos.X+size.X), int(pos.Y+size.Y))
		x = max(x, b.Max.X)
		streams = append(streams, Stream{Node: r.Node, Bounds: b})
	}
	return streams, nil
}
