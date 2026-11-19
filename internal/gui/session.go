package gui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/desktop"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/clipshare"
	"github.com/i5shuyi/remote-go/internal/screen"
	"github.com/i5shuyi/remote-go/internal/sessionclip"
	"github.com/i5shuyi/remote-go/internal/tunnel"
)

// remote is one open session and its window.
type remote struct {
	t    backend.Target
	id   string
	link *screen.Link
	clip *sessionclip.Controller

	mu  sync.Mutex
	s   *client.Session
	tun *tunnel.Conn // nil for development sessions

	path *core.Signal[string] // "direct" or "relay xxx"

	win     *desktop.Window
	stats   *core.Signal[client.Stats]
	control *core.Signal[bool]
	cfg     *core.Signal[client.Config]
	ended   *core.Signal[string] // why the session ended, "" while it runs
	retry   *core.Signal[bool]   // reconnecting
	closed  bool                 // the window went; UI thread only

	dial func(client.Config, client.Handler) (*backend.Remote, error)
}

// The screen view's input goes to the session once it is open.
func (r *remote) sess() *client.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.s
}

func (r *remote) MouseMove(x, y float64) {
	if s := r.sess(); s != nil {
		s.MouseMove(x, y)
	}
}

func (r *remote) MouseButton(b int, down bool, x, y float64) {
	if s := r.sess(); s != nil {
		s.MouseButton(b, down, x, y)
	}
}

func (r *remote) Wheel(dx, dy float64) {
	if s := r.sess(); s != nil {
		s.Wheel(dx, dy)
	}
}

func (r *remote) Key(code string, down bool) {
	if s := r.sess(); s != nil {
		s.Key(code, down)
	}
}

func (r *remote) ReleaseKeys() {
	if s := r.sess(); s != nil {
		s.ReleaseKeys()
	}
}

func (r *remote) RequestKeyframe() {
	if s := r.sess(); s != nil {
		s.RequestKeyframe()
	}
}

// openSession dials the device and opens its window; done reports the
// outcome on the UI thread.
func (g *gui) openSession(code, password string, done func(error)) {
	g.openSessionWith(func(cfg client.Config, h client.Handler) (*backend.Remote, error) {
		return g.be.Connect(code, password, cfg, h)
	}, done)
}

func (g *gui) openSessionWith(dial func(client.Config, client.Handler) (*backend.Remote, error), done func(error)) {
	b := make([]byte, 8)
	rand.Read(b)
	r := &remote{id: hex.EncodeToString(b), dial: dial}
	r.clip = sessionclip.New(g.app, r.sess, g.toast)
	r.path = core.NewSignal(g.app, "")
	r.stats = core.NewSignal(g.app, client.Stats{})
	r.control = core.NewSignal(g.app, false)
	r.ended = core.NewSignal(g.app, "")
	r.retry = core.NewSignal(g.app, false)
	r.link = screen.NewLink(r.id, r)
	st := g.st.GetUntracked()
	cfg := client.Config{Quality: st.Quality, FPS: st.FPS, Width: 1920}
	if cfg.Quality == 0 {
		cfg.Quality = 70
	}
	if cfg.FPS == 0 {
		cfg.FPS = 30
	}

	h := client.Handler{
		OnClipboard: func(m clipshare.Message) { g.app.Post(func() { r.clip.Receive(m) }) },
		OnVideo:     func(string, int, int) { r.link.Reset() },
		OnFrame:     r.link.Frame,
		OnInfo: func(info client.Info) {
			r.link.SetControl(info.Control)
			g.app.Post(func() {
				r.control.Set(info.Control)
				r.clip.Rich = info.ClipboardRich
				r.clip.SetAvailable(info.Control && info.Clipboard)
			})
		},
		OnStats: func(s client.Stats) {
			g.app.Post(func() { r.stats.Set(s) })
		},
		OnClose: func(err error) {
			log.Printf("session %s ended: %v", r.id, err)
			r.link.Close()
			g.app.Post(func() {
				r.clip.SetAvailable(false)
				if r.closed {
					return
				}
				// Keep the window, with the reason instead of a frozen
				// last frame, so the user can reconnect.
				switch {
				case errors.Is(err, client.ErrTimeout):
					r.ended.Set("对方已无响应：可能已退出程序、关闭了远程控制，或网络中断")
				case err != nil:
					r.ended.Set("连接已断开：" + err.Error())
				default:
					r.ended.Set("对方结束了远程会话")
				}
			})
		},
	}
	var rem *backend.Remote
	g.async(func() (err error) {
		rem, err = dial(cfg, h)
		return
	}, func(err error) {
		if err != nil {
			r.link.Close()
			if errors.Is(err, client.ErrBadCode) {
				err = errors.New("验证码错误，请向对方确认（连续错误 5 次会被锁定 1 分钟）")
			}
			done(err)
			return
		}
		s, t := rem.Session, rem.Target
		r.t = t
		r.mu.Lock()
		r.s, r.tun = s, rem.Tunnel
		r.mu.Unlock()
		if rem.Tunnel != nil {
			go r.watchPath(g)
		}
		r.control.Set(t.Control)
		r.cfg = core.NewSignal(g.app, cfg)
		r.link.SetControl(t.Control)
		title := fmt.Sprintf("%s（%s）- Remote Go", t.Name, groupCode(t.Code))
		r.win = desktop.OpenWindow(g.app, desktop.WindowOptions{Title: title, Width: 1280, Height: 800,
			Constraints: core.WindowConstraints{MinWidth: 640, MinHeight: 400}}, func(cx *core.Scope) core.Node {
			return g.sessionView(cx, r)
		})
		r.win.OnClosing(g.app, func(*desktop.WindowClosing) {
			r.clip.Set(false)
			r.closed = true
			go s.Close()
		})
		done(nil)
	})
}

// watchPath reports whether the tunnel is direct or relayed until it ends.
func (r *remote) watchPath(g *gui) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; ; i++ {
		r.mu.Lock()
		tun, s := r.tun, r.s
		r.mu.Unlock()
		p := tun.Path(ctx)
		g.app.Post(func() { r.path.Set(p) })
		d := 3 * time.Second
		if i > 5 {
			d = 15 * time.Second
		}
		select {
		case <-time.After(d):
		case <-s.Done():
			return
		}
	}
}

func (g *gui) sessionView(cx *core.Scope, r *remote) core.Node {
	cx.Cleanup(func() {
		r.clip.Set(false)
		r.closed = true
		if s := r.sess(); s != nil {
			go s.Close()
		}
	})
	return core.Show{When: func() bool { return r.ended.Get() == "" },
		Then: func(cx *core.Scope) core.Node {
			ref := &core.Ref{}
			return core.Native{Component: screen.Component, Props: map[string]any{"link": r.id}, Ref: ref,
				Label: "远程屏幕", Style: core.Style{FlexGrow: 1, FlexShrink: 1},
				OnEvent: func(name string, data any) {
					if name == screen.ControlsEvent {
						desktop.PopupMenu(cx, r.controlMenu(cx), data.(core.Point), func() {
							if !r.closed && r.ended.GetUntracked() == "" {
								ref.Command("focus", nil)
							}
						})
					}
				}}
		},
		Else: func(cx *core.Scope) core.Node { return g.endedView(cx, r) }}
}

func (r *remote) controlMenu(cx *core.Scope) []desktop.MenuItem {
	cfg := r.cfg.GetUntracked()
	setting := func(label string, options []ui.Option[int], value int, change func(*client.Config, int)) desktop.MenuItem {
		item := desktop.MenuItem{Label: label}
		for _, opt := range options {
			item.Items = append(item.Items, desktop.MenuItem{Label: opt.Label, Radio: true, Checked: opt.Value == value,
				OnSelect: func() {
					c := r.cfg.GetUntracked()
					change(&c, opt.Value)
					r.cfg.Set(c)
					if s := r.sess(); s != nil {
						go s.SetConfig(c)
					}
				}})
		}
		return item
	}
	s := r.stats.GetUntracked()
	status := "连接中…"
	if s.Codec != "" {
		status = fmt.Sprintf("%s · %d fps · %.1f Mbps · %d ms", s.Codec, s.FPS, s.Mbps, s.RTT/time.Millisecond)
	}
	switch p := r.path.GetUntracked(); {
	case p == "direct":
		status += " · P2P 直连"
	case strings.HasPrefix(p, "relay"):
		status += " · 中继 " + strings.ToUpper(strings.TrimPrefix(p, "relay "))
	}
	control := r.control.GetUntracked()
	if !control {
		status += " · 仅观看"
	}
	if s.Error != "" {
		status += " · " + s.Error
	}
	items := []desktop.MenuItem{
		{Label: r.t.Name + "  " + groupCode(r.t.Code), Disabled: true},
		{Label: status, Disabled: true},
		{Separator: true},
		setting("画质", qualities, cfg.Quality, func(c *client.Config, v int) { c.Quality = v }),
		setting("帧率", rates, cfg.FPS, func(c *client.Config, v int) { c.FPS = v }),
		setting("分辨率", widths, cfg.Width, func(c *client.Config, v int) { c.Width = v }),
		{Separator: true},
		{Label: "共享剪贴板", Checkable: true, Checked: r.clip.Enabled.GetUntracked(),
			Disabled: !control || !r.clip.Available.GetUntracked(),
			OnSelect: func() { r.clip.Set(!r.clip.Enabled.GetUntracked()) }},
		{Label: "Ctrl+Alt+Del", Disabled: !control, OnSelect: func() {
			if s := r.sess(); s != nil && r.control.GetUntracked() {
				go s.SendKeys("ControlLeft", "AltLeft", "Delete")
			}
		}},
		{Label: "切换全屏", OnSelect: func() { desktop.ToggleFullScreen(cx) }},
		{Label: "断开连接", OnSelect: func() { r.win.Close() }},
	}
	if status := r.clip.Status.GetUntracked(); status != "" {
		items = append(items, desktop.MenuItem{Label: status, Disabled: true})
	}
	return items
}

// endedView replaces the screen once the session ends.
func (g *gui) endedView(cx *core.Scope, r *remote) core.Node {
	reconnect := func() {
		r.retry.Set(true)
		g.openSessionWith(r.dial, func(err error) {
			if r.closed {
				return
			}
			r.retry.Set(false)
			if err != nil {
				r.ended.Set("重新连接失败：" + err.Error())
				return
			}
			r.win.Close() // the new session has its own window
		})
	}
	return core.View{Class: "flex-1 items-center justify-center bg-background gap-4 p-8", Children: []core.Node{
		core.View{Class: "w-[56px] h-[56px] rounded-full bg-muted items-center justify-center", Children: []core.Node{
			icon(lucide.Unplug, "w-[28px] h-[28px] text-muted-foreground"),
		}},
		text("text-lg font-semibold", "远程会话已断开"),
		textf("text-sm text-muted-foreground text-center max-w-[460px]", r.ended.Get),
		core.View{Class: "flex-row gap-3 mt-2", Children: []core.Node{
			core.Show{When: r.retry.Get,
				Then: func(*core.Scope) core.Node {
					return ui.Button{Text: "正在重新连接…", Icon: lucide.LoaderCircle, Disabled: func() bool { return true }}
				},
				Else: func(*core.Scope) core.Node {
					return ui.Button{Text: "重新连接", Icon: lucide.RefreshCw, OnPress: reconnect}
				}},
			ui.Button{Text: "关闭窗口", Variant: ui.ButtonOutline, OnPress: func() { r.win.Close() }},
		}},
	}}
}
