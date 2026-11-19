package host

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func fakeCapture(r image.Rectangle) (*image.RGBA, error) {
	img := image.NewRGBA(image.Rect(0, 0, 2560, 1440))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	return img, nil
}

func TestStream(t *testing.T) {
	captureRect = fakeCapture
	s, err := New(Config{Name: "rg-test", Code: "123456", ViewOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// index page served
	resp, err := http.Get(ts.URL + "/")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("index: %v %v", err, resp)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?code="

	if _, resp, err := websocket.Dial(ctx, wsURL+"000000", nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong code should be rejected with 401, got %v", err)
	}

	c, _, err := websocket.Dial(ctx, wsURL+"123456", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(32 << 20)

	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("info: %v %v", typ, err)
	}
	var info map[string]any
	json.Unmarshal(data, &info)
	if info["t"] != "info" || info["name"] != "rg-test" {
		t.Fatalf("bad info %s", data)
	}

	// no codec=h264 requested → JPEG announced
	if _, data, _ = c.Read(ctx); !strings.Contains(string(data), `"codec":"jpeg"`) {
		t.Fatalf("expected jpeg video msg, got %s", data)
	}
	c.Write(ctx, websocket.MessageText, []byte(`{"t":"cfg","w":1280,"q":50,"fps":30}`))
	// first frame may predate the cfg; wait for one at the requested width
	for i := 0; ; i++ {
		typ, data, err = c.Read(ctx)
		if err != nil || typ != websocket.MessageBinary || data[0] != frameJPEG {
			t.Fatalf("frame: %v %v", typ, err)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data[1:]))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Width == 1280 && cfg.Height == 720 {
			t.Logf("frame %d: %dx%d, %d bytes", i, cfg.Width, cfg.Height, len(data))
			break
		}
		if i > 10 {
			t.Fatalf("never got scaled frame, last %dx%d", cfg.Width, cfg.Height)
		}
	}
}

func TestLockout(t *testing.T) {
	s := &Server{cfg: Config{Code: "111111"}}
	for i := 0; i < 5; i++ {
		s.checkCode("x")
	}
	if err := s.checkCode("111111"); err == nil {
		t.Fatal("correct code should be locked out after 5 failures")
	}
}
