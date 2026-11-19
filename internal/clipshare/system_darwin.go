//go:build darwin && cgo

package clipshare

/*
#cgo LDFLAGS: -framework AppKit
#include <stdlib.h>
#include <stdint.h>
int rg_clip_rich_read(void **, int *, int, int, int);
int rg_clip_rich_write(const void *, int, int);
uint64_t rg_clip_version(void);
int rg_clip_read(void *out, int max);
int rg_clip_write(const void *data, int n);
*/
import "C"
import (
	"context"
	"encoding/json"
	"errors"
	"unsafe"
)

type systemBoard struct{}

func System() Board                 { return systemBoard{} }
func (systemBoard) Version() uint64 { return uint64(C.rg_clip_version()) }
func (systemBoard) Read() ([]byte, bool) {
	b := make([]byte, MaxBytes)
	n := int(C.rg_clip_read(unsafe.Pointer(&b[0]), C.int(len(b))))
	if n < 0 {
		return nil, false
	}
	return b[:n], true
}
func (systemBoard) Write(b []byte) bool {
	return len(b) > 0 && C.rg_clip_write(unsafe.Pointer(&b[0]), C.int(len(b))) != 0
}

func (systemBoard) ReadRich(ctx context.Context) (Content, error) {
	var buf unsafe.Pointer
	var n C.int
	k := C.rg_clip_rich_read(&buf, &n, MaxImage, MaxFiles, MaxPixels)
	if k == 0 {
		return Content{}, nil
	}
	if k < 0 {
		return Content{}, errors.New("无法读取剪贴板图片或文件")
	}
	defer C.free(buf)
	b := C.GoBytes(buf, n)
	if k == 1 {
		return Content{PNG: b}, nil
	}
	var paths []string
	err := json.Unmarshal(b, &paths)
	return Content{Files: paths}, err
}
func (systemBoard) WriteRich(c Content) error {
	b := c.PNG
	var files C.int
	if len(c.Files) > 0 {
		var err error
		b, err = json.Marshal(c.Files)
		if err != nil {
			return err
		}
		files = 1
	}
	if len(b) == 0 || C.rg_clip_rich_write(unsafe.Pointer(&b[0]), C.int(len(b)), files) == 0 {
		return errors.New("无法写入系统剪贴板")
	}
	return nil
}
