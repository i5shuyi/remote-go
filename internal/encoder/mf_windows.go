//go:build windows && (amd64 || arm64)

package encoder

// Windows hardware pipeline, all on the GPU:
//
//	DXGI Desktop Duplication (BGRA texture)
//	  → CopyResource into our own texture (kept for keyframe re-sends)
//	  → D3D11 Video Processor: BGRA→NV12 + scaling (BT.709, limited range)
//	  → Media Foundation hardware H.264 MFT (NVENC / Intel QSV / AMD AMF),
//	    fed D3D11 surfaces via IMFDXGIDeviceManager (zero copy)
//	  → Annex-B access units → Go
//
// Hardware MFTs are asynchronous: the encoder emits METransformNeedInput /
// METransformHaveOutput events, which we poll without blocking so capture and
// output can be interleaved on one OS thread.

import (
	"errors"
	"fmt"
	"image"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/kbinani/screenshot"
)

func init() {
	// Physical-pixel coordinates so DXGI and screenshot bounds agree.
	pSetProcessDPIAware.Call()
}

type mfVideo struct {
	frames chan Frame
	done   chan struct{}
	exited chan struct{}
	once   sync.Once
	err    error
	w, h   int

	stop    atomic.Bool
	keyReq  atomic.Bool
	waitKey bool // only touched on the pipeline goroutine
}

func startH264(o Options) (Video, error) {
	if err := modMFPlat.Load(); err != nil {
		return nil, fmt.Errorf("Media Foundation 不可用（Windows N 版需安装媒体功能包）: %w", err)
	}
	v := &mfVideo{frames: make(chan Frame, 60), done: make(chan struct{}), exited: make(chan struct{})}
	ready := make(chan error, 1)
	go v.run(o, ready)
	if err := <-ready; err != nil {
		<-v.exited
		return nil, err
	}
	return v, nil
}

func (v *mfVideo) Frames() <-chan Frame  { return v.frames }
func (v *mfVideo) Done() <-chan struct{} { return v.done }
func (v *mfVideo) Err() error            { return v.err }
func (v *mfVideo) Size() (int, int)      { return v.w, v.h }
func (v *mfVideo) RequestKeyframe()      { v.keyReq.Store(true) }

func (v *mfVideo) Close() {
	v.stop.Store(true)
	<-v.exited
	v.once.Do(func() { close(v.done) })
}

func (v *mfVideo) deliver(data []byte, key bool) {
	if key {
		v.waitKey = false
	}
	if v.waitKey {
		return
	}
	select {
	case v.frames <- Frame{Data: data, Key: key}:
	default:
		// consumer too slow: a dropped delta corrupts the stream, so resync
		v.waitKey = true
		v.keyReq.Store(true)
	}
}

func (v *mfVideo) run(o Options, ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(v.exited)

	pCoInitializeEx.Call(0, 0) // COINIT_MULTITHREADED
	defer pCoUninitialize.Call()
	if err := hr("MFStartup", must(pMFStartup.Call(mfVersion, 0))); err != nil {
		ready <- err
		return
	}
	defer pMFShutdown.Call()

	p := &pipeline{v: v, fps: min(max(o.FPS, 1), 60)}
	defer p.release()
	if err := p.init(o); err != nil {
		ready <- err
		return
	}
	v.w, v.h = p.w, p.h
	ready <- nil
	if err := p.loop(); err != nil {
		log.Printf("Windows 硬件编码停止: %v", err)
		v.err = err
		v.once.Do(func() { close(v.done) })
	}
}

func must(r uintptr, _ uintptr, _ error) uintptr { return r }

type nv12Slot struct {
	tex, view *com
}

type pipeline struct {
	v   *mfVideo
	fps int

	output  *com // IDXGIOutput1
	dev     *com // ID3D11Device
	ctx     *com // ID3D11DeviceContext
	vdev    *com // ID3D11VideoDevice
	vctx    *com // ID3D11VideoContext
	dup     *com // IDXGIOutputDuplication
	bgra    *com // our copy of the desktop
	vpEnum  *com
	vp      *com
	inView  *com
	ring    []nv12Slot
	ringIdx int
	mgr     *com // IMFDXGIDeviceManager
	act     *com // IMFActivate of the chosen encoder
	mft     *com // IMFTransform
	gen     *com // IMFMediaEventGenerator
	codec   *com // ICodecAPI (optional)

	srcW, srcH int
	w, h       int
	bitrate    int
	outInfo    mftOutputStreamInfo
	seqHeader  []byte
	start      time.Time
	haveFrame  bool
}

func (p *pipeline) release() {
	if p.mft != nil {
		p.mft.call(vIMFTransform_ProcessMessage, mftMsgEndOfStream, 0)
		p.mft.call(vIMFTransform_ProcessMessage, mftMsgEndStreaming, 0)
	}
	for _, c := range []*com{p.codec, p.gen, p.mft} {
		c.Release()
	}
	if p.act != nil {
		p.act.call(vIMFActivate_ShutdownObject)
		p.act.Release()
	}
	p.mgr.Release()
	for _, s := range p.ring {
		s.view.Release()
		s.tex.Release()
	}
	for _, c := range []*com{p.inView, p.vp, p.vpEnum, p.bgra, p.dup, p.vctx, p.vdev, p.ctx, p.dev, p.output} {
		c.Release()
	}
	*p = pipeline{}
}

func (p *pipeline) init(o Options) error {
	if err := p.openOutput(o.Display); err != nil {
		return err
	}
	if err := p.duplicate(); err != nil {
		return err
	}
	// Encoded size: native pixels scaled to MaxWidth, ≤4096 (H.264 level 5.2), even.
	limit := o.MaxWidth
	if limit <= 0 {
		limit = 1920
	}
	limit = min(limit, 4096)
	p.w, p.h = p.srcW, p.srcH
	if p.w > limit {
		p.h = p.h * limit / p.w
		p.w = limit
	}
	p.w &^= 1
	p.h &^= 1
	p.bitrate = Bitrate(p.w, p.h, p.fps, o.Quality)

	if err := p.setupVideoProcessor(); err != nil {
		return err
	}
	if err := p.setupEncoder(); err != nil {
		return err
	}
	p.start = time.Now()
	log.Printf("Media Foundation 硬件 H.264 %dx%d@%d %d kbps", p.w, p.h, p.fps, p.bitrate/1000)
	return nil
}

// openOutput finds the DXGI output matching the screenshot display bounds,
// and creates the D3D11 device on that output's adapter.
func (p *pipeline) openOutput(display int) error {
	want := screenshot.GetDisplayBounds(display)
	var fac *com
	if err := hr("CreateDXGIFactory1", must(pCreateDXGIFactory1.Call(
		uintptr(unsafe.Pointer(&IID_IDXGIFactory1)), uintptr(unsafe.Pointer(&fac))))); err != nil {
		return err
	}
	defer fac.Release()

	type cand struct {
		adapter, output *com
		bounds          image.Rectangle
	}
	var cands []cand
	for ai := 0; ; ai++ {
		var ad *com
		if hr("EnumAdapters1", fac.call(vIDXGIFactory1_EnumAdapters1, uintptr(ai), uintptr(unsafe.Pointer(&ad)))) != nil {
			break
		}
		for oi := 0; ; oi++ {
			var out *com
			if hr("EnumOutputs", ad.call(vIDXGIAdapter1_EnumOutputs, uintptr(oi), uintptr(unsafe.Pointer(&out)))) != nil {
				break
			}
			var d dxgiOutputDesc
			out.call(vIDXGIOutput1_GetDesc, uintptr(unsafe.Pointer(&d)))
			if d.AttachedToDesktop == 0 {
				out.Release()
				continue
			}
			ad.call(1) // AddRef: each candidate holds its own adapter reference
			r := d.DesktopCoordinates
			cands = append(cands, cand{ad, out, image.Rect(int(r.Left), int(r.Top), int(r.Right), int(r.Bottom))})
		}
		ad.Release()
	}
	if len(cands) == 0 {
		return errors.New("DXGI 没有找到连接的显示器")
	}
	pick := -1
	for i, c := range cands {
		if c.bounds == want {
			pick = i
		}
	}
	if pick < 0 {
		pick = min(display, len(cands)-1)
	}
	for i, c := range cands {
		if i != pick {
			c.output.Release()
			c.adapter.Release()
		}
	}
	adapter, output := cands[pick].adapter, cands[pick].output
	defer adapter.Release()
	defer output.Release()

	var err error
	if p.output, err = output.QI(&IID_IDXGIOutput1); err != nil {
		return fmt.Errorf("需要 Windows 8+（IDXGIOutput1）: %w", err)
	}
	if err := hr("D3D11CreateDevice", must(pD3D11CreateDevice.Call(adapter.u(), 0 /*UNKNOWN*/, 0,
		d3d11CreateBGRASupport|d3d11CreateVideoSupport, 0, 0, d3d11SDKVersion,
		uintptr(unsafe.Pointer(&p.dev)), 0, uintptr(unsafe.Pointer(&p.ctx))))); err != nil {
		return err
	}
	// The MFT uses the device from its own threads.
	if mt, err := p.dev.QI(&IID_ID3D10Multithread); err == nil {
		mt.call(vID3D10Multithread_SetMultithreadProtected, 1)
		mt.Release()
	}
	return nil
}

func (p *pipeline) duplicate() error {
	if err := hr("DuplicateOutput", p.output.call(vIDXGIOutput1_DuplicateOutput, p.dev.u(),
		uintptr(unsafe.Pointer(&p.dup)))); err != nil {
		if hrCode(err) == eAccessDenied {
			return fmt.Errorf("%w：当前处于安全桌面（锁屏/UAC），或进程运行在服务会话中", err)
		}
		return err
	}
	var d dxgiOutduplDesc
	p.dup.call(vIDXGIOutputDuplication_GetDesc, uintptr(unsafe.Pointer(&d)))
	if p.srcW != 0 && (int(d.Width) != p.srcW || int(d.Height) != p.srcH) {
		return fmt.Errorf("显示分辨率已变化 (%dx%d → %dx%d)", p.srcW, p.srcH, d.Width, d.Height)
	}
	p.srcW, p.srcH = int(d.Width), int(d.Height)
	return nil
}

func (p *pipeline) createTexture(w, h int, format, bind uint32) (*com, error) {
	d := texture2DDesc{Width: uint32(w), Height: uint32(h), MipLevels: 1, ArraySize: 1, Format: format,
		SampleCount: 1, BindFlags: bind}
	var t *com
	err := hr("CreateTexture2D", p.dev.call(vID3D11Device_CreateTexture2D, uintptr(unsafe.Pointer(&d)), 0,
		uintptr(unsafe.Pointer(&t))))
	return t, err
}

func (p *pipeline) setupVideoProcessor() error {
	var err error
	if p.bgra, err = p.createTexture(p.srcW, p.srcH, dxgiFormatB8G8R8A8, d3d11BindRenderTarget|d3d11BindShaderResource); err != nil {
		return err
	}
	if p.vdev, err = p.dev.QI(&IID_ID3D11VideoDevice); err != nil {
		return fmt.Errorf("显卡驱动不支持 D3D11 视频处理: %w", err)
	}
	if p.vctx, err = p.ctx.QI(&IID_ID3D11VideoContext); err != nil {
		return err
	}
	cd := vpContentDesc{InRateNum: uint32(p.fps), InRateDen: 1, InputWidth: uint32(p.srcW), InputHeight: uint32(p.srcH),
		OutRateNum: uint32(p.fps), OutRateDen: 1, OutputWidth: uint32(p.w), OutputHeight: uint32(p.h)}
	if err := hr("CreateVideoProcessorEnumerator", p.vdev.call(vID3D11VideoDevice_CreateVideoProcessorEnumerator,
		uintptr(unsafe.Pointer(&cd)), uintptr(unsafe.Pointer(&p.vpEnum)))); err != nil {
		return err
	}
	if err := hr("CreateVideoProcessor", p.vdev.call(vID3D11VideoDevice_CreateVideoProcessor,
		p.vpEnum.u(), 0, uintptr(unsafe.Pointer(&p.vp)))); err != nil {
		return err
	}
	iv := vpInputViewDesc{ViewDimension: 1}
	if err := hr("CreateVideoProcessorInputView", p.vdev.call(vID3D11VideoDevice_CreateVideoProcessorInputView,
		p.bgra.u(), p.vpEnum.u(), uintptr(unsafe.Pointer(&iv)), uintptr(unsafe.Pointer(&p.inView)))); err != nil {
		return err
	}
	// Input: full-range RGB. Output: BT.709 limited range (what browsers assume for HD).
	inCS, outCS := uint32(0), uint32(0x4|0x1<<4)
	p.vctx.call(vID3D11VideoContext_VideoProcessorSetStreamFrameFormat, p.vp.u(), 0, 0 /*PROGRESSIVE*/)
	p.vctx.call(vID3D11VideoContext_VideoProcessorSetStreamColorSpace, p.vp.u(), 0, uintptr(unsafe.Pointer(&inCS)))
	p.vctx.call(vID3D11VideoContext_VideoProcessorSetOutputColorSpace, p.vp.u(), uintptr(unsafe.Pointer(&outCS)))

	// A small ring of NV12 surfaces: the async encoder may still be reading
	// the previous ones.
	for i := 0; i < 6; i++ {
		t, err := p.createTexture(p.w, p.h, dxgiFormatNV12, d3d11BindRenderTarget|d3d11BindVideoEncoder)
		if err != nil {
			if t, err = p.createTexture(p.w, p.h, dxgiFormatNV12, d3d11BindRenderTarget); err != nil {
				return fmt.Errorf("NV12 纹理: %w", err)
			}
		}
		ov := vpOutputViewDesc{ViewDimension: 1}
		var view *com
		if err := hr("CreateVideoProcessorOutputView", p.vdev.call(vID3D11VideoDevice_CreateVideoProcessorOutputView,
			t.u(), p.vpEnum.u(), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&view)))); err != nil {
			t.Release()
			return err
		}
		p.ring = append(p.ring, nv12Slot{t, view})
	}
	return nil
}

func (p *pipeline) setupEncoder() error {
	var token uint32
	if err := hr("MFCreateDXGIDeviceManager", must(pMFCreateDXGIDeviceManager.Call(
		uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&p.mgr))))); err != nil {
		return err
	}
	if err := hr("ResetDevice", p.mgr.call(vIMFDXGIDeviceManager_ResetDevice, p.dev.u(), uintptr(token))); err != nil {
		return err
	}

	in := mftRegisterTypeInfo{MFMediaType_Video, MFVideoFormat_NV12}
	out := mftRegisterTypeInfo{MFMediaType_Video, MFVideoFormat_H264}
	var acts **com
	var n uint32
	args := append(guidArgs(&MFT_CATEGORY_VIDEO_ENCODER), mftEnumHardware|mftEnumSortAndFilter,
		uintptr(unsafe.Pointer(&in)), uintptr(unsafe.Pointer(&out)),
		uintptr(unsafe.Pointer(&acts)), uintptr(unsafe.Pointer(&n)))
	if err := hr("MFTEnumEx", must(pMFTEnumEx.Call(args...))); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("没有找到硬件 H.264 编码器（NVENC/QSV/AMF）")
	}
	list := unsafe.Slice(acts, n)
	defer pCoTaskMemFree.Call(uintptr(unsafe.Pointer(acts)))

	var lastErr error
	for i, a := range list {
		if p.mft != nil {
			a.Release()
			continue
		}
		if err := p.tryEncoder(a); err != nil {
			lastErr = fmt.Errorf("编码器 #%d: %w", i, err)
			log.Printf("跳过 %v", lastErr)
			a.call(vIMFActivate_ShutdownObject)
			a.Release()
			for _, c := range []*com{p.codec, p.gen, p.mft} {
				c.Release()
			}
			p.codec, p.gen, p.mft = nil, nil, nil
			continue
		}
		p.act = a
	}
	if p.mft == nil {
		return lastErr
	}
	return nil
}

func (p *pipeline) tryEncoder(a *com) error {
	if err := hr("ActivateObject", a.call(vIMFActivate_ActivateObject,
		uintptr(unsafe.Pointer(&IID_IMFTransform)), uintptr(unsafe.Pointer(&p.mft)))); err != nil {
		return err
	}
	var attrs *com
	if hr("GetAttributes", p.mft.call(vIMFTransform_GetAttributes, uintptr(unsafe.Pointer(&attrs)))) == nil && attrs != nil {
		attrs.call(vIMFAttributes_SetUINT32, uintptr(unsafe.Pointer(&MF_TRANSFORM_ASYNC_UNLOCK)), 1)
		attrs.call(vIMFAttributes_SetUINT32, uintptr(unsafe.Pointer(&MF_LOW_LATENCY)), 1)
		attrs.Release()
	}
	if err := hr("SET_D3D_MANAGER", p.mft.call(vIMFTransform_ProcessMessage, mftMsgSetD3DManager, p.mgr.u())); err != nil {
		return err
	}

	// Rate control etc. must be set before the media types on some encoders.
	if c, err := p.mft.QI(&IID_ICodecAPI); err == nil {
		p.codec = c
		p.setCodecUI4(&CODECAPI_AVEncCommonRateControlMode, 0) // CBR
		p.setCodecUI4(&CODECAPI_AVEncCommonMeanBitRate, uint32(p.bitrate))
		p.setCodecBool(&CODECAPI_AVLowLatencyMode, true)
		p.setCodecUI4(&CODECAPI_AVEncMPVGOPSize, uint32(p.fps*10))
		p.setCodecUI4(&CODECAPI_AVEncMPVDefaultBPictureCount, 0)
	}

	var err error
	for _, profile := range []uint32{100, 77} { // High, then Main
		if err = p.setOutputType(profile); err == nil {
			break
		}
	}
	if err != nil {
		return err
	}
	if err := p.setInputType(); err != nil {
		return err
	}
	if err := hr("GetOutputStreamInfo", p.mft.call(vIMFTransform_GetOutputStreamInfo, 0,
		uintptr(unsafe.Pointer(&p.outInfo)))); err != nil {
		return err
	}
	if p.gen, err = p.mft.QI(&IID_IMFMediaEventGenerator); err != nil {
		return fmt.Errorf("编码器不是异步 MFT: %w", err)
	}
	if err := hr("BEGIN_STREAMING", p.mft.call(vIMFTransform_ProcessMessage, mftMsgBeginStreaming, 0)); err != nil {
		return err
	}
	return hr("START_OF_STREAM", p.mft.call(vIMFTransform_ProcessMessage, mftMsgStartOfStream, 0))
}

func (p *pipeline) setCodecUI4(g *GUID, v uint32) {
	val := variant{VT: vtUI4, Val: uint64(v)}
	p.codec.call(vICodecAPI_SetValue, uintptr(unsafe.Pointer(g)), uintptr(unsafe.Pointer(&val)))
}

func (p *pipeline) setCodecBool(g *GUID, v bool) {
	val := variant{VT: vtBool}
	if v {
		val.Val = 0xFFFF // VARIANT_TRUE
	}
	p.codec.call(vICodecAPI_SetValue, uintptr(unsafe.Pointer(g)), uintptr(unsafe.Pointer(&val)))
}

func newMediaType() (*com, error) {
	var t *com
	err := hr("MFCreateMediaType", must(pMFCreateMediaType.Call(uintptr(unsafe.Pointer(&t)))))
	return t, err
}

func (p *pipeline) setVideoAttrs(t *com) {
	t.call(vIMFAttributes_SetUINT64, uintptr(unsafe.Pointer(&MF_MT_FRAME_SIZE)), uintptr(uint64(p.w)<<32|uint64(p.h)))
	t.call(vIMFAttributes_SetUINT64, uintptr(unsafe.Pointer(&MF_MT_FRAME_RATE)), uintptr(uint64(p.fps)<<32|1))
	t.call(vIMFAttributes_SetUINT64, uintptr(unsafe.Pointer(&MF_MT_PIXEL_ASPECT_RATIO)), uintptr(uint64(1)<<32|1))
	t.call(vIMFAttributes_SetUINT32, uintptr(unsafe.Pointer(&MF_MT_INTERLACE_MODE)), 2) // progressive
}

func (p *pipeline) setOutputType(profile uint32) error {
	t, err := newMediaType()
	if err != nil {
		return err
	}
	defer t.Release()
	t.call(vIMFAttributes_SetGUID, uintptr(unsafe.Pointer(&MF_MT_MAJOR_TYPE)), uintptr(unsafe.Pointer(&MFMediaType_Video)))
	t.call(vIMFAttributes_SetGUID, uintptr(unsafe.Pointer(&MF_MT_SUBTYPE)), uintptr(unsafe.Pointer(&MFVideoFormat_H264)))
	t.call(vIMFAttributes_SetUINT32, uintptr(unsafe.Pointer(&MF_MT_AVG_BITRATE)), uintptr(p.bitrate))
	t.call(vIMFAttributes_SetUINT32, uintptr(unsafe.Pointer(&MF_MT_MPEG2_PROFILE)), uintptr(profile))
	p.setVideoAttrs(t)
	return hr("SetOutputType", p.mft.call(vIMFTransform_SetOutputType, 0, t.u(), 0))
}

func (p *pipeline) setInputType() error {
	t, err := newMediaType()
	if err != nil {
		return err
	}
	t.call(vIMFAttributes_SetGUID, uintptr(unsafe.Pointer(&MF_MT_MAJOR_TYPE)), uintptr(unsafe.Pointer(&MFMediaType_Video)))
	t.call(vIMFAttributes_SetGUID, uintptr(unsafe.Pointer(&MF_MT_SUBTYPE)), uintptr(unsafe.Pointer(&MFVideoFormat_NV12)))
	t.call(vIMFAttributes_SetUINT32, uintptr(unsafe.Pointer(&MF_MT_YUV_MATRIX)), 1)          // BT.709
	t.call(vIMFAttributes_SetUINT32, uintptr(unsafe.Pointer(&MF_MT_VIDEO_NOMINAL_RANGE)), 2) // 16-235
	p.setVideoAttrs(t)
	err = hr("SetInputType", p.mft.call(vIMFTransform_SetInputType, 0, t.u(), 0))
	t.Release()
	if err == nil {
		return nil
	}
	// Fall back to the encoder's own NV12 type with our size/rate filled in.
	for i := 0; ; i++ {
		var at *com
		if hr("GetInputAvailableType", p.mft.call(vIMFTransform_GetInputAvailableType, 0, uintptr(i),
			uintptr(unsafe.Pointer(&at)))) != nil {
			return err
		}
		at.call(vIMFAttributes_SetGUID, uintptr(unsafe.Pointer(&MF_MT_SUBTYPE)), uintptr(unsafe.Pointer(&MFVideoFormat_NV12)))
		p.setVideoAttrs(at)
		e2 := hr("SetInputType", p.mft.call(vIMFTransform_SetInputType, 0, at.u(), 0))
		at.Release()
		if e2 == nil {
			return nil
		}
	}
}

// ---------------------------------------------------------------- runtime

func (p *pipeline) loop() error {
	interval := time.Second / time.Duration(p.fps)
	next := time.Now()
	needInput := 0
	for !p.v.stop.Load() {
		// 1. drain encoder events without blocking
		for {
			var ev *com
			r := p.gen.call(vIMFMediaEventGenerator_GetEvent, mfEventFlagNoWait, uintptr(unsafe.Pointer(&ev)))
			if uint32(r) == mfENoEventsAvailable {
				break
			}
			if err := hr("GetEvent", r); err != nil {
				return err
			}
			var typ uint32
			ev.call(vIMFMediaEvent_GetType, uintptr(unsafe.Pointer(&typ)))
			ev.Release()
			switch typ {
			case meTransformNeedInput:
				needInput++
			case meTransformHaveOutput:
				if err := p.processOutput(); err != nil {
					return err
				}
			}
		}

		if needInput == 0 {
			time.Sleep(time.Millisecond)
			continue
		}

		// 2. feed the encoder when it wants input and a frame is due
		forceKey := p.v.keyReq.Swap(false)
		now := time.Now()
		if now.Before(next) && !forceKey {
			time.Sleep(min(next.Sub(now), time.Millisecond))
			continue
		}
		timeout := uint32(5)
		if forceKey && p.haveFrame {
			timeout = 0
		}
		got, err := p.acquire(timeout)
		if err != nil {
			return err
		}
		if !got && !(forceKey && p.haveFrame) {
			if forceKey {
				p.v.keyReq.Store(true) // nothing captured yet; retry
			}
			continue
		}
		if forceKey && p.codec != nil {
			p.setCodecUI4(&CODECAPI_AVEncVideoForceKeyFrame, 1)
		}
		if err := p.submit(); err != nil {
			return err
		}
		needInput--
		next = now.Add(interval)
	}
	return nil
}

// acquire grabs the next desktop frame into p.bgra. It returns false if the
// desktop image has not changed within timeoutMs.
func (p *pipeline) acquire(timeoutMs uint32) (bool, error) {
	var info dxgiOutduplFrameInfo
	var res *com
	r := p.dup.call(vIDXGIOutputDuplication_AcquireNextFrame, uintptr(timeoutMs),
		uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&res)))
	switch uint32(r) {
	case dxgiErrorWaitTimeout:
		return false, nil
	case dxgiErrorAccessLost:
		// Mode change, lock screen, UAC prompt… re-create the duplication.
		p.dup.Release()
		p.dup = nil
		for !p.v.stop.Load() {
			err := p.duplicate()
			if err == nil {
				return false, nil
			}
			if hrCode(err) != eAccessDenied && hrCode(err) != dxgiErrorAccessLost {
				return false, err
			}
			time.Sleep(250 * time.Millisecond)
		}
		return false, nil
	}
	if err := hr("AcquireNextFrame", r); err != nil {
		return false, err
	}
	defer p.dup.call(vIDXGIOutputDuplication_ReleaseFrame)
	defer res.Release()
	if info.LastPresentTime == 0 {
		return false, nil // only the mouse moved; image unchanged
	}
	tex, err := res.QI(&IID_ID3D11Texture2D)
	if err != nil {
		return false, err
	}
	p.ctx.call(vID3D11DeviceContext_CopyResource, p.bgra.u(), tex.u())
	tex.Release()
	p.haveFrame = true
	return true, nil
}

// submit converts p.bgra to NV12 on the GPU and hands it to the encoder.
func (p *pipeline) submit() error {
	slot := p.ring[p.ringIdx%len(p.ring)]
	p.ringIdx++
	stream := vpStream{Enable: 1, InputSurface: p.inView}
	if err := hr("VideoProcessorBlt", p.vctx.call(vID3D11VideoContext_VideoProcessorBlt,
		p.vp.u(), slot.view.u(), 0, 1, uintptr(unsafe.Pointer(&stream)))); err != nil {
		return err
	}

	var buf *com
	if err := hr("MFCreateDXGISurfaceBuffer", must(pMFCreateDXGISurfaceBuffer.Call(
		uintptr(unsafe.Pointer(&IID_ID3D11Texture2D)), slot.tex.u(), 0, 0, uintptr(unsafe.Pointer(&buf))))); err != nil {
		return err
	}
	if b2, err := buf.QI(&IID_IMF2DBuffer); err == nil {
		var n uint32
		if hr("GetContiguousLength", b2.call(vIMF2DBuffer_GetContiguousLength, uintptr(unsafe.Pointer(&n)))) == nil {
			buf.call(vIMFMediaBuffer_SetCurrentLength, uintptr(n))
		}
		b2.Release()
	}
	var s *com
	if err := hr("MFCreateSample", must(pMFCreateSample.Call(uintptr(unsafe.Pointer(&s))))); err != nil {
		buf.Release()
		return err
	}
	s.call(vIMFSample_AddBuffer, buf.u())
	buf.Release()
	ts := time.Since(p.start).Nanoseconds() / 100 // 100ns units
	s.call(vIMFSample_SetSampleTime, uintptr(ts))
	s.call(vIMFSample_SetSampleDuration, uintptr(int64(time.Second/time.Duration(p.fps))/100))
	r := p.mft.call(vIMFTransform_ProcessInput, 0, s.u(), 0)
	s.Release()
	if uint32(r) == mfENotAccepting {
		return nil
	}
	return hr("ProcessInput", r)
}

func (p *pipeline) processOutput() error {
	var odb mftOutputDataBuffer
	if p.outInfo.Flags&(mftOutProvidesSamples|mftOutCanProvideSamples) == 0 {
		// The encoder wants us to allocate the output sample.
		var mb, s *com
		size := max(p.outInfo.Size, uint32(p.w*p.h))
		if err := hr("MFCreateMemoryBuffer", must(pMFCreateMemoryBuffer.Call(uintptr(size), uintptr(unsafe.Pointer(&mb))))); err != nil {
			return err
		}
		if err := hr("MFCreateSample", must(pMFCreateSample.Call(uintptr(unsafe.Pointer(&s))))); err != nil {
			mb.Release()
			return err
		}
		s.call(vIMFSample_AddBuffer, mb.u())
		mb.Release()
		odb.Sample = s
	}
	var status uint32
	r := p.mft.call(vIMFTransform_ProcessOutput, 0, 1, uintptr(unsafe.Pointer(&odb)), uintptr(unsafe.Pointer(&status)))
	odb.Events.Release()
	defer odb.Sample.Release()
	switch uint32(r) {
	case mfETransformNeedMoreInput:
		return nil
	case mfETransformStreamChange:
		var t *com
		if err := hr("GetOutputAvailableType", p.mft.call(vIMFTransform_GetOutputAvailableType, 0, 0,
			uintptr(unsafe.Pointer(&t)))); err != nil {
			return err
		}
		defer t.Release()
		p.seqHeader = nil
		return hr("SetOutputType", p.mft.call(vIMFTransform_SetOutputType, 0, t.u(), 0))
	}
	if err := hr("ProcessOutput", r); err != nil {
		return err
	}
	if odb.Sample == nil {
		return nil
	}

	var buf *com
	if err := hr("ConvertToContiguousBuffer", odb.Sample.call(vIMFSample_ConvertToContiguousBuffer,
		uintptr(unsafe.Pointer(&buf)))); err != nil {
		return err
	}
	defer buf.Release()
	var data *byte
	var maxLen, cur uint32
	if err := hr("Lock", buf.call(vIMFMediaBuffer_Lock, uintptr(unsafe.Pointer(&data)),
		uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&cur)))); err != nil {
		return err
	}
	au := append([]byte(nil), unsafe.Slice(data, cur)...)
	buf.call(vIMFMediaBuffer_Unlock)
	if len(au) == 0 {
		return nil
	}

	key := hasNAL(au, 5)
	if key && !hasNAL(au, 7) {
		// Some encoders only put SPS/PPS in the media type; the browser
		// decoder needs them in-band on every keyframe.
		if p.seqHeader == nil {
			p.seqHeader = p.sequenceHeader()
		}
		au = append(append([]byte(nil), p.seqHeader...), au...)
	}
	p.v.deliver(au, key)
	return nil
}

func (p *pipeline) sequenceHeader() []byte {
	var t *com
	if hr("GetOutputCurrentType", p.mft.call(vIMFTransform_GetOutputCurrentType, 0, uintptr(unsafe.Pointer(&t)))) != nil {
		return nil
	}
	defer t.Release()
	var n uint32
	if hr("GetBlobSize", t.call(vIMFAttributes_GetBlobSize, uintptr(unsafe.Pointer(&MF_MT_MPEG_SEQUENCE_HEADER)),
		uintptr(unsafe.Pointer(&n)))) != nil || n == 0 {
		return nil
	}
	b := make([]byte, n)
	if hr("GetBlob", t.call(vIMFAttributes_GetBlob, uintptr(unsafe.Pointer(&MF_MT_MPEG_SEQUENCE_HEADER)),
		uintptr(unsafe.Pointer(&b[0])), uintptr(n), uintptr(unsafe.Pointer(&n)))) != nil {
		return nil
	}
	return b[:n]
}

// hasNAL reports whether an Annex-B access unit contains a NAL of type t.
func hasNAL(au []byte, t byte) bool {
	for i := 0; i+3 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 {
			if au[i+3]&0x1f == t {
				return true
			}
			i += 2
		}
	}
	return false
}
