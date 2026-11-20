package encoder

import "testing"

func TestHasIDR(t *testing.T) {
	for _, c := range []struct {
		au   []byte
		want bool
	}{
		{[]byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3, 0, 0, 1, 0x65, 9}, true}, // SPS PPS IDR
		{[]byte{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x41, 9, 9}, false},            // AUD, P slice
		{[]byte{0, 0, 1, 0x25, 0}, true},                                           // IDR, nal_ref_idc 1
		{[]byte{0, 0, 1}, false},
		{nil, false},
	} {
		if got := HasIDR(c.au); got != c.want {
			t.Errorf("HasIDR(% x) = %v, want %v", c.au, got, c.want)
		}
	}
}
