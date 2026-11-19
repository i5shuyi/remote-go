//go:build darwin && cgo

package host

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/encoder"
)

// The desktop app's Go client against a real host.
func TestGoClient(t *testing.T) {
	frames, err := encoder.SelfTest(1280, 720, 30)
	if err != nil {
		t.Fatal(err)
	}
	startVideo = func(o encoder.Options) (encoder.Video, error) { return newFakeVideo(frames), nil }
	captureRect = fakeCapture
	s, _ := New(Config{Name: "rg-test", Code: "123456", ViewOnly: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")
	var d net.Dialer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.Dial(ctx, d.DialContext, addr, "wrong", true, client.Config{}, client.Handler{}); !errors.Is(err, client.ErrBadCode) {
		t.Fatalf("bad code: got %v", err)
	}

	info := make(chan client.Info, 1)
	video := make(chan string, 4)
	kinds := make(chan byte, 64)
	stats := make(chan client.Stats, 4)
	closed := make(chan error, 1)
	sess, err := client.Dial(ctx, d.DialContext, addr, " 123456 ", true, client.Config{Quality: 70, FPS: 30, Width: 1280}, client.Handler{
		OnInfo:  func(i client.Info) { info <- i },
		OnVideo: func(codec string, w, h int) { video <- codec },
		OnFrame: func(k byte, data []byte) {
			select {
			case kinds <- k:
			default:
			}
		},
		OnStats: func(s client.Stats) {
			select {
			case stats <- s:
			default:
			}
		},
		OnClose: func(err error) { closed <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	if i := <-info; i.Name != "rg-test" || i.Control {
		t.Fatalf("info %+v", i)
	}
	if c := <-video; c != "H.264" {
		t.Fatalf("codec %q", c)
	}
	if k := <-kinds; k != client.FrameH264Key {
		t.Fatalf("first frame kind %d", k)
	}
	select {
	case st := <-stats:
		if st.FPS == 0 || st.Codec != "H.264" {
			t.Fatalf("stats %+v", st)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no stats")
	}
	// Input on a view-only host is ignored, not fatal.
	sess.MouseMove(0.5, 0.5)
	sess.Key("KeyA", true)
	sess.ReleaseKeys()
	sess.Close()
	if err := <-closed; err != nil {
		t.Fatalf("close: %v", err)
	}
}

type recInjector struct{ text chan string }

func (r *recInjector) MouseMove(x, y float64)                          {}
func (r *recInjector) MouseButton(button int, down bool, x, y float64) {}
func (r *recInjector) Wheel(dx, dy float64)                            {}
func (r *recInjector) Key(code string, down bool)                      {}
func (r *recInjector) Text(s string)                                   { r.text <- s }
func (r *recInjector) ReleaseAll()                                     {}

// The phone app types through "tx", Unicode text the host injects as is.
func TestTextInput(t *testing.T) {
	frames, err := encoder.SelfTest(640, 360, 30)
	if err != nil {
		t.Fatal(err)
	}
	startVideo = func(o encoder.Options) (encoder.Video, error) { return newFakeVideo(frames), nil }
	captureRect = fakeCapture
	s, _ := New(Config{Name: "rg-test", Code: "123456", ViewOnly: true})
	rec := &recInjector{text: make(chan string, 4)}
	s.inj = rec
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	var d net.Dialer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := client.Dial(ctx, d.DialContext, strings.TrimPrefix(ts.URL, "http://"), "123456", true, client.Config{}, client.Handler{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sess.Text("你好, Remote Go")
	select {
	case got := <-rec.text:
		if got != "你好, Remote Go" {
			t.Fatalf("text %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no text")
	}
}
