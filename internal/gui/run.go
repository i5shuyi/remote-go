// Package gui is the desktop app's interface, drawn with NSGo's native
// views (AppKit on macOS, Direct2D on Windows): a main window like UU
// Remote's, and a window per remote session whose screen is a native
// view decoding H.264 in hardware (package screen).
package gui

import (
	"context"
	"log"
	"os"
	"reflect"
	"runtime"
	"time"

	"github.com/nsgo-dev/nsgo"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/desktop"
	"github.com/nsgo-dev/nsgo/ui"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/liveupdate"
	"github.com/i5shuyi/remote-go/internal/screen"
)

// Run opens the app and returns when it quits.
func Run() error {
	var local *backend.App
	var panel *liveupdate.Panel
	if !core.App().Development && (runtime.GOOS == "windows" || runtime.GOOS == "darwin") {
		var err error
		panel, err = liveupdate.NewPanel()
		if err != nil {
			return err
		}
		watchUpdateRestart()
	} else {
		local = backend.New()
	}
	resume := panel != nil && panel.ResumingSessions()
	var explicitQuit bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nsgo.Run(func(app *core.Scope) {
		g := &gui{app: app, local: local, panel: panel}
		g.st = core.NewSignalFunc(app, backend.StateDTO{}, func(a, b backend.StateDTO) bool { return false })
		g.page = core.NewSignal(app, "home")
		g.toasts = ui.NewToaster(app)
		g.pending = core.NewSignal(app, "")
		g.busy = core.NewSignal(app, false)

		if local != nil {
			g.be = local
			local.OnChange = func() { app.Post(func() { g.setState(local.State()) }) }
			local.Verbose = os.Getenv("RG_VERBOSE") != ""
			local.Start()
			g.st.Set(local.State())
		} else {
			g.be = panel
			panel.Report = func(err error) { app.Post(func() { g.toast(err) }) }
			go g.pollService(ctx, !resume)
		}
		app.OnQuit(func(context.Context) { explicitQuit = g.quitting && !updateRestarting(); cancel() })

		desktop.SetTitle(app, "Remote Go")
		if !resume {
			nsgo.SetMainView(app, g.main)
		}
		g.runInBackground()
		if local != nil {
			g.devSession()
		}
	},
		nsgo.WindowSize(980, 640),
		nsgo.WindowConstraints(core.WindowConstraints{MinWidth: 860, MinHeight: 560}),
		screen.Option(),
	)
	cancel()
	if local != nil {
		local.Stop()
	}
	if panel != nil && explicitQuit {
		if err := panel.Quit(); err != nil {
			log.Print(err)
		}
	}
	return nil
}

func (g *gui) pollService(ctx context.Context, startDev bool) {
	var previous *backend.StateDTO
	for ctx.Err() == nil {
		r, err := g.panel.Sync()
		if ctx.Err() != nil {
			return
		}
		state := r.State
		if err != nil {
			state = backend.StateDTO{Net: "error", NetError: "无法连接本机会话服务：" + err.Error()}
		}
		// Poll the control plane only. Video, input and clipboard never use it.
		if previous == nil || !reflect.DeepEqual(*previous, state) {
			previous = &state
			launchDev := startDev && err == nil
			if launchDev {
				startDev = false
			}
			g.app.Post(func() {
				g.setState(state)
				if launchDev {
					g.devSession()
				}
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// setState publishes worker state; a new local prompt brings the main
// window forward so the person sees it.
func (g *gui) setState(s backend.StateDTO) {
	before := len(g.st.GetUntracked().Members.Prompts)
	g.st.Set(s)
	if len(s.Members.Prompts) > before {
		g.showMain()
	}
}
