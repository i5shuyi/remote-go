package tunnel

import (
	"testing"

	"github.com/tailscale/tailcat"
)

func TestNormalizeRelay(t *testing.T) {
	for input, want := range map[string]string{
		"": "", " Derp.Example.com ": "derp.example.com:443",
		"https://derp.example.com:8443/derp": "derp.example.com:8443",
		"[::1]:444":                          "[::1]:444", "::1": "[::1]:443",
	} {
		if got, err := NormalizeRelay(input); err != nil || got != want {
			t.Errorf("NormalizeRelay(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"http://relay.example", "https://", ":443", "relay.example:", "relay:0", "relay:65536", "user@relay.example", "relay/foo", "relay?id=1", "https://[hostname]:443"} {
		if _, err := NormalizeRelay(input); err == nil {
			t.Errorf("accepted invalid relay %q", input)
		}
	}
}

func TestRelayAddressSelection(t *testing.T) {
	code, err := NewDeviceCode(CustomRegion)
	if err != nil || !Valid(code) || code[8] != '0' {
		t.Fatalf("private code %q: %v", code, err)
	}
	if _, err := Addr(code, "test-password", ""); err == nil {
		t.Fatal("private code accepted without endpoint")
	}
	addr, err := Addr(code, "test-password", "relay.example:8443")
	if err != nil {
		t.Fatal(err)
	}
	ci, err := tailcat.ParseAddr(addr)
	if err != nil || len(ci.Region) != 1 || len(ci.Region[0].Nodes) != 1 {
		t.Fatalf("embedded region: %v", err)
	}
	node := ci.Region[0].Nodes[0]
	if node.HostName != "relay.example" || node.DERPPort != 8443 || node.InsecureForTests {
		t.Fatalf("unexpected DERP node: %+v", node)
	}
	public := CodeRegion(code, 304)
	if public != code[:8]+"4" {
		t.Fatalf("re-homed code %q", public)
	}
	old, err := Addr(public, "test-password", "")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := Addr(public, "test-password", "relay.example:8443")
	if err != nil || old != saved {
		t.Fatalf("saved private relay changed public address: %v", err)
	}
}
