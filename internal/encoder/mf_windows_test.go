//go:build windows && (amd64 || arm64)

package encoder

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// Run on a Windows machine with a GPU (not over RDP, not as a service):
//
//	go test ./internal/encoder/ -run TestMediaFoundation -v
//	RG_DUMP_H264=out.h264 go test ./internal/encoder/ -run TestMediaFoundation -v && ffprobe out.h264
func TestMediaFoundation(t *testing.T) {
	v, err := StartH264(Options{Display: 0, MaxWidth: 1920, FPS: 30, Quality: 70})
	if err != nil {
		t.Skipf("hardware encoder unavailable: %v", err)
	}
	defer v.Close()
	w, h := v.Size()
	t.Logf("encoding %dx%d", w, h)

	var all bytes.Buffer
	n, keys := 0, 0
	deadline := time.After(3 * time.Second)
	kfAt := time.After(1500 * time.Millisecond)
loop:
	for {
		select {
		case f := <-v.Frames():
			if n == 0 {
				if !f.Key || !hasNAL(f.Data, 7) || !hasNAL(f.Data, 8) || !hasNAL(f.Data, 5) {
					t.Fatalf("first frame must be IDR with SPS/PPS (key=%v)", f.Key)
				}
			}
			if f.Key {
				keys++
			}
			n++
			all.Write(f.Data)
		case <-kfAt:
			v.RequestKeyframe() // must work even if the screen is static
		case <-v.Done():
			t.Fatalf("encoder stopped: %v", v.Err())
		case <-deadline:
			break loop
		}
	}
	t.Logf("%d frames, %d keyframes, %d KB", n, keys, all.Len()/1024)
	if keys < 2 {
		t.Errorf("expected the requested keyframe, got %d keyframes", keys)
	}
	if out := os.Getenv("RG_DUMP_H264"); out != "" {
		os.WriteFile(out, all.Bytes(), 0o644)
	}
}
