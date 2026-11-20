// Package mobile is Remote Go's phone app: a controller only. It connects
// to a desktop running Remote Go through tailcat with the device code and
// verification code, and shows the remote screen in a web view whose
// WebCodecs decoder runs on the phone's hardware (MediaCodec on Android).
// NSGo's Android runtime cannot host views of the app's own, so a web view
// is the one way to reach the hardware decoder.
package mobile

import (
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/nsgo-dev/nsgo"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/ui"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/members"
)

// Recent is a device the phone connected to.
type Recent struct {
	Code string    `json:"code"`
	Name string    `json:"name"`
	OS   string    `json:"os"`
	When time.Time `json:"when"`
}

// Settings are the defaults for new sessions.
type Settings struct {
	RelayAddress string `json:"relayAddress,omitempty"`
	Quality      int    `json:"quality"`
	FPS          int    `json:"fps"`
	Mode         string `json:"mode"` // "touch" or "pad"
}

type app struct {
	cx       *core.Scope
	toasts   *ui.Toaster
	recent   *core.Signal[[]Recent]
	settings *core.Signal[Settings]
	session  *core.Signal[*remote] // the open session, nil on the home page
	verbose  bool

	node    *members.Node // "my devices": this phone is a controller only
	mem     *core.Signal[members.Snapshot]
	prompt  *core.Signal[*pairPrompt]
	pairing *core.Signal[string]
}

// Run opens the phone app and returns when it ends. On a desktop it opens
// a phone-sized window, for development (RG_MOBILE=1).
func Run() error {
	nsgo.Run(func(cx *core.Scope) {
		a := &app{cx: cx, toasts: ui.NewToaster(cx), verbose: os.Getenv("RG_VERBOSE") != ""}
		a.recent = core.NewSignalFunc(cx, a.load(), func(x, y []Recent) bool { return false })
		a.settings = core.NewSignal(cx, a.loadSettings())
		a.session = core.NewSignal[*remote](cx, nil)
		a.openMembers()
		nsgo.SetMainView(cx, a.main)
		a.devConnect()
	}, nsgo.WindowSize(400, 820))
	return nil
}

func (a *app) main(cx *core.Scope) core.Node {
	return core.View{Class: "flex-1 bg-background", Children: []core.Node{
		core.Show{When: func() bool { return a.session.Get() == nil }, Then: a.home},
		// Keyed by session, so a reconnect builds a fresh view.
		core.Each(func() []*remote {
			if r := a.session.Get(); r != nil {
				return []*remote{r}
			}
			return nil
		}, func(r *remote) string { return r.id }, func(cx *core.Scope, r func() *remote, _ func() int) core.Node {
			return a.sessionView(cx, r())
		}),
		ui.Toasts{Toaster: a.toasts},
	}}
}

func (a *app) toast(err error) {
	a.toasts.Show(ui.ToastContent{Type: ui.ToastError, Title: err.Error()})
}

func (a *app) notice(msg string) {
	a.toasts.Show(ui.ToastContent{Type: ui.ToastSuccess, Title: msg, Duration: 2 * time.Second})
}

// ------------------------------------------------------------------ storage

const (
	keyRecent   = "recent"
	keySettings = "settings"
)

func (a *app) load() []Recent {
	var r []Recent
	if s, ok := a.cx.Storage().GetItem(keyRecent); ok {
		json.Unmarshal([]byte(s), &r)
	}
	return r
}

func (a *app) loadSettings() Settings {
	st := Settings{Quality: 70, FPS: 30, Mode: "touch"}
	if s, ok := a.cx.Storage().GetItem(keySettings); ok {
		json.Unmarshal([]byte(s), &st)
	}
	return st
}

func (a *app) saveSettings(st Settings) error {
	b, _ := json.Marshal(st)
	if err := a.cx.Storage().SetItem(keySettings, string(b)); err != nil {
		log.Printf("save settings: %v", err)
		return err
	}
	a.settings.Set(st)
	return nil
}

func (a *app) remember(t backend.Target) {
	if t.Code == "" { // "my devices" sessions are listed there instead
		return
	}
	r := []Recent{{Code: t.Code, Name: t.Name, OS: t.OS, When: time.Now()}}
	for _, x := range a.recent.GetUntracked() {
		if x.Code != t.Code && len(r) < 12 {
			r = append(r, x)
		}
	}
	a.setRecent(r)
}

func (a *app) forget(code string) {
	var r []Recent
	for _, x := range a.recent.GetUntracked() {
		if x.Code != code {
			r = append(r, x)
		}
	}
	a.setRecent(r)
}

func (a *app) setRecent(r []Recent) {
	a.recent.Set(r)
	b, _ := json.Marshal(r)
	if err := a.cx.Storage().SetItem(keyRecent, string(b)); err != nil {
		log.Printf("save recent: %v", err)
	}
}
