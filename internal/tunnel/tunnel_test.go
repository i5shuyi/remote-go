package tunnel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestCodes(t *testing.T) {
	c, err := NewDeviceCode(303)
	if err != nil || !Valid(c) {
		t.Fatalf("code %q %v", c, err)
	}
	if r, _ := Region(c[:3] + " " + c[3:6] + "-" + c[6:]); r != 303 {
		t.Fatalf("region %d", r)
	}
	a1, _ := Addr(c, "abc234", "")
	a2, _ := Addr(c, "abc234", "")
	a3, _ := Addr(c, "abc235", "")
	if a1 != a2 || a1 == a3 || len(a1) < 40 {
		t.Fatalf("addr derivation: %q %q %q", a1, a2, a3)
	}
	if id, err := Region("123456780"); err != nil || id != CustomRegion {
		t.Fatal("region digit 0 must select a custom relay")
	}
}

// TestTailcat connects through tailcat's public DERP relays.
// RG_TAILCAT_E2E=1 go test ./internal/tunnel -run TestTailcat -v
func TestTailcat(t *testing.T) {
	if os.Getenv("RG_TAILCAT_E2E") == "" {
		t.Skip("set RG_TAILCAT_E2E=1 (needs the internet)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t0 := time.Now()
	region, err := NearestRegion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := NewDeviceCode(region)
	t.Logf("nearest region %d (%v), device code %s", region, time.Since(t0), code)

	t0 = time.Now()
	ln, err := Listen(ctx, code, "pass99", "", Logf(testing.Verbose() && os.Getenv("RG_TAILCAT_VERBOSE") != ""))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Logf("host listening (%v)", time.Since(t0))
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "meow") }))

	t0 = time.Now()
	c, err := Dial(ctx, code, "pass99", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Logf("tunnel up (%v)", time.Since(t0))
	hc := &http.Client{Transport: &http.Transport{DialContext: c.DialContext}}
	resp, err := hc.Get("http://host/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "meow" {
		t.Fatalf("got %q", b)
	}
	time.Sleep(2 * time.Second)
	t.Logf("path: %s", c.Path(ctx))

	wctx, wcancel := context.WithTimeout(ctx, 8*time.Second)
	defer wcancel()
	if _, err := Dial(wctx, code, "wrong1", "", nil); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestMemberRelay(t *testing.T) {
	for r, want := range map[MemberRelay]string{
		{Region: 304}:                      `{"region":304}`,
		{Address: "derp.example.com:8443"}: `{"address":"derp.example.com:8443"}`,
		{Address: "[2001:db8::1]:443"}:     `{"address":"[2001:db8::1]:443"}`,
	} {
		if got, err := r.Canonical(); err != nil || string(got) != want {
			t.Errorf("%+v → %s %v", r, got, err)
		}
	}
	for _, r := range []MemberRelay{{}, {Region: 900}, {Region: 304, Address: "a.b:443"}, {Address: "DERP.example.com"}} {
		if _, err := r.Canonical(); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	e := MemberEndpoint{GroupKey: make([]byte, 32), GroupID: make([]byte, 32), KeyVersion: make([]byte, 32), MemberID: make([]byte, 32), Relay: MemberRelay{Region: 301}}
	a1, _ := e.Addr()
	e.Relay = MemberRelay{Region: 302}
	a2, _ := e.Addr()
	if a1 == a2 || a1 == "" {
		t.Fatal("relay must change the member address")
	}
}
