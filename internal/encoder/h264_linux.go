//go:build linux && !android

package encoder

// Linux capture and encode: GStreamer grabs the screen (ximagesrc on X11,
// pipewiresrc on the portal's screen-sharing stream on Wayland) and the
// first H.264 encoder that starts encodes it, hardware first (VA-API,
// NVENC), then x264 or OpenH264 in software.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kbinani/screenshot"

	"github.com/i5shuyi/remote-go/internal/portal"
)

type gstEncoder struct {
	name   string // GStreamer element
	format string // raw format it takes
	props  func(kbps, gop int) string
}

var encoders = []gstEncoder{
	{"vah264enc", "NV12", func(kbps, gop int) string {
		return fmt.Sprintf("rate-control=cbr bitrate=%d key-int-max=%d b-frames=0", kbps, gop)
	}},
	{"vaapih264enc", "NV12", func(kbps, gop int) string {
		return fmt.Sprintf("rate-control=cbr bitrate=%d keyframe-period=%d max-bframes=0", kbps, gop)
	}},
	{"nvh264enc", "NV12", func(kbps, gop int) string {
		return fmt.Sprintf("rc-mode=cbr preset=low-latency-hq bitrate=%d gop-size=%d", kbps, gop)
	}},
	{"x264enc", "I420", func(kbps, gop int) string {
		return fmt.Sprintf("tune=zerolatency speed-preset=ultrafast bitrate=%d key-int-max=%d bframes=0", kbps, gop)
	}},
	{"openh264enc", "I420", func(kbps, gop int) string {
		return fmt.Sprintf("rate-control=bitrate complexity=low bitrate=%d gop-size=%d", kbps*1000, gop)
	}},
}

type gstVideo struct {
	p      *pipeline
	w, h   int
	frames chan Frame
	done   chan struct{}
	stop   chan struct{}
	wg     sync.WaitGroup

	mu     sync.Mutex // guards p against Close
	err    error
	closed bool
	fd     int // the PipeWire remote, -1 on X11
}

func startH264(o Options) (Video, error) {
	if err := loadGst(); err != nil {
		return nil, err
	}
	fps := o.FPS
	if fps <= 0 {
		fps = 30
	}
	var (
		source string
		sw, sh int // the source's size in pixels
		fd     = -1
	)
	if portal.Wayland() {
		var err error
		if fd, source, sw, sh, err = pipewireSource(o.Display, fps); err != nil {
			return nil, err
		}
	} else {
		if os.Getenv("DISPLAY") == "" {
			return nil, fmt.Errorf("%w：没有 X11 显示", ErrUnsupported)
		}
		b := screenshot.GetDisplayBounds(o.Display)
		if b.Empty() {
			return nil, fmt.Errorf("显示器 %d 不存在", o.Display)
		}
		source = fmt.Sprintf("ximagesrc use-damage=false show-pointer=true startx=%d starty=%d endx=%d endy=%d"+
			" ! video/x-raw,framerate=%d/1", b.Min.X, b.Min.Y, b.Max.X-1, b.Max.Y-1, fps)
		sw, sh = b.Dx(), b.Dy()
	}
	closeFD := func() {
		if fd >= 0 {
			syscall.Close(fd)
		}
	}
	w, h := sw, sh
	if o.MaxWidth > 0 && w > o.MaxWidth {
		h = h * o.MaxWidth / w
		w = o.MaxWidth
	}
	w, h = max(w&^1, 2), max(h&^1, 2)
	kbps := max(Bitrate(w, h, fps, o.Quality)/1000, 1)
	gop := fps * 10 // keyframes on request; this is the safety net

	var errs []string
	for _, enc := range encoders {
		if !hasElement(enc.name) {
			continue
		}
		desc := fmt.Sprintf("%s ! queue max-size-buffers=1 leaky=downstream"+
			" ! videoscale ! videoconvert ! video/x-raw,format=%s,width=%d,height=%d,pixel-aspect-ratio=1/1"+
			" ! %s %s ! h264parse config-interval=-1 ! video/x-h264,stream-format=byte-stream,alignment=au"+
			" ! appsink name=sink sync=false max-buffers=8",
			source, enc.format, w, h, enc.name, enc.props(kbps, gop))
		v, err := openVideo(desc, w, h)
		if err == nil {
			v.fd = fd
			return v, nil
		}
		errs = append(errs, enc.name+": "+err.Error())
	}
	closeFD()
	if len(errs) == 0 {
		return nil, fmt.Errorf("%w：GStreamer 没有可用的 H.264 编码器（安装 gstreamer1.0-vaapi、gst-plugins-bad 或 gst-plugins-ugly）", ErrUnsupported)
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

// pipewireSource opens the portal's stream for display and measures it:
// the portal gives logical sizes, the stream scaled monitors' pixels.
func pipewireSource(display, fps int) (fd int, source string, w, h int, err error) {
	if !hasElement("pipewiresrc") {
		return -1, "", 0, 0, fmt.Errorf("%w：GStreamer 缺少 pipewiresrc（安装 gstreamer1.0-pipewire）", ErrUnsupported)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	s, err := portal.Get(ctx)
	if err != nil {
		return -1, "", 0, 0, err
	}
	streams := s.Streams()
	if display >= len(streams) {
		return -1, "", 0, 0, fmt.Errorf("共享的屏幕只有 %d 个，没有编号 %d", len(streams), display)
	}
	if fd, err = s.PipeWire(); err != nil {
		return -1, "", 0, 0, err
	}
	// pipewiresrc duplicates the descriptor; ours stays open for restarts
	// and is closed with the video.
	src := fmt.Sprintf("pipewiresrc fd=%d path=%d do-timestamp=true always-copy=true keepalive-time=1000", fd, streams[display].Node)
	w, h, err = probeSize(src + " ! video/x-raw ! appsink name=sink sync=false max-buffers=1")
	if err != nil {
		syscall.Close(fd)
		return -1, "", 0, 0, err
	}
	return fd, src + fmt.Sprintf(" ! video/x-raw ! videorate drop-only=true max-rate=%d", fps), w, h, nil
}

// probeSize runs desc until its first sample and returns that sample's size.
func probeSize(desc string) (int, int, error) {
	p, err := launch(desc)
	if err != nil {
		return 0, 0, err
	}
	defer p.close()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		s, ok := p.pull(gstSecond / 10)
		if s != 0 {
			w, h := sampleSize(s)
			gst.miniObjectUnref(s)
			if w > 0 && h > 0 {
				return w, h, nil
			}
		}
		if err := p.err0(); err != nil || !ok {
			if err == nil {
				err = errors.New("屏幕共享流已结束")
			}
			return 0, 0, err
		}
	}
	return 0, 0, errors.New("屏幕共享流没有画面")
}

// openVideo starts desc and waits for its first frame, so an encoder that
// cannot open its device lets the next one try.
func openVideo(desc string, w, h int) (*gstVideo, error) {
	p, err := launch(desc)
	if err != nil {
		return nil, err
	}
	if err := p.waitPlaying(3 * gstSecond); err != nil {
		p.close()
		return nil, err
	}
	var first []byte
	for deadline := time.Now().Add(3 * time.Second); first == nil; {
		s, ok := p.pull(gstSecond / 10)
		if s != 0 {
			first = sampleBytes(s)
			gst.miniObjectUnref(s)
		}
		if err := p.err0(); err != nil || !ok {
			p.close()
			if err == nil {
				err = errors.New("视频流已结束")
			}
			return nil, err
		}
		if time.Now().After(deadline) {
			p.close()
			return nil, errors.New("编码器没有输出")
		}
	}
	v := &gstVideo{p: p, w: w, h: h, fd: -1, frames: make(chan Frame, 4), done: make(chan struct{}), stop: make(chan struct{})}
	v.wg.Add(1)
	go v.run(first)
	return v, nil
}

func (v *gstVideo) run(first []byte) {
	defer v.wg.Done()
	send := func(data []byte) bool {
		select {
		case v.frames <- Frame{Data: data, Key: HasIDR(data)}:
			return true
		case <-v.stop:
			return false
		}
	}
	if !send(first) {
		return
	}
	for {
		select {
		case <-v.stop:
			return
		default:
		}
		s, ok := v.p.pull(gstSecond / 10)
		if s != 0 {
			data := sampleBytes(s)
			gst.miniObjectUnref(s)
			if len(data) > 0 && !send(data) {
				return
			}
		}
		if err := v.p.err0(); err != nil || !ok {
			if err == nil {
				err = errors.New("视频流已结束")
			}
			v.mu.Lock()
			v.err = err
			v.mu.Unlock()
			close(v.done)
			return
		}
	}
}

func (v *gstVideo) Frames() <-chan Frame  { return v.frames }
func (v *gstVideo) Done() <-chan struct{} { return v.done }
func (v *gstVideo) Size() (int, int)      { return v.w, v.h }

func (v *gstVideo) Err() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.err
}

func (v *gstVideo) RequestKeyframe() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.closed {
		v.p.forceKeyframe()
	}
}

func (v *gstVideo) Close() {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	v.mu.Unlock()
	close(v.stop)
	v.wg.Wait()
	v.p.close()
	if v.fd >= 0 {
		syscall.Close(v.fd)
	}
}
