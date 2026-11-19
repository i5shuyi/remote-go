package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/i5shuyi/remote-go/internal/tunnel"
)

func TestSetRelay(t *testing.T) {
	a := New()
	a.ctx, a.cancel = context.WithCancel(context.Background())
	defer a.Stop()
	_, a.cancelHoming = context.WithCancel(a.ctx)
	a.cfgPath = filepath.Join(t.TempDir(), "app.json")
	a.cfg = Config{DeviceCode: "123456784", Homed: true}
	a.net = "ready"
	updates := make(chan struct{}, 1)
	a.OnChange = func() {
		select {
		case updates <- struct{}{}:
		default:
		}
	}
	waitCode := func(code string) {
		t.Helper()
		timeout := time.After(3 * time.Second)
		for {
			if s := a.State(); s.Net == "ready" && s.DeviceCode == code {
				return
			}
			select {
			case <-updates:
			case <-timeout:
				t.Fatalf("waiting for %s: %+v", code, a.State())
			}
		}
	}
	if err := a.SetRelay(0, "relay.example"); err != nil {
		t.Fatal(err)
	}
	waitCode("123456784")
	if err := a.SetRelay(tunnel.CustomRegion, ""); err == nil {
		t.Fatal("accepted an empty private relay")
	}
	if err := a.SetRelay(tunnel.CustomRegion, "relay.example:8443"); err != nil {
		t.Fatal(err)
	}
	waitCode("123456780")
	var disk Config
	b, err := os.ReadFile(a.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &disk); err != nil || !disk.Homed || disk.DeviceCode != "123456780" || disk.RelayRegion != tunnel.CustomRegion || disk.RelayAddress != "relay.example:8443" {
		t.Fatalf("unexpected saved relay: %+v, %v", disk, err)
	}
	if err := a.SetRelay(302, disk.RelayAddress); err != nil {
		t.Fatal(err)
	}
	waitCode("123456782")
}
