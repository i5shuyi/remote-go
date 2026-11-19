//go:build darwin && cgo

package host

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/i5shuyi/remote-go/internal/encoder"
	"github.com/i5shuyi/remote-go/internal/tunnel"
)

// TestServeTailcat serves the fake H.264 stream through a real tailcat
// tunnel, for checking the desktop app end to end:
//
//	RG_TAILCAT_HOST=1 go test ./internal/host -run TestServeTailcat -v -timeout 40m
func TestServeTailcat(t *testing.T) {
	if os.Getenv("RG_TAILCAT_HOST") == "" {
		t.Skip("set RG_TAILCAT_HOST=1 (needs the internet)")
	}
	frames, err := encoder.SelfTest(1280, 720, 30)
	if err != nil {
		t.Fatal(err)
	}
	startVideo = func(o encoder.Options) (encoder.Video, error) { return newFakeVideo(frames), nil }
	captureRect = fakeCapture
	s, _ := New(Config{Name: "测试主机", Code: "test2345", ViewOnly: true})
	ctx := context.Background()
	region, err := tunnel.NearestRegion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := tunnel.NewDeviceCode(region)
	ln, err := tunnel.Listen(ctx, code, s.Code(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fmt.Printf("TAILCAT-HOST %s %s\n", code, s.Code())
	go s.Serve(ln)
	time.Sleep(30 * time.Minute)
}
