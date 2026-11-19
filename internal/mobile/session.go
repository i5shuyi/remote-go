package mobile

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nsgo-dev/nsgo/base"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/clipshare"
	"github.com/i5shuyi/remote-go/internal/controlball"
	"github.com/i5shuyi/remote-go/internal/sessionclip"
	"github.com/i5shuyi/remote-go/internal/tunnel"
)

//go:embed player.html
var playerHTML string

// debugPlayer logs the player page's messages (RG_DEBUG_PLAYER=1).
var debugPlayer = os.Getenv("RG_DEBUG_PLAYER") != ""

// playerBase gives the player page an https origin: WebCodecs needs a
// secure context.
const playerBase = "https://player.remote-go.invalid/"

// remote is one session: its connection and what its page shows.
type remote struct {
	id       string
	code, pw string // for reconnecting
	t        backend.Target

	mu  sync.Mutex
	s   *client.Session
	tun *tunnel.Conn

	web      *core.Ref
	ready    atomic.Bool  // the player page is listening
	inflight atomic.Int32 // frames posted to the page, not yet delivered
	needKey  atomic.Bool  // deltas are useless until the next keyframe

	clip    *sessionclip.Controller
	stats   *core.Signal[client.Stats]
	path    *core.Signal[string]
	control *core.Signal[bool]
	cfg     *core.Signal[client.Config]
	mode    *core.Signal[string]
	ended   *core.Signal[string]
	retry   *core.Signal[bool]
	panel   *core.Signal[bool] // the toolbar is open
	kb      *core.Signal[bool] // the keyboard bar is open
	confirm *core.Signal[bool] // asking whether to disconnect
}

func (r *remote) sess() *client.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.s
}

// open connects to the host and shows its screen; done runs on the UI
// thread with the outcome.
func (a *app) open(code, pw string, done func(error)) {
	b := make([]byte, 8)
	rand.Read(b)
	st := a.settings.GetUntracked()
	cx := a.cx
	r := &remote{id: hex.EncodeToString(b), code: code, pw: pw, web: &core.Ref{},
		stats: core.NewSignal(cx, client.Stats{}), path: core.NewSignal(cx, ""), control: core.NewSignal(cx, false),
		cfg:  core.NewSignal(cx, client.Config{Quality: st.Quality, FPS: st.FPS, Width: 1920}),
		mode: core.NewSignal(cx, st.Mode), ended: core.NewSignal(cx, ""), retry: core.NewSignal(cx, false),
		panel: core.NewSignal(cx, false), kb: core.NewSignal(cx, false), confirm: core.NewSignal(cx, false)}
	r.needKey.Store(true)
	r.clip = sessionclip.New(cx, r.sess, a.toast)

	h := client.Handler{
		OnClipboard: func(m clipshare.Message) { cx.Post(func() { r.clip.Receive(m) }) },
		OnVideo: func(string, int, int) {
			r.needKey.Store(true)
			r.postFrame(a, "v")
		},
		OnFrame: func(kind byte, data []byte) { r.frame(a, kind, data) },
		OnInfo: func(info client.Info) {
			cx.Post(func() {
				r.control.Set(info.Control)
				r.clip.Rich = info.ClipboardRich
				r.clip.SetAvailable(info.Control && info.Clipboard)
			})
		},
		OnStats: func(s client.Stats) { cx.Post(func() { r.stats.Set(s) }) },
		OnClose: func(err error) {
			log.Printf("session %s ended: %v", r.id, err)
			cx.Post(func() {
				r.clip.SetAvailable(false)
				switch {
				case errors.Is(err, client.ErrTimeout):
					r.ended.Set("对方已无响应：可能已退出程序、关闭了远程控制，或网络中断")
				case err != nil:
					r.ended.Set("连接已断开：" + err.Error())
				default:
					r.ended.Set("远程会话已结束")
				}
				r.kb.Set(false)
				r.panel.Set(false)
			})
		},
	}
	cfg := r.cfg.GetUntracked()
	logf := tunnel.Logf(a.verbose)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		rem, err := backend.Dial(ctx, code, pw, st.RelayAddress, cfg, h, logf)
		cx.Post(func() {
			if err != nil {
				if errors.Is(err, client.ErrBadCode) {
					err = errors.New("验证码错误，请向对方确认（连续错误 5 次会被锁定 1 分钟）")
				}
				done(err)
				return
			}
			r.t = rem.Target
			r.mu.Lock()
			r.s, r.tun = rem.Session, rem.Tunnel
			r.mu.Unlock()
			r.control.Set(rem.Target.Control)
			go r.watchPath(cx)
			a.remember(rem.Target)
			if old := a.session.GetUntracked(); old != nil {
				old.close()
			}
			a.session.Set(r)
			done(nil)
		})
	}()
}

func (r *remote) close() {
	r.clip.Set(false)
	if s := r.sess(); s != nil {
		go s.Close()
	}
}

// frame forwards one video frame to the page, dropping deltas while the
// page cannot keep up and asking for a keyframe to resynchronise.
func (r *remote) frame(a *app, kind byte, data []byte) {
	if !r.ready.Load() {
		return
	}
	var prefix string
	switch kind {
	case client.FrameH264Key:
		prefix = "k"
		r.needKey.Store(false)
	case client.FrameH264Diff:
		prefix = "d"
		if r.needKey.Load() {
			return
		}
		if r.inflight.Load() > 6 {
			r.needKey.Store(true)
			if s := r.sess(); s != nil {
				go s.RequestKeyframe()
			}
			return
		}
	case client.FrameJPEG:
		prefix = "j"
		if r.inflight.Load() > 3 {
			return
		}
	default:
		return
	}
	r.postFrame(a, prefix+base64.StdEncoding.EncodeToString(data))
}

func (r *remote) postFrame(a *app, msg string) {
	r.inflight.Add(1)
	a.cx.Post(func() {
		r.inflight.Add(-1)
		r.web.PostMessage(msg)
	})
}

// onMessage takes the player page's touches and requests.
func (r *remote) onMessage(a *app, data string) {
	var m struct {
		T    string  `json:"t"`
		X    float64 `json:"x"`
		Y    float64 `json:"y"`
		B    int     `json:"b"`
		DX   float64 `json:"dx"`
		DY   float64 `json:"dy"`
		Msg  string  `json:"msg"`
		H264 bool    `json:"h264"`
	}
	if json.Unmarshal([]byte(data), &m) != nil {
		return
	}
	s := r.sess()
	if debugPlayer && m.T != "mm" {
		log.Printf("player: %s", data)
	}
	switch m.T {
	case "ready":
		if r.ready.Swap(true) {
			r.web.PostMessage("m" + r.mode.GetUntracked()) // the page missed our answer
			return
		}
		r.needKey.Store(true)
		r.web.PostMessage("m" + r.mode.GetUntracked())
		if !m.H264 {
			r.ended.Set(errNoDecoder.Error() + "，请在应用商店更新「Android System WebView」后重试")
			r.close()
			return
		}
		if s != nil {
			go s.RequestKeyframe()
		}
	case "kf":
		r.needKey.Store(true)
		if s != nil {
			go s.RequestKeyframe()
		}
	case "error":
		a.toast(errors.New(m.Msg))
	case "mm":
		if s != nil && r.control.GetUntracked() {
			s.MouseMove(m.X, m.Y)
		}
	case "md", "mu":
		if s != nil && r.control.GetUntracked() {
			go s.MouseButton(m.B, m.T == "md", m.X, m.Y)
		}
	case "wh":
		if s != nil && r.control.GetUntracked() {
			go s.Wheel(m.DX, m.DY)
		}
	}
}

// watchPath reports whether the tunnel is direct or relayed until it ends.
func (r *remote) watchPath(cx *core.Scope) {
	r.mu.Lock()
	tun, s := r.tun, r.s
	r.mu.Unlock()
	if tun == nil {
		return
	}
	for i := 0; ; i++ {
		p := tun.Path(context.Background())
		cx.Post(func() { r.path.Set(p) })
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

func (r *remote) setCfg(f func(*client.Config)) {
	c := r.cfg.GetUntracked()
	f(&c)
	r.cfg.Set(c)
	if s := r.sess(); s != nil {
		go s.SetConfig(c)
	}
}

func (r *remote) statsText() string {
	if r.ended.Get() != "" {
		return "已断开"
	}
	s := r.stats.Get()
	if s.Codec == "" {
		return "连接中…"
	}
	out := fmt.Sprintf("%d fps · %.1f Mbps · %d ms", s.FPS, s.Mbps, s.RTT/time.Millisecond)
	switch p := r.path.Get(); {
	case p == "direct":
		out += " · 直连"
	case strings.HasPrefix(p, "relay"):
		out += " · 中继 " + strings.ToUpper(strings.TrimPrefix(p, "relay "))
	}
	if !r.control.Get() {
		out += " · 仅观看"
	}
	return out
}

// ------------------------------------------------------------------ views

func (a *app) sessionView(cx *core.Scope, r *remote) core.Node {
	release := cx.KeepAwake("远程控制中", core.KeepAwakeOptions{Display: true})
	cx.Cleanup(func() {
		release()
		r.close()
	})
	cx.SetStatusBar(core.StatusBarSetting{Hidden: core.ToggleOn})
	cx.OnBackPress(func() bool {
		switch {
		case r.kb.GetUntracked():
			r.kb.Set(false)
		case r.panel.GetUntracked():
			r.panel.Set(false)
		case r.ended.GetUntracked() != "":
			a.session.Set(nil)
		default:
			r.confirm.Set(true)
		}
		return true
	})

	return core.View{Class: "flex-1 bg-black", Children: []core.Node{
		core.Show{When: func() bool { return r.ended.Get() == "" }, Then: func(cx *core.Scope) core.Node {
			return core.View{Class: "flex-1", Children: []core.Node{
				core.WebView{Source: core.WebSource{HTML: playerHTML, BaseURL: playerBase}, NoScroll: true, Ref: r.web,
					Label: "远程屏幕", OnMessage: func(d string) { r.onMessage(a, d) },
					OnLoadStart: func(core.WebNavigation) { r.ready.Store(false) },
					Style:       core.Style{FlexGrow: 1}},
				a.toolbar(cx, r),
				core.Show{When: r.kb.Get, Then: func(cx *core.Scope) core.Node { return a.keyboardBar(cx, r) }},
			}}
		}, Else: func(cx *core.Scope) core.Node { return a.endedView(cx, r) }},
		a.disconnectDialog(cx, r),
	}}
}

// toolbar keeps the control ball's position while the session panel is open.
func (a *app) toolbar(cx *core.Scope, r *remote) core.Node {
	action := func(label string, svg func() *core.SVG, on func()) core.Node {
		return core.View{Class: "flex-1 items-center gap-1 py-1.5 rounded-xl", Role: core.RoleButton, Label: label,
			OnPress: func(core.PressEvent) { on() }, Children: []core.Node{
				icon(svg, "w-[22px] h-[22px] text-white"),
				text("text-[11px] text-white/80 truncate", label),
			}}
	}
	position := core.NewSignal(cx, controlball.TopRight())
	return core.Show{When: func() bool { return !r.panel.Get() }, Then: func(cx *core.Scope) core.Node {
		return mobileControlBall(cx, position, func() { r.panel.Set(true) })
	}, Else: func(cx *core.Scope) core.Node {
		return core.View{Class: "absolute top-0 left-0 right-0 items-end pt-[env(safe-area-inset-top)] px-3", Style: core.Style{PointerEvents: core.PointerEventsBoxNone}, Children: []core.Node{
			core.View{Class: "mt-2 self-stretch rounded-2xl bg-black/80 border border-white/15 p-3 gap-3", Children: []core.Node{
				core.View{Class: "flex-row items-center gap-2", Children: []core.Node{
					core.View{ClassFunc: func() string {
						if r.stats.Get().FPS > 0 {
							return "w-[8px] h-[8px] rounded-full bg-green-500"
						}
						return "w-[8px] h-[8px] rounded-full bg-amber-500"
					}},
					core.View{Class: "flex-1 shrink", Children: []core.Node{
						text("text-sm font-semibold text-white truncate", r.t.Name+"  "+groupCode(r.t.Code)),
						textf("text-xs text-white/70 truncate", r.statsText),
					}},
					ui.IconButton{Icon: lucide.ChevronUp, Label: "收起", OnPress: func() { r.panel.Set(false) }},
				}},
				core.View{Class: "flex-row", Children: []core.Node{
					action("键盘", lucide.Keyboard, func() { r.panel.Set(false); r.kb.Set(true) }),
					core.Show{When: func() bool { return r.mode.Get() == "touch" },
						Then: func(*core.Scope) core.Node {
							return action("触控", lucide.Pointer, func() { a.setMode(r, "pad") })
						},
						Else: func(*core.Scope) core.Node {
							return action("触摸板", lucide.Touchpad, func() { a.setMode(r, "touch") })
						}},
					action("适应屏幕", lucide.Shrink, func() { r.web.PostMessage("z") }),
					action("C+A+Del", lucide.Command, func() {
						if s := r.sess(); s != nil {
							go s.SendKeys("ControlLeft", "AltLeft", "Delete")
						}
					}),
					action("断开", lucide.Power, func() { r.confirm.Set(true) }),
				}},
				core.View{Class: "flex-row gap-2", Children: []core.Node{
					ui.Select[int]{Options: qualities, Label: "画质", Value: func() int { return r.cfg.Get().Quality },
						OnChange: func(v int) { r.setCfg(func(c *client.Config) { c.Quality = v }) }, Style: core.Style{FlexGrow: 1, FlexShrink: 1, FlexBasis: core.Pt(0)}},
					ui.Select[int]{Options: rates, Label: "帧率", Value: func() int { return r.cfg.Get().FPS },
						OnChange: func(v int) { r.setCfg(func(c *client.Config) { c.FPS = v }) }, Style: core.Style{FlexGrow: 1, FlexShrink: 1, FlexBasis: core.Pt(0)}},
				}},
				core.View{Class: "flex-row items-center justify-between", Children: []core.Node{
					text("text-sm text-white", "共享剪贴板"),
					ui.Switch{Label: "共享剪贴板", Checked: r.clip.Enabled.Get, Disabled: func() bool { return !r.clip.Available.Get() }, OnChange: r.clip.Set},
				}},
				core.Show{When: func() bool { return r.clip.Status.Get() != "" }, Then: func(cx *core.Scope) core.Node { return textf("text-xs text-white/70", r.clip.Status.Get) }},
				textf("text-xs text-white/60 leading-4", func() string {
					if r.mode.Get() == "pad" {
						return "触摸板：单指移动光标，轻点＝左键，双指轻点＝右键，轻点后按住拖动＝拖拽，双指滑动＝滚动，双指张合＝缩放"
					}
					return "触控：轻点＝左键，长按＝右键，单指拖动＝拖拽，双指滑动＝滚动，双指张合＝缩放"
				}),
			}},
		}}
	}}
}

// The overlay passes touches outside the ball through to the remote screen.
func mobileControlBall(cx *core.Scope, position *core.Signal[controlball.Position], open func()) core.Node {
	size := core.NewSignal(cx, cx.Env().Window())
	var dragStart controlball.Position
	return core.View{StyleFunc: func() core.Style {
		safe := cx.Env().SafeArea()
		return core.Style{
			Position: core.Absolute, PhysicalEdges: core.PhysicalEdges{Inset: core.SidesHorizontal},
			Left: core.Pt(safe.Left), Right: core.Pt(safe.Right),
			Top: core.Pt(safe.Top), Bottom: core.Pt(max(safe.Bottom, cx.Env().KeyboardHeight())),
			PointerEvents: core.PointerEventsBoxNone,
		}
	}, OnLayout: func(b core.Bounds) { size.Set(b.Size) }, Children: []core.Node{
		core.View{Class: "rounded-full bg-black/55 border border-white/20 items-center justify-center", Role: core.RoleButton, Label: "工具栏",
			StyleFunc: func() core.Style {
				s := size.Get()
				x, y := position.Get().Origin(float64(s.Width), float64(s.Height))
				return core.Style{
					Position: core.Absolute, PhysicalEdges: core.PhysicalEdges{Inset: core.SideLeft},
					Width: core.Pt(controlball.Size), Height: core.Pt(controlball.Size),
					Left: core.Pt(core.Points(x)), Top: core.Pt(core.Points(y)),
				}
			},
			OnPress: func(core.PressEvent) { open() },
			OnPan: func(e core.PanEvent) {
				if e.Phase == core.GestureBegan {
					dragStart = position.GetUntracked()
				}
				if e.Phase == core.GestureCancelled {
					position.Set(dragStart)
					return
				}
				s := size.GetUntracked()
				w, h := float64(s.Width), float64(s.Height)
				x, y := dragStart.Origin(w, h)
				position.Set(dragStart.Move(x+float64(e.Translation.X), y+float64(e.Translation.Y), w, h, e.Phase == core.GestureEnded))
			},
			Children: []core.Node{icon(lucide.Ellipsis, "w-[22px] h-[22px] text-white")},
		},
	}}
}

func (a *app) setMode(r *remote, m string) {
	r.mode.Set(m)
	r.web.PostMessage("m" + m)
	st := a.settings.GetUntracked()
	st.Mode = m
	a.saveSettings(st)
}

func (a *app) disconnectDialog(cx *core.Scope, r *remote) core.Node {
	return ui.Dialog{
		Open:         r.confirm.Get,
		OnOpenChange: func(open bool, _ base.Reason) { r.confirm.Set(open) },
		Title:        "断开远程连接？",
		Description:  "断开后回到首页，可以在「最近连接」里重新连接。",
		Footer: []core.Node{
			ui.Button{Text: "取消", Variant: ui.ButtonOutline, OnPress: func() { r.confirm.Set(false) }},
			ui.Button{Text: "断开", OnPress: func() {
				r.confirm.Set(false)
				r.close()
				a.session.Set(nil)
			}},
		},
	}
}

// endedView replaces the screen once the session ends.
func (a *app) endedView(cx *core.Scope, r *remote) core.Node {
	reconnect := func() {
		r.retry.Set(true)
		a.open(r.code, r.pw, func(err error) {
			r.retry.Set(false)
			if err != nil {
				r.ended.Set("重新连接失败：" + err.Error())
			}
		})
	}
	return core.View{Class: "flex-1 items-center justify-center bg-background gap-4 p-8", Children: []core.Node{
		core.View{Class: "w-[56px] h-[56px] rounded-full bg-muted items-center justify-center", Children: []core.Node{
			icon(lucide.Unplug, "w-[28px] h-[28px] text-muted-foreground"),
		}},
		text("text-lg font-semibold", "远程会话已断开"),
		textf("text-sm text-muted-foreground text-center", r.ended.Get),
		core.View{Class: "self-stretch gap-3 mt-2", Children: []core.Node{
			ui.Button{Text: "重新连接", Size: ui.SizeLarge, Icon: lucide.RefreshCw, Busy: r.retry.Get, OnPress: reconnect},
			ui.Button{Text: "返回首页", Size: ui.SizeLarge, Variant: ui.ButtonOutline, OnPress: func() { a.session.Set(nil) }},
		}},
	}}
}
