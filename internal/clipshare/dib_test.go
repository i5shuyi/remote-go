package clipshare

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestWindowsClipboardBitmap(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	img.Set(2, 1, color.NRGBA{B: 255, A: 255})
	var raw bytes.Buffer
	if err := png.Encode(&raw, img); err != nil {
		t.Fatal(err)
	}
	dib, err := dibFromPNG(raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	b, err := pngFromDIB(dib)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds() != img.Bounds() || color.NRGBAModel.Convert(decoded.At(2, 1)) != (color.NRGBA{B: 255, A: 255}) || color.NRGBAModel.Convert(decoded.At(0, 0)) != (color.NRGBA{255, 255, 255, 255}) {
		t.Fatal("invalid DIB orientation, alpha or color order")
	}
}
