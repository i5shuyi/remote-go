// Package display finds the host's monitors: through X11, Quartz or GDI
// with the screenshot package, and on Wayland through the portal's
// screen-sharing session.
package display

import (
	"context"
	"fmt"
	"image"

	"github.com/kbinani/screenshot"

	"github.com/i5shuyi/remote-go/internal/portal"
)

// Prepare readies the screen for sharing. On Wayland it starts the
// portal session now, so its permission dialog shows while someone is at
// the machine; input asks for keyboard and pointer too.
func Prepare(input bool) {
	if portal.Wayland() {
		portal.Prepare(input)
	}
}

// Count returns the number of monitors, or -1 when only the portal knows
// (it reports the monitors the person chose to share).
func Count() int {
	if portal.Wayland() {
		return -1
	}
	return screenshot.NumActiveDisplays()
}

// Bounds returns monitor i's rectangle. On Wayland it waits for the
// portal session and uses the compositor's logical coordinates.
func Bounds(ctx context.Context, i int) (image.Rectangle, error) {
	if !portal.Wayland() {
		return screenshot.GetDisplayBounds(i), nil
	}
	s, err := portal.Get(ctx)
	if err != nil {
		return image.Rectangle{}, err
	}
	streams := s.Streams()
	if i >= len(streams) {
		return image.Rectangle{}, fmt.Errorf("共享的屏幕只有 %d 个，没有编号 %d", len(streams), i)
	}
	return streams[i].Bounds, nil
}
