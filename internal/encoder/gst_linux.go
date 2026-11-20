//go:build linux && !android

package encoder

// GStreamer bindings, loaded with purego so the binary still starts on a
// machine without GStreamer (it then streams JPEG).

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	gstStateNull    = 1
	gstStatePlaying = 4

	gstStateChangeFailure = 0

	gstMessageEOS   = 1 << 0
	gstMessageError = 1 << 1

	gstMapRead = 1

	gstSecond        = uint64(1_000_000_000)
	gstClockTimeNone = ^uint64(0)
)

// gstMapInfo is GstMapInfo; Go lays it out as C does.
type gstMapInfo struct {
	memory   uintptr
	flags    int32
	data     unsafe.Pointer
	size     uintptr
	maxsize  uintptr
	user     [4]uintptr
	reserved [4]uintptr
}

var gst struct {
	once sync.Once
	err  error

	initCheck         func(argc, argv, gerr uintptr) int32
	parseLaunch       func(desc string, gerr *uintptr) uintptr
	binGetByName      func(bin uintptr, name string) uintptr
	elementSetState   func(el uintptr, state int32) int32
	elementGetState   func(el uintptr, state, pending *int32, timeout uint64) int32
	elementGetBus     func(el uintptr) uintptr
	elementSendEvent  func(el, event uintptr) int32
	busTimedPop       func(bus uintptr, timeout uint64, types int32) uintptr
	messageParseError func(msg uintptr, gerr *uintptr, debug *uintptr)
	miniObjectUnref   func(obj uintptr)
	objectUnref       func(obj uintptr)
	factoryFind       func(name string) uintptr
	sampleGetBuffer   func(sample uintptr) uintptr
	sampleGetCaps     func(sample uintptr) uintptr
	capsGetStructure  func(caps uintptr, index uint32) uintptr
	structureGetInt   func(s uintptr, field string, v *int32) int32
	bufferMap         func(buf uintptr, info *gstMapInfo, flags int32) int32
	bufferUnmap       func(buf uintptr, info *gstMapInfo)
	bufferNewAllocate func(alloc uintptr, size uintptr, params uintptr) uintptr
	bufferFill        func(buf uintptr, offset uintptr, src unsafe.Pointer, size uintptr) uintptr

	appSinkTryPull func(sink uintptr, timeout uint64) uintptr
	appSinkIsEOS   func(sink uintptr) int32
	appSrcPush     func(src, buf uintptr) int32
	forceKeyUnit   func(runningTime uint64, allHeaders int32, count uint32) uintptr
	gErrorFree     func(err uintptr)
	gFree          func(p uintptr)
}

func dlopen(names ...string) (uintptr, error) {
	var err error
	for _, n := range names {
		var h uintptr
		if h, err = purego.Dlopen(n, purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
			return h, nil
		}
	}
	return 0, err
}

// loadGst loads GStreamer once and initializes it.
func loadGst() error {
	gst.once.Do(func() {
		core, err := dlopen("libgstreamer-1.0.so.0", "libgstreamer-1.0.so")
		if err != nil {
			gst.err = fmt.Errorf("%w：未安装 GStreamer（%v）", ErrUnsupported, err)
			return
		}
		app, err := dlopen("libgstapp-1.0.so.0", "libgstapp-1.0.so")
		if err != nil {
			gst.err = fmt.Errorf("%w：未安装 gst-plugins-base（%v）", ErrUnsupported, err)
			return
		}
		video, err := dlopen("libgstvideo-1.0.so.0", "libgstvideo-1.0.so")
		if err != nil {
			gst.err = fmt.Errorf("%w：未安装 gst-plugins-base（%v）", ErrUnsupported, err)
			return
		}
		glib, err := dlopen("libglib-2.0.so.0", "libglib-2.0.so")
		if err != nil {
			gst.err = err
			return
		}
		for _, f := range []struct {
			lib uintptr
			fn  any
			sym string
		}{
			{core, &gst.initCheck, "gst_init_check"},
			{core, &gst.parseLaunch, "gst_parse_launch"},
			{core, &gst.binGetByName, "gst_bin_get_by_name"},
			{core, &gst.elementSetState, "gst_element_set_state"},
			{core, &gst.elementGetState, "gst_element_get_state"},
			{core, &gst.elementGetBus, "gst_element_get_bus"},
			{core, &gst.elementSendEvent, "gst_element_send_event"},
			{core, &gst.busTimedPop, "gst_bus_timed_pop_filtered"},
			{core, &gst.messageParseError, "gst_message_parse_error"},
			{core, &gst.miniObjectUnref, "gst_mini_object_unref"},
			{core, &gst.objectUnref, "gst_object_unref"},
			{core, &gst.factoryFind, "gst_element_factory_find"},
			{core, &gst.sampleGetBuffer, "gst_sample_get_buffer"},
			{core, &gst.sampleGetCaps, "gst_sample_get_caps"},
			{core, &gst.capsGetStructure, "gst_caps_get_structure"},
			{core, &gst.structureGetInt, "gst_structure_get_int"},
			{core, &gst.bufferMap, "gst_buffer_map"},
			{core, &gst.bufferUnmap, "gst_buffer_unmap"},
			{core, &gst.bufferNewAllocate, "gst_buffer_new_allocate"},
			{core, &gst.bufferFill, "gst_buffer_fill"},
			{app, &gst.appSinkTryPull, "gst_app_sink_try_pull_sample"},
			{app, &gst.appSinkIsEOS, "gst_app_sink_is_eos"},
			{app, &gst.appSrcPush, "gst_app_src_push_buffer"},
			{video, &gst.forceKeyUnit, "gst_video_event_new_upstream_force_key_unit"},
			{glib, &gst.gErrorFree, "g_error_free"},
			{glib, &gst.gFree, "g_free"},
		} {
			if _, err := purego.Dlsym(f.lib, f.sym); err != nil {
				gst.err = fmt.Errorf("%w：GStreamer 缺少 %s", ErrUnsupported, f.sym)
				return
			}
			purego.RegisterLibFunc(f.fn, f.lib, f.sym)
		}
		if gst.initCheck(0, 0, 0) == 0 {
			gst.err = fmt.Errorf("%w：GStreamer 初始化失败", ErrUnsupported)
		}
	})
	return gst.err
}

// hasElement reports whether GStreamer has the element factory name.
func hasElement(name string) bool {
	if loadGst() != nil {
		return false
	}
	f := gst.factoryFind(name)
	if f == 0 {
		return false
	}
	gst.objectUnref(f)
	return true
}

// gerror turns a GError into an error and frees it.
func gerror(p uintptr) error {
	if p == 0 {
		return errors.New("GStreamer 错误")
	}
	// GError is {GQuark domain; gint code; gchar *message}.
	msg := gostring(*(*uintptr)(unsafe.Add(cptr(p), 8)))
	gst.gErrorFree(p)
	return errors.New(msg)
}

// cptr turns an address GStreamer returned into a pointer; the memory is
// C's, so the garbage collector never moves it.
func cptr(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }

func gostring(p uintptr) string {
	if p == 0 {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(cptr(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(cptr(p)), n))
}

// pipeline is a running gst_parse_launch pipeline with an appsink named
// "sink" (and an appsrc named "src", if it has one).
type pipeline struct {
	el, sink, src, bus uintptr
}

func launch(desc string) (*pipeline, error) {
	if err := loadGst(); err != nil {
		return nil, err
	}
	var gerr uintptr
	el := gst.parseLaunch(desc, &gerr)
	if el == 0 || gerr != 0 {
		err := gerror(gerr)
		if el != 0 {
			gst.objectUnref(el)
		}
		return nil, err
	}
	p := &pipeline{el: el, sink: gst.binGetByName(el, "sink"), src: gst.binGetByName(el, "src"), bus: gst.elementGetBus(el)}
	if p.sink == 0 {
		p.close()
		return nil, errors.New("GStreamer 管线没有 appsink")
	}
	if gst.elementSetState(el, gstStatePlaying) == gstStateChangeFailure {
		err := p.err()
		p.close()
		return nil, err
	}
	return p, nil
}

// waitPlaying waits for a live pipeline to start, so a hardware element
// that cannot open its device fails here rather than mid-stream.
func (p *pipeline) waitPlaying(timeout uint64) error {
	var state, pending int32
	if gst.elementGetState(p.el, &state, &pending, timeout) == gstStateChangeFailure {
		return p.err()
	}
	return p.err0()
}

// err returns the pipeline's first error message, or a generic error.
func (p *pipeline) err() error {
	if err := p.err0(); err != nil {
		return err
	}
	return errors.New("GStreamer 管线无法启动")
}

// err0 returns a pending error or end of stream, nil when there is none.
func (p *pipeline) err0() error {
	msg := gst.busTimedPop(p.bus, 0, gstMessageError|gstMessageEOS)
	if msg == 0 {
		return nil
	}
	defer gst.miniObjectUnref(msg)
	// GstMessage starts with a GstMiniObject; its type field follows it.
	var gerr, debug uintptr
	typ := *(*int32)(unsafe.Add(cptr(msg), miniObjectSize))
	if typ&gstMessageError == 0 {
		return errors.New("视频流已结束")
	}
	gst.messageParseError(msg, &gerr, &debug)
	if debug != 0 {
		gst.gFree(debug)
	}
	return gerror(gerr)
}

// miniObjectSize is sizeof(GstMiniObject): GType, two gints, guint
// flags, three function pointers, a guint and a pointer.
var miniObjectSize = func() uintptr {
	p := unsafe.Sizeof(uintptr(0))
	size := p + 4 + 4 + 4 // type, refcount, lockstate, flags
	size = (size + p - 1) &^ (p - 1)
	size += 3 * p // copy, dispose, free
	size += 4     // priv_uint
	size = (size + p - 1) &^ (p - 1)
	size += p // priv_pointer
	return size
}()

// pull waits up to timeout for the sink's next sample; ok is false at the
// end of the stream.
func (p *pipeline) pull(timeout uint64) (sample uintptr, ok bool) {
	s := gst.appSinkTryPull(p.sink, timeout)
	if s == 0 && gst.appSinkIsEOS(p.sink) != 0 {
		return 0, false
	}
	return s, true
}

// sampleBytes copies a sample's buffer.
func sampleBytes(sample uintptr) []byte {
	buf := gst.sampleGetBuffer(sample)
	if buf == 0 {
		return nil
	}
	var info gstMapInfo
	if gst.bufferMap(buf, &info, gstMapRead) == 0 {
		return nil
	}
	out := make([]byte, info.size)
	copy(out, unsafe.Slice((*byte)(info.data), info.size))
	gst.bufferUnmap(buf, &info)
	return out
}

// sampleSize reads width and height from a sample's caps.
func sampleSize(sample uintptr) (w, h int) {
	caps := gst.sampleGetCaps(sample)
	if caps == 0 {
		return 0, 0
	}
	s := gst.capsGetStructure(caps, 0)
	var cw, ch int32
	if s == 0 || gst.structureGetInt(s, "width", &cw) == 0 || gst.structureGetInt(s, "height", &ch) == 0 {
		return 0, 0
	}
	return int(cw), int(ch)
}

// push copies data into a buffer for the appsrc.
func (p *pipeline) push(data []byte) bool {
	if p.src == 0 || len(data) == 0 {
		return false
	}
	buf := gst.bufferNewAllocate(0, uintptr(len(data)), 0)
	if buf == 0 {
		return false
	}
	gst.bufferFill(buf, 0, unsafe.Pointer(&data[0]), uintptr(len(data)))
	return gst.appSrcPush(p.src, buf) == 0 // GST_FLOW_OK; takes the buffer
}

func (p *pipeline) forceKeyframe() {
	if ev := gst.forceKeyUnit(gstClockTimeNone, 1, 0); ev != 0 {
		gst.elementSendEvent(p.sink, ev)
	}
}

func (p *pipeline) close() {
	if p.el != 0 {
		gst.elementSetState(p.el, gstStateNull)
	}
	for _, o := range []uintptr{p.sink, p.src, p.bus, p.el} {
		if o != 0 {
			gst.objectUnref(o)
		}
	}
	*p = pipeline{}
}
