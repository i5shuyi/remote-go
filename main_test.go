package main

import (
	"context"
	"testing"
)

func TestHostCodeRelayFlags(t *testing.T) {
	for _, tc := range []struct {
		device, endpoint string
		region           int
		suffix           byte
		valid            bool
	}{
		{"", "", 304, '4', true}, {"", "relay.example:8443", 0, '0', true},
		{"123456783", "", 0, '3', true}, {"123456780", "relay.example", 0, '0', true},
		{"123456783", "", 304, 0, false}, {"123456780", "", 0, 0, false},
		{"123456783", "relay.example", 0, 0, false}, {"", "relay.example", 304, 0, false},
	} {
		got, err := hostCode(context.Background(), tc.device, tc.region, tc.endpoint)
		if (err == nil) != tc.valid || tc.valid && (len(got) != 9 || got[8] != tc.suffix) {
			t.Errorf("hostCode(%q, %d, %q) = %q, %v", tc.device, tc.region, tc.endpoint, got, err)
		}
	}
}
