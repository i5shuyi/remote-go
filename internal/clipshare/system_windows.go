//go:build windows

package clipshare

import (
	"context"
	"encoding/binary"
	"errors"
	"runtime"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

type systemBoard struct{}

var version = windows.NewLazySystemDLL("user32.dll").NewProc("GetClipboardSequenceNumber")
var globalSize = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalSize")

func System() Board                 { return systemBoard{} }
func (systemBoard) Version() uint64 { v, _, _ := version.Call(); return uint64(v) }

func (systemBoard) Read() ([]byte, bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if !win.OpenClipboard(0) {
		return nil, false
	}
	defer win.CloseClipboard()
	if !win.IsClipboardFormatAvailable(win.CF_UNICODETEXT) {
		return nil, true
	}
	h := win.HGLOBAL(win.GetClipboardData(win.CF_UNICODETEXT))
	size, _, _ := globalSize.Call(uintptr(h))
	if h == 0 || size < 2 {
		return nil, false
	}
	p := win.GlobalLock(h)
	if p == nil {
		return nil, false
	}
	defer win.GlobalUnlock(h)
	units := unsafe.Slice((*uint16)(p), min(size/2, MaxBytes+1))
	for i, u := range units {
		if u == 0 {
			return []byte(string(utf16.Decode(units[:i]))), true
		}
	}
	return nil, true
}

func (systemBoard) Write(data []byte) bool {
	units, err := windows.UTF16FromString(string(data))
	if err != nil {
		return false
	}
	return publish([]formatData{{win.CF_UNICODETEXT, unsafe.Slice((*byte)(unsafe.Pointer(&units[0])), 2*len(units))}})
}

type formatData struct {
	id uint32
	b  []byte
}

var pngID = sync.OnceValue(func() uint32 {
	name, _ := windows.UTF16PtrFromString("PNG")
	id, _, _ := windows.NewLazySystemDLL("user32.dll").NewProc("RegisterClipboardFormatW").Call(uintptr(unsafe.Pointer(name)))
	return uint32(id)
})

func publish(items []formatData) bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	class, _ := windows.UTF16PtrFromString("STATIC")
	owner := win.CreateWindowEx(0, class, nil, 0, 0, 0, 0, 0, win.HWND_MESSAGE, 0, 0, nil)
	if owner == 0 {
		return false
	}
	defer win.DestroyWindow(owner)
	handles := make([]win.HGLOBAL, len(items))
	defer func() {
		for _, h := range handles {
			if h != 0 {
				win.GlobalFree(h)
			}
		}
	}()
	for i, f := range items {
		if len(f.b) == 0 || f.id == 0 {
			return false
		}
		h := win.GlobalAlloc(win.GMEM_MOVEABLE, uintptr(len(f.b)))
		handles[i] = h
		if h == 0 {
			return false
		}
		p := win.GlobalLock(h)
		if p == nil {
			return false
		}
		copy(unsafe.Slice((*byte)(p), len(f.b)), f.b)
		win.GlobalUnlock(h)
	}
	for attempt := 0; !win.OpenClipboard(owner); attempt++ {
		if attempt == 9 {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer win.CloseClipboard()
	if !win.EmptyClipboard() {
		return false
	}
	ok := false
	for i, f := range items {
		if win.SetClipboardData(f.id, win.HANDLE(handles[i])) != 0 {
			handles[i] = 0
			ok = true
		}
	}
	return ok
}

func readMemory(format uint32, limit uintptr) ([]byte, error) {
	h := win.HGLOBAL(win.GetClipboardData(format))
	size, _, _ := globalSize.Call(uintptr(h))
	if h == 0 || size == 0 || size > limit {
		return nil, errors.New("剪贴板数据大小不受支持")
	}
	p := win.GlobalLock(h)
	if p == nil {
		return nil, errors.New("无法读取系统剪贴板")
	}
	defer win.GlobalUnlock(h)
	return append([]byte(nil), unsafe.Slice((*byte)(p), size)...), nil
}

func (systemBoard) ReadRich(ctx context.Context) (Content, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if !win.OpenClipboard(0) {
		return Content{}, errBusy
	}
	defer win.CloseClipboard()
	if win.IsClipboardFormatAvailable(win.CF_HDROP) {
		h := win.HDROP(win.GetClipboardData(win.CF_HDROP))
		n := win.DragQueryFile(h, 0xffffffff, nil, 0)
		if n == 0 || n > MaxFiles {
			return Content{}, errors.New("剪贴板文件数量不受支持")
		}
		var paths []string
		for i := uint(0); i < n; i++ {
			size := win.DragQueryFile(h, i, nil, 0)
			if size == 0 || size > 32767 {
				return Content{}, errors.New("剪贴板文件路径无效")
			}
			buf := make([]uint16, size+1)
			if win.DragQueryFile(h, i, &buf[0], size+1) != size {
				return Content{}, errors.New("无法读取剪贴板文件列表")
			}
			paths = append(paths, windows.UTF16ToString(buf))
		}
		return Content{Files: paths}, nil
	}
	if id := pngID(); id != 0 && win.IsClipboardFormatAvailable(id) {
		b, err := readMemory(id, MaxImage)
		return Content{PNG: b}, err
	}
	for _, format := range []uint32{win.CF_DIBV5, win.CF_DIB} {
		if win.IsClipboardFormatAvailable(format) {
			b, err := readMemory(format, MaxPixels*4+2048)
			if err != nil {
				return Content{}, err
			}
			b, err = pngFromDIB(b)
			if err != nil && format == win.CF_DIBV5 && win.IsClipboardFormatAvailable(win.CF_DIB) {
				continue
			}
			return Content{PNG: b}, err
		}
	}
	return Content{}, nil
}

func (systemBoard) WriteRich(c Content) error {
	var formats []formatData
	if len(c.Files) > 0 {
		drop := make([]byte, 20)
		binary.LittleEndian.PutUint32(drop, 20)
		binary.LittleEndian.PutUint32(drop[16:], 1)
		for _, path := range c.Files {
			units, err := windows.UTF16FromString(path)
			if err != nil {
				return err
			}
			for _, unit := range units {
				drop = binary.LittleEndian.AppendUint16(drop, unit)
			}
		}
		formats = []formatData{{win.CF_HDROP, append(drop, 0, 0)}}
	} else {
		b, err := dibFromPNG(c.PNG)
		if err != nil {
			return err
		}
		formats = []formatData{{pngID(), c.PNG}, {win.CF_DIB, b}}
	}
	if !publish(formats) {
		return errors.New("无法写入系统剪贴板")
	}
	return nil
}
