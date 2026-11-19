//go:build darwin && cgo

package host

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/i5shuyi/remote-go/internal/encoder"
)

// fakeVideo replays real VideoToolbox output in a loop at 30 fps.
type fakeVideo struct {
	frames []encoder.Frame
	ch     chan encoder.Frame
	done   chan struct{}
	once   sync.Once
	kf     chan struct{}
}

func newFakeVideo(frames []encoder.Frame) *fakeVideo {
	v := &fakeVideo{frames: frames, ch: make(chan encoder.Frame), done: make(chan struct{}), kf: make(chan struct{}, 1)}
	go func() {
		i := 0
		t := time.NewTicker(time.Second / 30)
		defer t.Stop()
		for {
			select {
			case <-v.done:
				return
			case <-v.kf:
				i = 0
			case <-t.C:
			}
			select {
			case v.ch <- v.frames[i]:
			case <-v.done:
				return
			}
			if i = i + 1; i == len(v.frames) {
				i = 0
			}
		}
	}()
	return v
}

func (v *fakeVideo) Frames() <-chan encoder.Frame { return v.ch }
func (v *fakeVideo) Done() <-chan struct{}        { return v.done }
func (v *fakeVideo) Err() error                   { return nil }
func (v *fakeVideo) Size() (int, int)             { return 1280, 720 }
func (v *fakeVideo) RequestKeyframe()             { signal(v.kf) }
func (v *fakeVideo) Close()                       { v.once.Do(func() { close(v.done) }) }

func TestH264Protocol(t *testing.T) {
	frames, err := encoder.SelfTest(1280, 720, 30)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	startVideo = func(o encoder.Options) (encoder.Video, error) {
		starts++
		return newFakeVideo(frames), nil
	}
	captureRect = fakeCapture
	s, _ := New(Config{Name: "rg-test", Code: "123456", ViewOnly: true})

	if addr := os.Getenv("RG_E2E_ADDR"); addr != "" {
		// Manual browser check: open http://<addr>/, code 123456.
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: s.Handler()}
		go srv.Serve(ln)
		time.Sleep(90 * time.Second)
		srv.Close()
		return
	}

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws?code=123456&codec=h264", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(8 << 20)

	readJSON := func(want string) map[string]any {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if typ == websocket.MessageText {
				var m map[string]any
				json.Unmarshal(data, &m)
				if m["t"] == want {
					return m
				}
			}
		}
	}
	readFrame := func() byte {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if typ == websocket.MessageBinary {
				return data[0]
			}
		}
	}

	readJSON("info")
	if m := readJSON("video"); m["codec"] != "h264" {
		t.Fatalf("expected h264, got %v", m)
	}
	if f := readFrame(); f != frameH264Key {
		t.Fatalf("first frame should be keyframe, got %d", f)
	}
	if f := readFrame(); f != frameH264Diff {
		t.Fatalf("second frame should be delta, got %d", f)
	}

	// keyframe request is routed to the encoder
	c.Write(ctx, websocket.MessageText, []byte(`{"t":"kf"}`))
	gotKey := false
	for i := 0; i < 10 && !gotKey; i++ {
		gotKey = readFrame() == frameH264Key
	}
	if !gotKey {
		t.Fatal("no keyframe after kf request")
	}

	// changing settings restarts the encoder and re-announces the stream
	c.Write(ctx, websocket.MessageText, []byte(`{"t":"cfg","w":1280,"q":90,"fps":30}`))
	readJSON("video")
	if starts != 2 {
		t.Fatalf("expected encoder restart, starts=%d", starts)
	}
}

func TestH264FallbackToJPEG(t *testing.T) {
	startVideo = func(encoder.Options) (encoder.Video, error) { return nil, errors.New("no permission") }
	captureRect = fakeCapture
	s, _ := New(Config{Name: "rg-test", Code: "123456", ViewOnly: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws?code=123456&codec=h264", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(8 << 20)
	sawJPEGMsg := false
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.MessageText && strings.Contains(string(data), `"codec":"jpeg"`) {
			sawJPEGMsg = true
		}
		if typ == websocket.MessageBinary {
			if !sawJPEGMsg || data[0] != frameJPEG {
				t.Fatalf("expected JPEG fallback, got frame type %d (announced=%v)", data[0], sawJPEGMsg)
			}
			return
		}
	}
}
