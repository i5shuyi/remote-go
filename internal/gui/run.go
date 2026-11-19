// Package gui is the desktop app's interface, drawn with NSGo's native
// views (AppKit on macOS, Direct2D on Windows): a main window like UU
// Remote's, and a window per remote session whose screen is a native
// view decoding H.264 in hardware (package screen).
package gui

import (
	"os"

	"github.com/nsgo-dev/nsgo"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/desktop"
	"github.com/nsgo-dev/nsgo/ui"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/screen"
)

// Run opens the app and returns when it quits.
func Run() error {
	be := backend.New()
	nsgo.Run(func(app *core.Scope) {
		g := &gui{app: app, be: be}
		g.st = core.NewSignalFunc(app, backend.StateDTO{}, func(a, b backend.StateDTO) bool { return false })
		g.page = core.NewSignal(app, "home")
		g.toasts = ui.NewToaster(app)
		g.pending = core.NewSignal(app, "")
		g.busy = core.NewSignal(app, false)

		be.OnChange = func() { app.Post(func() { g.st.Set(be.State()) }) }
		be.Verbose = os.Getenv("RG_VERBOSE") != ""
		be.Start()
		g.st.Set(be.State())

		desktop.SetTitle(app, "Remote Go")
		nsgo.SetMainView(app, g.main)
		g.runInBackground()
		g.devSession()
	},
		nsgo.WindowSize(980, 640),
		nsgo.WindowConstraints(core.WindowConstraints{MinWidth: 860, MinHeight: 560}),
		screen.Option(),
	)
	be.Stop()
	return nil
}
