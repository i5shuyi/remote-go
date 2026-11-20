//go:build linux && !android

package encoder

import (
	"fmt"
	"sync"
)

// Picture is a decoded frame: BGRA pixels, Width*4 bytes per row.
type Picture struct {
	Pix           []byte
	Width, Height int
}

// decoders are tried in order: VA-API and NVDEC, then FFmpeg and OpenH264.
var decoders = []struct{ name, props string }{
	{"vah264dec", ""},
	{"nvh264dec", ""},
	{"avdec_h264", "max-threads=1"}, // frame threads would add a frame of delay each
	{"openh264dec", ""},
}

var decodeOnce struct {
	sync.Once
	element string
}

// decoderElement returns the GStreamer pipeline segment that decodes
// H.264, or "" when there is none.
func decoderElement() string {
	decodeOnce.Do(func() {
		for _, e := range []string{"appsrc", "appsink", "h264parse", "videoconvert"} {
			if !hasElement(e) {
				return
			}
		}
		for _, d := range decoders {
			if hasElement(d.name) {
				decodeOnce.element = d.name + " " + d.props
				return
			}
		}
	})
	return decodeOnce.element
}

// CanDecodeH264 reports whether GStreamer can decode H.264 here.
func CanDecodeH264() bool { return decoderElement() != "" }

// H264Decoder decodes Annex-B access units with GStreamer. Pictures come
// out on its own goroutine.
type H264Decoder struct {
	out func(*Picture)

	mu   sync.Mutex
	p    *pipeline
	stop chan struct{}
	wg   sync.WaitGroup
}

// NewH264Decoder starts a decoder that calls out with each picture, on
// its own goroutine; out must not keep the picture past the call.
func NewH264Decoder(out func(*Picture)) (*H264Decoder, error) {
	d := &H264Decoder{out: out}
	if err := d.start(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *H264Decoder) start() error {
	dec := decoderElement()
	if dec == "" {
		return fmt.Errorf("%w：GStreamer 没有 H.264 解码器（安装 gstreamer1.0-libav 或 gstreamer1.0-vaapi）", ErrUnsupported)
	}
	p, err := launch("appsrc name=src is-live=true do-timestamp=true format=time" +
		" caps=video/x-h264,stream-format=byte-stream,alignment=au ! h264parse ! " + dec +
		" ! videoconvert ! video/x-raw,format=BGRA ! appsink name=sink sync=false max-buffers=2 drop=true")
	if err != nil {
		return err
	}
	d.p, d.stop = p, make(chan struct{})
	d.wg.Add(1)
	go d.run(p, d.stop)
	return nil
}

func (d *H264Decoder) run(p *pipeline, stop chan struct{}) {
	defer d.wg.Done()
	var pic Picture
	for {
		select {
		case <-stop:
			return
		default:
		}
		s, ok := p.pull(gstSecond / 10)
		if !ok {
			return
		}
		if s == 0 {
			continue
		}
		w, h := sampleSize(s)
		pix := sampleBytes(s)
		gst.miniObjectUnref(s)
		if w > 0 && h > 0 && len(pix) >= w*h*4 {
			pic = Picture{Pix: pix[:w*h*4], Width: w, Height: h}
			d.out(&pic)
		}
	}
}

func (d *H264Decoder) halt() {
	if d.p == nil {
		return
	}
	close(d.stop)
	d.wg.Wait()
	d.p.close()
	d.p = nil
}

// Decode queues one access unit. It reports false when the decoder
// failed and needs a keyframe to restart.
func (d *H264Decoder) Decode(au []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.p != nil && d.p.err0() != nil {
		d.halt()
		return false
	}
	if d.p == nil {
		if d.start() != nil {
			return false
		}
		if !HasIDR(au) {
			return false
		}
	}
	if !d.p.push(au) {
		d.halt()
		return false
	}
	return true
}

// Flush drops decoder state; the next access unit should be a keyframe.
func (d *H264Decoder) Flush() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.halt()
}

func (d *H264Decoder) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.halt()
}
