// Package encoder provides hardware video encoding of the screen.
// On macOS: ScreenCaptureKit + VideoToolbox H.264 (see vt_darwin.m).
package encoder

import "errors"

var ErrUnsupported = errors.New("当前平台暂不支持硬件视频编码")

// Frame is one H.264 access unit in Annex-B format. Keyframes carry SPS/PPS.
type Frame struct {
	Data []byte
	Key  bool
}

type Video interface {
	Frames() <-chan Frame
	// Done is closed when capture stops on its own (Err says why).
	Done() <-chan struct{}
	Err() error
	RequestKeyframe()
	Size() (w, h int)
	Close()
}

type Options struct {
	Display  int
	MaxWidth int
	FPS      int
	Quality  int // 10..95, mapped to a bitrate
}

// Bitrate maps the UI quality setting to bits per second.
func Bitrate(w, h, fps, quality int) int {
	bpp := 0.08
	switch {
	case quality <= 40:
		bpp = 0.04
	case quality >= 90:
		bpp = 0.16
	}
	// screen content is mostly static, so scale sub-linearly with fps
	effFPS := float64(fps)
	if effFPS > 15 {
		effFPS = 15 + (effFPS-15)/2
	}
	br := int(float64(w*h) * effFPS * bpp)
	return min(max(br, 500_000), 40_000_000)
}

// StartH264 starts hardware capture+encode, or returns ErrUnsupported.
func StartH264(o Options) (Video, error) { return startH264(o) }
