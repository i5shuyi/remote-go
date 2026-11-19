package host

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/i5shuyi/remote-go/internal/input"
)

type updateTestInput struct {
	input.Injector
	keys chan bool
}

func (i updateTestInput) Key(_ string, down bool) { i.keys <- down }
func (updateTestInput) ReleaseAll()               {}

func TestUpdateKeepsViewerAndHeldKey(t *testing.T) {
	keys := make(chan bool, 2)
	s := &Server{capture: fakeCapture, cfg: Config{Code: "test"}, inj: updateTestInput{keys: keys}, sessions: make(map[int]*liveSession)}
	// httptest.Close does not wait for hijacked sockets. Wait for the
	// streaming handler to release admission before testing idle retirement.
	finished := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWS(w, r)
		if r.URL.Path == "/live" {
			close(finished)
		}
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	c, _, err := websocket.Dial(ctx, url+"/live?code=test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.CloseNow(); <-finished }()
	c.SetReadLimit(32 << 20)
	frame := func() {
		t.Helper()
		for {
			kind, data, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if kind == websocket.MessageBinary && len(data) > 1 && data[0] == frameJPEG {
				return
			}
		}
	}
	key := func(packet string, want bool) {
		t.Helper()
		if err := c.Write(ctx, websocket.MessageText, []byte(packet)); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-keys:
			if got != want {
				t.Fatalf("key down = %v", got)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	frame()
	key(`{"t":"kd","c":"KeyA"}`, true)
	if s.DrainIfIdle() {
		t.Fatal("retired an active viewer")
	}
	key(`{"t":"ku","c":"KeyA"}`, false)
	frame()
	c.CloseNow()
	<-finished
	if !s.DrainIfIdle() {
		t.Fatal("idle host did not retire")
	}
	_, resp, err := websocket.Dial(ctx, url+"/new?code=test", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("admission after retirement: %v %v", resp, err)
	}
}
