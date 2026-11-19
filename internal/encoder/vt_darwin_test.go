//go:build darwin && cgo

package encoder

import (
	"bytes"
	"os"
	"testing"
)

// nalTypes lists the NAL unit types in an Annex-B access unit.
func nalTypes(au []byte) []byte {
	var types []byte
	for i := 0; i+4 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 0 && au[i+3] == 1 {
			types = append(types, au[i+4]&0x1f)
			i += 3
		}
	}
	return types
}

func TestVideoToolboxH264(t *testing.T) {
	frames, err := selfTest(1920, 1080, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 55 {
		t.Fatalf("expected ~60 encoded frames, got %d", len(frames))
	}
	first := frames[0]
	if !first.Key {
		t.Fatal("first frame must be a keyframe")
	}
	ft := nalTypes(first.Data)
	if !bytes.Contains(ft, []byte{7}) || !bytes.Contains(ft, []byte{8}) || !bytes.Contains(ft, []byte{5}) {
		t.Fatalf("keyframe should contain SPS(7), PPS(8), IDR(5); got %v", ft)
	}
	sps := first.Data[bytes.Index(first.Data, []byte{0, 0, 0, 1, first.Data[4]})+4:]
	t.Logf("profile_idc=%d constraints=%#x level_idc=%d", sps[1], sps[2], sps[3])
	total, keys := 0, 0
	var all bytes.Buffer
	for _, f := range frames {
		total += len(f.Data)
		all.Write(f.Data)
		if f.Key {
			keys++
		}
	}
	t.Logf("%d frames, %d keyframes, %.1f KB total", len(frames), keys, float64(total)/1024)
	if out := os.Getenv("RG_DUMP_H264"); out != "" {
		os.WriteFile(out, all.Bytes(), 0o644)
	}
}
