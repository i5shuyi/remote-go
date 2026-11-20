package gui

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/nsgo-dev/nsgo"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/desktop"
	"github.com/nsgo-dev/nsgo/ui"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/encoder"
	"github.com/i5shuyi/remote-go/internal/liveupdate"
	"github.com/i5shuyi/remote-go/internal/screen"
	"github.com/i5shuyi/remote-go/internal/tunnel"
)

func SessionWindow() error {
	var w liveupdate.WindowRequest
	if err := json.NewDecoder(os.Stdin).Decode(&w); err != nil {
		return err
	}
	nsgo.Run(func(app *core.Scope) {
		g := &gui{app: app, toasts: ui.NewToaster(app)}
		g.st = core.NewSignalFunc(app, backend.StateDTO{Quality: w.Quality, FPS: w.FPS}, func(a, b backend.StateDTO) bool { return false })
		desktop.SetShutdownMode(app, core.ShutdownOnLastWindowClose)
		go func() { io.Copy(io.Discard, os.Stdin); app.Post(app.Quit) }()
		var target backend.Target
		g.openSessionWith(func(cfg client.Config, h client.Handler) (*backend.Remote, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			if w.Member != "" {
				p := liveupdate.Proxy{Addr: w.MemberAddr, Token: w.MemberToken, Name: w.MemberName}
				w.MemberAddr = "" // the first proxy is single-use; reconnects ask again
				if p.Addr == "" {
					var err error
					if p, err = liveupdate.RequestMemberProxy(w.Member); err != nil {
						return nil, err
					}
				}
				r, err := backend.DialProxy(ctx, p.Addr, p.Token, cfg, h)
				if err == nil {
					r.Target.Member, r.Target.Name = w.Member, p.Name
					target = r.Target
				}
				return r, err
			}
			if w.Local != "" {
				var d net.Dialer
				s, err := client.Dial(ctx, d.DialContext, w.Local, w.Password, encoder.CanDecodeH264(), cfg, h)
				if err != nil {
					return nil, err
				}
				target = backend.Target{Code: w.Local, Name: "开发会话", Control: true}
				return &backend.Remote{Target: target, Session: s}, nil
			}
			r, err := backend.Dial(ctx, w.Code, w.Password, w.Relay, cfg, h, tunnel.Logf(os.Getenv("RG_VERBOSE") != ""))
			if err == nil {
				target = r.Target
			}
			return r, err
		}, func(err error) {
			result := liveupdate.WindowResult{Target: target}
			if err != nil {
				result.Error = err.Error()
			}
			json.NewEncoder(os.Stdout).Encode(result)
			if err != nil {
				app.Quit()
			}
		})
	}, nsgo.InstanceKey("session-"+strconv.Itoa(os.Getpid())), screen.Option())
	return nil
}
