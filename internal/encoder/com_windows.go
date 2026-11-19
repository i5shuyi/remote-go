//go:build windows && (amd64 || arm64)

package encoder

// Minimal COM plumbing for calling D3D11/DXGI/Media Foundation from pure Go
// (no cgo). Vtable slot indices are generated from the mingw-w64 IDL files
// into vtbl_windows.go — see genvtbl/.

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type GUID = windows.GUID

func mustGUID(s string) GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return g
}

// com is a COM interface pointer. The memory belongs to COM, not the Go heap.
type com struct {
	vtbl *[1024]uintptr
}

// call invokes vtable slot idx with c as `this`.
//
// Pointers to Go memory must be converted with uintptr(unsafe.Pointer(&x))
// directly in the argument list; uintptrescapes keeps them alive and pinned.
//
//go:uintptrescapes
func (c *com) call(idx int, args ...uintptr) uintptr {
	a := make([]uintptr, 0, len(args)+1)
	a = append(a, uintptr(unsafe.Pointer(c)))
	a = append(a, args...)
	r, _, _ := syscall.SyscallN(c.vtbl[idx], a...)
	return r
}

func (c *com) u() uintptr { return uintptr(unsafe.Pointer(c)) }

func (c *com) Release() {
	if c != nil {
		c.call(2)
	}
}

func (c *com) QI(iid *GUID) (*com, error) {
	var out *com
	if err := hr("QueryInterface", c.call(0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))); err != nil {
		return nil, err
	}
	return out, nil
}

type hrError struct {
	op   string
	code uint32
}

func (e hrError) Error() string { return fmt.Sprintf("%s 失败 (HRESULT 0x%08X)", e.op, e.code) }

func hr(op string, r uintptr) error {
	if int32(uint32(r)) < 0 {
		return hrError{op, uint32(r)}
	}
	return nil
}

func hrCode(err error) uint32 {
	if e, ok := err.(hrError); ok {
		return e.code
	}
	return 0
}

var (
	modD3D11  = windows.NewLazySystemDLL("d3d11.dll")
	modDXGI   = windows.NewLazySystemDLL("dxgi.dll")
	modMFPlat = windows.NewLazySystemDLL("mfplat.dll")
	modOle32  = windows.NewLazySystemDLL("ole32.dll")
	modUser32 = windows.NewLazySystemDLL("user32.dll")

	pD3D11CreateDevice         = modD3D11.NewProc("D3D11CreateDevice")
	pCreateDXGIFactory1        = modDXGI.NewProc("CreateDXGIFactory1")
	pMFStartup                 = modMFPlat.NewProc("MFStartup")
	pMFShutdown                = modMFPlat.NewProc("MFShutdown")
	pMFTEnumEx                 = modMFPlat.NewProc("MFTEnumEx")
	pMFCreateDXGIDeviceManager = modMFPlat.NewProc("MFCreateDXGIDeviceManager")
	pMFCreateMediaType         = modMFPlat.NewProc("MFCreateMediaType")
	pMFCreateSample            = modMFPlat.NewProc("MFCreateSample")
	pMFCreateDXGISurfaceBuffer = modMFPlat.NewProc("MFCreateDXGISurfaceBuffer")
	pMFCreateMemoryBuffer      = modMFPlat.NewProc("MFCreateMemoryBuffer")
	pCoInitializeEx            = modOle32.NewProc("CoInitializeEx")
	pCoUninitialize            = modOle32.NewProc("CoUninitialize")
	pCoTaskMemFree             = modOle32.NewProc("CoTaskMemFree")
	pSetProcessDPIAware        = modUser32.NewProc("SetProcessDPIAware")
)

// guidArgs passes a GUID by value per the Windows calling convention:
// by hidden reference on x64, in two registers on ARM64. g must not be on
// the Go stack (use a package-level variable).
func guidArgs(g *GUID) []uintptr {
	if runtime.GOARCH == "arm64" {
		p := (*[2]uint64)(unsafe.Pointer(g))
		return []uintptr{uintptr(p[0]), uintptr(p[1])}
	}
	return []uintptr{uintptr(unsafe.Pointer(g))}
}

// Media Foundation / CODECAPI GUIDs (verified against mingw-w64 mfapi.h,
// mftransform.idl and codecapi.h).
var (
	MFT_CATEGORY_VIDEO_ENCODER            = mustGUID("{f79eac7d-e545-4387-bdee-d647d7bde42a}")
	MFMediaType_Video                     = mustGUID("{73646976-0000-0010-8000-00aa00389b71}")
	MFVideoFormat_NV12                    = mustGUID("{3231564e-0000-0010-8000-00aa00389b71}") // FCC('NV12')
	MFVideoFormat_H264                    = mustGUID("{34363248-0000-0010-8000-00aa00389b71}") // FCC('H264')
	MF_MT_MAJOR_TYPE                      = mustGUID("{48eba18e-f8c9-4687-bf11-0a74c9f96a8f}")
	MF_MT_SUBTYPE                         = mustGUID("{f7e34c9a-42e8-4714-b74b-cb29d72c35e5}")
	MF_MT_AVG_BITRATE                     = mustGUID("{20332624-fb0d-4d9e-bd0d-cbf6786c102e}")
	MF_MT_FRAME_SIZE                      = mustGUID("{1652c33d-d6b2-4012-b834-72030849a37d}")
	MF_MT_FRAME_RATE                      = mustGUID("{c459a2e8-3d2c-4e44-b132-fee5156c7bb0}")
	MF_MT_PIXEL_ASPECT_RATIO              = mustGUID("{c6376a1e-8d0a-4027-be45-6d9a0ad39bb6}")
	MF_MT_INTERLACE_MODE                  = mustGUID("{e2724bb8-e676-4806-b4b2-a8d6efb44ccd}")
	MF_MT_MPEG2_PROFILE                   = mustGUID("{ad76a80b-2d5c-4e0b-b375-64e520137036}")
	MF_MT_MPEG_SEQUENCE_HEADER            = mustGUID("{3c036de7-3ad0-4c9e-9216-ee6d6ac21cb3}")
	MF_MT_YUV_MATRIX                      = mustGUID("{3e23d450-2c75-4d25-a00e-b91670d12327}")
	MF_MT_VIDEO_NOMINAL_RANGE             = mustGUID("{c21b8ee5-b956-4071-8daf-325edf5cab11}")
	MF_TRANSFORM_ASYNC_UNLOCK             = mustGUID("{e5666d6b-3422-4eb6-a421-da7db1f8e207}")
	MF_LOW_LATENCY                        = mustGUID("{9c27891a-ed7a-40e1-88e8-b22727a024ee}")
	CODECAPI_AVEncCommonRateControlMode   = mustGUID("{1c0608e9-370c-4710-8a58-cb6181c42423}")
	CODECAPI_AVEncCommonMeanBitRate       = mustGUID("{f7222374-2144-4815-b550-a37f8e12ee52}")
	CODECAPI_AVLowLatencyMode             = mustGUID("{9c27891a-ed7a-40e1-88e8-b22727a024ee}")
	CODECAPI_AVEncMPVGOPSize              = mustGUID("{95f31b26-95a4-41aa-9303-246a7fc6eef1}")
	CODECAPI_AVEncMPVDefaultBPictureCount = mustGUID("{8d390aac-dc5c-4200-b57f-814d04babab2}")
	CODECAPI_AVEncVideoForceKeyFrame      = mustGUID("{398c1b98-8353-475a-9ef2-8f265d260345}")
)

const (
	mfVersion = 0x00020070 // MF_SDK_VERSION<<16 | MF_API_VERSION

	mftEnumHardware      = 0x04
	mftEnumSortAndFilter = 0x40

	mftMsgSetD3DManager     = 0x00000002
	mftMsgBeginStreaming    = 0x10000000
	mftMsgEndStreaming      = 0x10000001
	mftMsgEndOfStream       = 0x10000002
	mftMsgStartOfStream     = 0x10000003
	mftOutProvidesSamples   = 0x100
	mftOutCanProvideSamples = 0x200

	meTransformNeedInput  = 601
	meTransformHaveOutput = 602
	mfEventFlagNoWait     = 1

	mfENoEventsAvailable      = 0xC00D3E80
	mfETransformStreamChange  = 0xC00D6D61
	mfETransformNeedMoreInput = 0xC00D6D72
	mfENotAccepting           = 0xC00D36B5
	dxgiErrorWaitTimeout      = 0x887A0027
	dxgiErrorAccessLost       = 0x887A0026
	eAccessDenied             = 0x80070005

	dxgiFormatB8G8R8A8 = 87
	dxgiFormatNV12     = 103

	d3d11BindShaderResource = 0x8
	d3d11BindRenderTarget   = 0x20
	d3d11BindVideoEncoder   = 0x200

	d3d11CreateBGRASupport  = 0x20
	d3d11CreateVideoSupport = 0x800
	d3d11SDKVersion         = 7

	vtUI4  = 19
	vtBool = 11
)

type rect32 struct{ Left, Top, Right, Bottom int32 }

type dxgiOutputDesc struct { // DXGI_OUTPUT_DESC
	DeviceName         [32]uint16
	DesktopCoordinates rect32
	AttachedToDesktop  int32
	Rotation           uint32
	Monitor            uintptr
}

type dxgiOutduplDesc struct { // DXGI_OUTDUPL_DESC
	Width, Height              uint32
	RefreshNum, RefreshDen     uint32
	Format                     uint32
	ScanlineOrdering, Scaling  uint32
	Rotation                   uint32
	DesktopImageInSystemMemory int32
}

type dxgiOutduplFrameInfo struct { // DXGI_OUTDUPL_FRAME_INFO
	LastPresentTime           int64
	LastMouseUpdateTime       int64
	AccumulatedFrames         uint32
	RectsCoalesced            int32
	ProtectedContentMaskedOut int32
	PointerX, PointerY        int32
	PointerVisible            int32
	TotalMetadataBufferSize   uint32
	PointerShapeBufferSize    uint32
}

type texture2DDesc struct { // D3D11_TEXTURE2D_DESC
	Width, Height, MipLevels, ArraySize, Format uint32
	SampleCount, SampleQuality                  uint32
	Usage, BindFlags, CPUAccessFlags, MiscFlags uint32
}

type vpContentDesc struct { // D3D11_VIDEO_PROCESSOR_CONTENT_DESC
	InputFrameFormat          uint32
	InRateNum, InRateDen      uint32
	InputWidth, InputHeight   uint32
	OutRateNum, OutRateDen    uint32
	OutputWidth, OutputHeight uint32
	Usage                     uint32
}

type vpInputViewDesc struct{ FourCC, ViewDimension, MipSlice, ArraySlice uint32 }
type vpOutputViewDesc struct{ ViewDimension, MipSlice, FirstArraySlice, ArraySize uint32 }

type vpStream struct { // D3D11_VIDEO_PROCESSOR_STREAM
	Enable                                                   int32
	OutputIndex, InputFrameOrField, PastFrames, FutureFrames uint32
	PastSurfaces                                             uintptr
	InputSurface                                             *com
	FutureSurfaces, PastSurfacesRight                        uintptr
	InputSurfaceRight                                        *com
	FutureSurfacesRight                                      uintptr
}

type mftRegisterTypeInfo struct{ Major, Sub GUID }

type mftOutputStreamInfo struct{ Flags, Size, Alignment uint32 }

type mftOutputDataBuffer struct {
	StreamID uint32
	Sample   *com
	Status   uint32
	Events   *com
}

type variant struct {
	VT      uint16
	_, _, _ uint16
	Val     uint64
	_       uint64
}

// Compile-time layout checks against the Windows x64/ARM64 ABI.
var (
	_ [96 - unsafe.Sizeof(dxgiOutputDesc{})]byte
	_ [unsafe.Sizeof(dxgiOutputDesc{}) - 96]byte
	_ [36 - unsafe.Sizeof(dxgiOutduplDesc{})]byte
	_ [unsafe.Sizeof(dxgiOutduplDesc{}) - 36]byte
	_ [48 - unsafe.Sizeof(dxgiOutduplFrameInfo{})]byte
	_ [unsafe.Sizeof(dxgiOutduplFrameInfo{}) - 48]byte
	_ [44 - unsafe.Sizeof(texture2DDesc{})]byte
	_ [unsafe.Sizeof(texture2DDesc{}) - 44]byte
	_ [40 - unsafe.Sizeof(vpContentDesc{})]byte
	_ [unsafe.Sizeof(vpContentDesc{}) - 40]byte
	_ [72 - unsafe.Sizeof(vpStream{})]byte
	_ [unsafe.Sizeof(vpStream{}) - 72]byte
	_ [32 - unsafe.Sizeof(mftOutputDataBuffer{})]byte
	_ [unsafe.Sizeof(mftOutputDataBuffer{}) - 32]byte
	_ [24 - unsafe.Sizeof(variant{})]byte
	_ [unsafe.Sizeof(variant{}) - 24]byte
)
