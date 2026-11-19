//go:build darwin && cgo

package encoder

/*
#cgo CFLAGS: -fobjc-arc -Wno-unused-command-line-argument
#cgo LDFLAGS: -framework Foundation -framework ScreenCaptureKit -framework VideoToolbox -framework CoreMedia -framework CoreVideo -framework CoreGraphics
#include <stdlib.h>
#include "vt_darwin.h"
*/
import "C"

import (
	"errors"
	"runtime/cgo"
	"sync"
	"unsafe"
)

type vtVideo struct {
	ptr   unsafe.Pointer
	h     cgo.Handle
	w, h_ int

	frames chan Frame
	done   chan struct{}
	once   sync.Once
	err    error

	mu       sync.Mutex
	closed   bool
	waitKey  bool // dropped a frame; discard deltas until next keyframe
	keyAsked bool
}

func startH264(o Options) (Video, error) {
	v := &vtVideo{frames: make(chan Frame, 60), done: make(chan struct{})}
	v.h = cgo.NewHandle(v)
	fps := min(max(o.FPS, 1), 60)
	// bitrate is computed for the requested width; the exact size is chosen natively
	br := Bitrate(o.MaxWidth, o.MaxWidth*9/16, fps, o.Quality)
	var w, h C.int
	errBuf := make([]byte, 512)
	p := C.rg_enc_start(C.int(o.Display), C.int(o.MaxWidth), C.int(fps), C.int(br), C.uintptr_t(v.h),
		&w, &h, (*C.char)(unsafe.Pointer(&errBuf[0])), C.int(len(errBuf)))
	if p == nil {
		v.h.Delete()
		return nil, errors.New(C.GoString((*C.char)(unsafe.Pointer(&errBuf[0]))))
	}
	v.ptr, v.w, v.h_ = p, int(w), int(h)
	return v, nil
}

func (v *vtVideo) Frames() <-chan Frame  { return v.frames }
func (v *vtVideo) Done() <-chan struct{} { return v.done }
func (v *vtVideo) Size() (int, int)      { return v.w, v.h_ }
func (v *vtVideo) Err() error            { return v.err }

func (v *vtVideo) RequestKeyframe() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.closed {
		C.rg_enc_keyframe(v.ptr)
	}
}

func (v *vtVideo) Close() {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	v.mu.Unlock()
	C.rg_enc_stop(v.ptr) // synchronous: no callbacks after this returns
	v.h.Delete()
	v.once.Do(func() { close(v.done) })
}

func (v *vtVideo) deliver(data []byte, key bool) {
	v.mu.Lock()
	if key {
		v.waitKey, v.keyAsked = false, false
	}
	if v.waitKey {
		ask := !v.keyAsked
		v.keyAsked = true
		v.mu.Unlock()
		if ask {
			go v.RequestKeyframe() // not on the VT thread: Close may be waiting on it
		}
		return
	}
	select {
	case v.frames <- Frame{Data: data, Key: key}:
		v.mu.Unlock()
	default:
		// consumer too slow: a dropped delta corrupts the stream, so resync
		v.waitKey, v.keyAsked = true, true
		v.mu.Unlock()
		go v.RequestKeyframe()
	}
}

//export goOnFrame
func goOnFrame(handle C.uintptr_t, data unsafe.Pointer, n C.int, key C.int) {
	v, ok := cgo.Handle(handle).Value().(*vtVideo)
	if !ok {
		return
	}
	v.deliver(C.GoBytes(data, n), key != 0)
}

//export goOnStop
func goOnStop(handle C.uintptr_t, msg *C.char) {
	v, ok := cgo.Handle(handle).Value().(*vtVideo)
	if !ok {
		return
	}
	v.err = errors.New(C.GoString(msg))
	v.once.Do(func() { close(v.done) })
}

// SelfTest encodes n synthetic frames through the same VideoToolbox session
// setup used for screen capture (no Screen Recording permission needed).
// Useful as a diagnostic and in tests.
func SelfTest(w, h, n int) ([]Frame, error) { return selfTest(w, h, n) }

func selfTest(w, h, n int) ([]Frame, error) {
	v := &vtVideo{frames: make(chan Frame, n+8), done: make(chan struct{})}
	v.h = cgo.NewHandle(v)
	defer v.h.Delete()
	errBuf := make([]byte, 512)
	if C.rg_enc_selftest(C.int(w), C.int(h), C.int(n), C.uintptr_t(v.h),
		(*C.char)(unsafe.Pointer(&errBuf[0])), C.int(len(errBuf))) != 0 {
		return nil, errors.New(C.GoString((*C.char)(unsafe.Pointer(&errBuf[0]))))
	}
	close(v.frames)
	var out []Frame
	for f := range v.frames {
		out = append(out, f)
	}
	return out, nil
}
