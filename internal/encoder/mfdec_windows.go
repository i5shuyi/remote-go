//go:build windows && (amd64 || arm64)

package encoder

// H.264 decoding for the Windows viewer: Microsoft's H.264 decoder MFT
// (DXVA-capable, software output here) producing NV12, converted to BGRA
// for a GDI blit. It runs on its own locked OS thread with COM initialised.

import (
	"errors"
	"runtime"
	"sync"
	"unsafe"
)

var (
	pCoCreateInstance = modOle32.NewProc("CoCreateInstance")

	CLSID_CMSH264DecoderMFT         = mustGUID("{62CE7E72-4C71-4d20-B15D-452831A87D9D}")
	MF_MT_DEFAULT_STRIDE            = mustGUID("{644b4e48-1e02-4516-b0eb-c01ca9d49ac6}")
	MF_MT_MINIMUM_DISPLAY_APERTURE  = mustGUID("{d7388766-18fe-48c6-a177-ee894867c8c4}")
	CODECAPI_AVDecVideoAcceleration = mustGUID("{f7db8a2f-4f48-4ee8-ae31-8b6ebe558ae2}")
	MF_SA_D3D_AWARE                 = mustGUID("{eaa35c29-775e-488e-9b61-b3283e49583b}")
	errDecoderClosed                = errors.New("解码器已关闭")
)

const (
	vIMFAttributes_GetUINT64 = 8
	vIMFAttributes_GetGUID   = 10
	mftMsgCommandFlush       = 0
	clsctxInprocServer       = 1
	coinitMultithreaded      = 0
)

// Picture is a decoded frame: BGRA pixels, Width*4 bytes per row.
type Picture struct {
	Pix           []byte
	Width, Height int
}

// H264Decoder decodes Annex-B access units on a background thread.
type H264Decoder struct {
	in     chan []byte
	out    func(*Picture)
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
	failed bool
}

// NewH264Decoder starts a decoder that calls out with each picture, on
// its own goroutine; out must not keep the picture past the call.
func NewH264Decoder(out func(*Picture)) (*H264Decoder, error) {
	if err := modMFPlat.Load(); err != nil {
		return nil, err
	}
	d := &H264Decoder{in: make(chan []byte, 8), out: out, done: make(chan struct{})}
	ready := make(chan error, 1)
	go d.run(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return d, nil
}

// Decode queues an access unit. It reports false when the decoder failed
// or fell behind; the caller then resets and waits for a keyframe.
func (d *H264Decoder) Decode(au []byte) bool {
	d.mu.Lock()
	failed := d.failed
	d.failed = false
	d.mu.Unlock()
	if failed {
		d.Flush()
		return false
	}
	select {
	case d.in <- append([]byte(nil), au...):
		return true
	case <-d.done:
		return false
	default: // falling behind: drop and resync
		d.Flush()
		return false
	}
}

// Flush drops queued input; decoding restarts at the next keyframe.
func (d *H264Decoder) Flush() {
	for {
		select {
		case <-d.in:
		default:
			select {
			case d.in <- nil: // nil asks the thread to flush the MFT
			default:
			}
			return
		}
	}
}

// Close stops the decoder.
func (d *H264Decoder) Close() { d.once.Do(func() { close(d.done) }) }

func (d *H264Decoder) fail(err error) {
	d.mu.Lock()
	d.err, d.failed = err, true
	d.mu.Unlock()
}

type decoder struct {
	mft     *com
	outInfo mftOutputStreamInfo
	w, h    int // coded size
	cw, ch  int // display size
	stride  int
	pic     Picture
}

func (d *H264Decoder) run(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pCoInitializeEx.Call(0, coinitMultithreaded)
	defer pCoUninitialize.Call()
	pMFStartup.Call(mfVersion, 0)
	defer pMFShutdown.Call()

	var dec decoder
	if err := dec.init(); err != nil {
		ready <- err
		return
	}
	defer dec.mft.Release()
	ready <- nil
	for {
		select {
		case <-d.done:
			return
		case au := <-d.in:
			if au == nil {
				dec.mft.call(vIMFTransform_ProcessMessage, mftMsgCommandFlush, 0)
				continue
			}
			if err := dec.decode(au, d.out); err != nil {
				dec.mft.call(vIMFTransform_ProcessMessage, mftMsgCommandFlush, 0)
				d.fail(err)
			}
		}
	}
}

func (dec *decoder) init() error {
	var t *com
	if err := hr("CoCreateInstance(H264 decoder)", must(pCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&CLSID_CMSH264DecoderMFT)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&IID_IMFTransform)), uintptr(unsafe.Pointer(&t))))); err != nil {
		return err
	}
	dec.mft = t
	var attrs *com
	if hr("GetAttributes", t.call(vIMFTransform_GetAttributes, uintptr(unsafe.Pointer(&attrs)))) == nil && attrs != nil {
		attrs.call(vIMFAttributes_SetUINT32, uintptr(unsafe.Pointer(&MF_LOW_LATENCY)), 1)
		attrs.Release()
	}
	it, err := newMediaType()
	if err != nil {
		t.Release()
		return err
	}
	it.call(vIMFAttributes_SetGUID, uintptr(unsafe.Pointer(&MF_MT_MAJOR_TYPE)), uintptr(unsafe.Pointer(&MFMediaType_Video)))
	it.call(vIMFAttributes_SetGUID, uintptr(unsafe.Pointer(&MF_MT_SUBTYPE)), uintptr(unsafe.Pointer(&MFVideoFormat_H264)))
	it.call(vIMFAttributes_SetUINT32, uintptr(unsafe.Pointer(&MF_MT_INTERLACE_MODE)), 2)
	err = hr("SetInputType", t.call(vIMFTransform_SetInputType, 0, it.u(), 0))
	it.Release()
	if err == nil {
		err = dec.setOutput()
	}
	if err != nil {
		t.Release()
		return err
	}
	t.call(vIMFTransform_ProcessMessage, mftMsgBeginStreaming, 0)
	t.call(vIMFTransform_ProcessMessage, mftMsgStartOfStream, 0)
	return nil
}

// setOutput picks the decoder's NV12 output type and reads its geometry.
func (dec *decoder) setOutput() error {
	for i := 0; ; i++ {
		var t *com
		if err := hr("GetOutputAvailableType", dec.mft.call(vIMFTransform_GetOutputAvailableType, 0, uintptr(i),
			uintptr(unsafe.Pointer(&t)))); err != nil {
			return err
		}
		var sub GUID
		t.call(vIMFAttributes_GetGUID, uintptr(unsafe.Pointer(&MF_MT_SUBTYPE)), uintptr(unsafe.Pointer(&sub)))
		if sub != MFVideoFormat_NV12 {
			t.Release()
			continue
		}
		err := hr("SetOutputType", dec.mft.call(vIMFTransform_SetOutputType, 0, t.u(), 0))
		if err == nil {
			dec.geometry(t)
			dec.mft.call(vIMFTransform_GetOutputStreamInfo, 0, uintptr(unsafe.Pointer(&dec.outInfo)))
		}
		t.Release()
		return err
	}
}

func (dec *decoder) geometry(t *com) {
	var size uint64
	t.call(vIMFAttributes_GetUINT64, uintptr(unsafe.Pointer(&MF_MT_FRAME_SIZE)), uintptr(unsafe.Pointer(&size)))
	dec.w, dec.h = int(size>>32), int(uint32(size))
	dec.cw, dec.ch = dec.w, dec.h
	var area struct {
		OffX, OffY int32 // MFOffset: fract uint16 + value int16
		CX, CY     int32
	}
	var n uint32
	if t.call(vIMFAttributes_GetBlob, uintptr(unsafe.Pointer(&MF_MT_MINIMUM_DISPLAY_APERTURE)),
		uintptr(unsafe.Pointer(&area)), unsafe.Sizeof(area), uintptr(unsafe.Pointer(&n))) == 0 &&
		area.CX > 0 && area.CY > 0 && int(area.CX) <= dec.w && int(area.CY) <= dec.h {
		dec.cw, dec.ch = int(area.CX), int(area.CY)
	}
	var stride uint32
	if t.call(vIMFAttributes_GetUINT32, uintptr(unsafe.Pointer(&MF_MT_DEFAULT_STRIDE)), uintptr(unsafe.Pointer(&stride))) == 0 &&
		int32(stride) > 0 {
		dec.stride = int(int32(stride))
	} else {
		dec.stride = dec.w
	}
}

func (dec *decoder) decode(au []byte, out func(*Picture)) error {
	var mb, s *com
	if err := hr("MFCreateMemoryBuffer", must(pMFCreateMemoryBuffer.Call(uintptr(len(au)), uintptr(unsafe.Pointer(&mb))))); err != nil {
		return err
	}
	var p *byte
	var maxLen, cur uint32
	if err := hr("Lock", mb.call(vIMFMediaBuffer_Lock, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&maxLen)),
		uintptr(unsafe.Pointer(&cur)))); err != nil {
		mb.Release()
		return err
	}
	copy(unsafe.Slice(p, len(au)), au)
	mb.call(vIMFMediaBuffer_Unlock)
	mb.call(vIMFMediaBuffer_SetCurrentLength, uintptr(len(au)))
	if err := hr("MFCreateSample", must(pMFCreateSample.Call(uintptr(unsafe.Pointer(&s))))); err != nil {
		mb.Release()
		return err
	}
	s.call(vIMFSample_AddBuffer, mb.u())
	mb.Release()
	defer s.Release()
	for try := 0; try < 2; try++ {
		r := dec.mft.call(vIMFTransform_ProcessInput, 0, s.u(), 0)
		if uint32(r) == mfENotAccepting {
			if err := dec.drain(out); err != nil {
				return err
			}
			continue
		}
		if err := hr("ProcessInput", r); err != nil {
			return err
		}
		break
	}
	return dec.drain(out)
}

// drain takes every picture the decoder has ready.
func (dec *decoder) drain(out func(*Picture)) error {
	for {
		var odb mftOutputDataBuffer
		if dec.outInfo.Flags&(mftOutProvidesSamples|mftOutCanProvideSamples) == 0 {
			var mb, s *com
			size := max(dec.outInfo.Size, uint32(dec.stride*dec.h*3/2))
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
		r := dec.mft.call(vIMFTransform_ProcessOutput, 0, 1, uintptr(unsafe.Pointer(&odb)), uintptr(unsafe.Pointer(&status)))
		odb.Events.Release()
		switch uint32(r) {
		case mfETransformNeedMoreInput:
			odb.Sample.Release()
			return nil
		case mfETransformStreamChange:
			odb.Sample.Release()
			if err := dec.setOutput(); err != nil {
				return err
			}
			continue
		}
		if err := hr("ProcessOutput", r); err != nil {
			odb.Sample.Release()
			return err
		}
		err := dec.emit(odb.Sample, out)
		odb.Sample.Release()
		if err != nil {
			return err
		}
	}
}

func (dec *decoder) emit(s *com, out func(*Picture)) error {
	if s == nil {
		return nil
	}
	var buf *com
	if err := hr("ConvertToContiguousBuffer", s.call(vIMFSample_ConvertToContiguousBuffer, uintptr(unsafe.Pointer(&buf)))); err != nil {
		return err
	}
	defer buf.Release()
	var p *byte
	var maxLen, cur uint32
	if err := hr("Lock", buf.call(vIMFMediaBuffer_Lock, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&maxLen)),
		uintptr(unsafe.Pointer(&cur)))); err != nil {
		return err
	}
	defer buf.call(vIMFMediaBuffer_Unlock)
	stride := dec.stride
	if stride*dec.h*3/2 > int(cur) {
		// Some outputs are tightly packed even when the type says otherwise.
		stride = dec.w
	}
	need := stride * dec.h * 3 / 2
	if dec.w <= 0 || dec.h <= 0 || need > int(cur) {
		return nil
	}
	nv12 := unsafe.Slice(p, need)
	w, h := dec.cw, dec.ch
	if len(dec.pic.Pix) != w*h*4 {
		dec.pic = Picture{Pix: make([]byte, w*h*4), Width: w, Height: h}
	}
	NV12ToBGRA(dec.pic.Pix, nv12, stride, dec.h, w, h)
	out(&dec.pic)
	return nil
}

// NV12ToBGRA converts BT.709 limited-range NV12 (Y plane of planeH rows
// of stride bytes, then interleaved CbCr) to w×h BGRA.
func NV12ToBGRA(dst, src []byte, stride, planeH, w, h int) {
	uv := src[stride*planeH:]
	for y := 0; y < h; y++ {
		yr := src[y*stride:]
		cr := uv[(y/2)*stride:]
		o := dst[y*w*4:]
		for x := 0; x < w; x++ {
			c := (int(yr[x]) - 16) * 298
			u := int(cr[x&^1]) - 128
			v := int(cr[x|1]) - 128
			r := (c + 459*v + 128) >> 8
			g := (c - 55*u - 136*v + 128) >> 8
			b := (c + 541*u + 128) >> 8
			i := x * 4
			o[i] = clamp8(b)
			o[i+1] = clamp8(g)
			o[i+2] = clamp8(r)
			o[i+3] = 255
		}
	}
}

func clamp8(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}
