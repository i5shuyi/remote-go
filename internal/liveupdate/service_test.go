package liveupdate

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/i5shuyi/remote-go/internal/backend"
)

// A replacement frontend opens its own control connection. Losing the first
// connection must not quit the host or drop pending or established windows.
func TestPanelReplacementKeepsSessionService(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	s := &service{be: backend.New(), windows: 2, ctx: ctx, stop: stop}
	call := func(op string) Reply {
		t.Helper()
		server, client := net.Pipe()
		done := make(chan struct{})
		go func() { s.serveConn(server); close(done) }()
		defer client.Close()
		if err := json.NewEncoder(client).Encode(Request{Op: op}); err != nil {
			t.Fatal(err)
		}
		var r Reply
		if err := json.NewDecoder(client).Decode(&r); err != nil || r.Error != "" {
			t.Fatalf("%s: %v %s", op, err, r.Error)
		}
		<-done
		return r
	}
	if r := call("state"); r.Windows != 2 {
		t.Fatal("first frontend lost its windows")
	}
	if r := call("retire"); r.Retiring || r.Windows != 2 {
		t.Fatal("replacement interrupted a window")
	}
	if ctx.Err() != nil {
		t.Fatal("closing the old control connection stopped the host")
	}
	if r := call("quit"); !r.Retiring || ctx.Err() == nil {
		t.Fatal("explicit quit did not stop the service")
	}
}
func TestWindowHelperRejectsOtherExecutables(t *testing.T) {
	for _, path := range []string{"", filepath.Join("..", "sessions.exe"), "other.exe", "sessions-abc.exe"} {
		if _, err := windowHelper(path); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
}
