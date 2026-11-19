package clipshare

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"

	"golang.org/x/image/bmp"
)

// A clipboard DIB omits the BMP file header. Restore it for the BMP decoder.
func pngFromDIB(d []byte) ([]byte, error) {
	if len(d) < 40 {
		return nil, errors.New("剪贴板位图不完整")
	}
	le := binary.LittleEndian
	info := le.Uint32(d)
	bits := le.Uint16(d[14:])
	palette := le.Uint32(d[32:])
	if bits <= 8 && palette == 0 {
		palette = 1 << bits
	}
	offset := uint64(info) + 4*uint64(palette)
	if offset > uint64(len(d)) {
		return nil, errors.New("剪贴板位图偏移无效")
	}
	var header [14]byte
	copy(header[:], "BM")
	le.PutUint32(header[2:], uint32(len(d)+14))
	le.PutUint32(header[10:], uint32(offset+14))
	reader := func() io.Reader { return io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(d)) }
	cfg, err := bmp.DecodeConfig(reader())
	if err != nil || cfg.Height <= 0 || cfg.Width <= 0 || cfg.Width > MaxPixels/cfg.Height {
		return nil, errors.New("不支持此剪贴板位图格式或尺寸")
	}
	img, err := bmp.Decode(reader())
	if err != nil {
		return nil, err
	}
	if rgba, ok := img.(*image.NRGBA); ok && info >= 108 && len(d) >= 108 && le.Uint32(d[52:]) == 0 {
		for i := 3; i < len(rgba.Pix); i += 4 {
			rgba.Pix[i] = 255
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), CheckPNG(out.Bytes())
}

func dibFromPNG(b []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	// Legacy bitmap consumers expect opaque BGR pixels. The PNG format
	// alongside this DIB retains the original alpha channel.
	rgb := image.NewRGBA(img.Bounds())
	draw.Draw(rgb, rgb.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(rgb, rgb.Bounds(), img, img.Bounds().Min, draw.Over)
	var out bytes.Buffer
	if err = bmp.Encode(&out, rgb); err != nil {
		return nil, err
	}
	return out.Bytes()[14:], nil
}
