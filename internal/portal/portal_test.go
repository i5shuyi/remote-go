package portal

import (
	"image"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestWayland(t *testing.T) {
	for _, c := range []struct {
		session, wayland, x11 string
		want                  bool
	}{
		{"wayland", "wayland-0", ":0", true},
		{"x11", "wayland-0", ":0", false},
		{"", "wayland-0", "", true},
		{"", "wayland-0", ":0", false}, // nested or XWayland-only: keep X11
		{"", "", ":0", false},
	} {
		t.Setenv("XDG_SESSION_TYPE", c.session)
		t.Setenv("WAYLAND_DISPLAY", c.wayland)
		t.Setenv("DISPLAY", c.x11)
		if got := Wayland(); got != c.want {
			t.Errorf("Wayland() with %+v = %v", c, got)
		}
	}
}

func TestParseStreams(t *testing.T) {
	type pair struct{ X, Y int32 }
	v := dbus.MakeVariant([]struct {
		Node  uint32
		Props map[string]dbus.Variant
	}{
		{51, map[string]dbus.Variant{"position": dbus.MakeVariant(pair{1920, 0}), "size": dbus.MakeVariant(pair{1280, 800})}},
		{52, map[string]dbus.Variant{"size": dbus.MakeVariant(pair{1920, 1080})}},
	})
	streams, err := parseStreams(v)
	if err != nil {
		t.Fatal(err)
	}
	want := []Stream{{51, image.Rect(1920, 0, 3200, 800)}, {52, image.Rect(3200, 0, 5120, 1080)}}
	if len(streams) != 2 || streams[0] != want[0] || streams[1] != want[1] {
		t.Fatalf("streams = %v, want %v", streams, want)
	}
	if _, err := parseStreams(dbus.Variant{}); err == nil {
		t.Fatal("no streams parsed without error")
	}
}
