// Package portal drives a Wayland desktop through xdg-desktop-portal: a
// ScreenCast session gives the screen as PipeWire streams, and a
// RemoteDesktop session adds keyboard and pointer input on the same
// streams. The compositor asks the person at the machine to allow the
// session once; its restore token, saved in the user's config directory,
// lets later sessions start without asking until they revoke it.
package portal

import (
	"errors"
	"image"
	"os"
)

// ErrUnavailable is returned off Linux and when no portal answers.
var ErrUnavailable = errors.New("xdg-desktop-portal 不可用")

// Wayland reports whether this process runs in a Wayland session, where
// the screen and input go through the portal instead of X11.
func Wayland() bool {
	switch os.Getenv("XDG_SESSION_TYPE") {
	case "wayland":
		return true
	case "x11":
		return false
	}
	return os.Getenv("WAYLAND_DISPLAY") != "" && os.Getenv("DISPLAY") == ""
}

// Stream is one shared monitor: its PipeWire node and its place in the
// compositor's logical coordinates.
type Stream struct {
	Node   uint32
	Bounds image.Rectangle
}
