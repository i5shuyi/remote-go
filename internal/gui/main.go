package gui

import (
	"errors"
	"runtime"

	"strings"
	"time"

	"github.com/nsgo-dev/nsgo/base"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/desktop"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"

	"github.com/i5shuyi/remote-go/assets"
	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/liveupdate"
	"github.com/i5shuyi/remote-go/internal/tunnel"
)

type settingsBackend interface {
	SetName(string) error
	SetFixedPassword(string) error
	RefreshPassword() string
	SetAllowControl(bool) error
	SetRelay(int, string) error
	SetDefaults(int, int)
	ForgetRecent(string)
	KickViewers()
	membersBackend
}

type gui struct {
	app     *core.Scope
	be      settingsBackend
	local   *backend.App
	panel   *liveupdate.Panel
	st      *core.Signal[backend.StateDTO]
	page    *core.Signal[string]
	toasts  *ui.Toaster
	pending *core.Signal[string] // device code waiting for its verification code
	busy    *core.Signal[bool]   // resolving a device

	mainWin  *desktop.Window
	quitting bool
}

// ------------------------------------------------------------------ helpers

func (g *gui) toast(err error) {
	g.toasts.Show(ui.ToastContent{Type: ui.ToastError, Title: err.Error()})
}

func (g *gui) notice(msg string) {
	g.toasts.Show(ui.ToastContent{Type: ui.ToastSuccess, Title: msg, Duration: 2 * time.Second})
}

// async runs fn off the UI thread and then done on it.
func (g *gui) async(fn func() error, done func(error)) {
	go func() {
		err := fn()
		g.app.Post(func() { done(err) })
	}()
}

func text(class string, s string) core.Node { return core.Text{Class: class, Content: s} }

func textf(class string, fn func() string) core.Node { return core.Text{Class: class, ContentFunc: fn} }

func icon(svg func() *core.SVG, class string) core.Node { return core.Icon{SVG: svg(), Class: class} }

func section(children ...core.Node) core.Node {
	return core.View{Class: "gap-3 py-2", Children: children}
}

func groupCode(c string) string {
	if len(c) == 9 {
		return c[:3] + " " + c[3:6] + " " + c[6:]
	}
	return c
}

func osName(os string) string {
	switch os {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	}
	return os
}

// ------------------------------------------------------------------ window

// The macOS title bar shares its background with the sidebar and page header.
func mainWindowDrag() core.DesktopViewProps {
	if runtime.GOOS == "darwin" {
		return core.DesktopViewProps{WindowDrag: core.WindowDragOn}
	}
	return core.DesktopViewProps{}
}

func pageHeader(title string, actions ...core.Node) core.Node {
	children := []core.Node{text("flex-1 text-2xl font-semibold", title)}
	return core.View{Class: "flex-row items-center gap-3 pt-8", Desktop: mainWindowDrag(), Children: append(children, actions...)}
}

func (g *gui) main(cx *core.Scope) core.Node {
	desktop.SetTitle(cx, "Remote Go")
	if runtime.GOOS == "darwin" {
		desktop.SetTitleBar(cx, desktop.TitleBarHidden, 36)
	}
	g.hideOnClose(cx)
	device := core.NewSignal(cx, "")
	return core.View{Class: "flex-1 flex-row bg-background", Children: []core.Node{
		g.sidebar(cx, device),
		core.View{Class: "flex-1 flex-col", Children: []core.Node{
			g.banners(cx),
			core.Scroll{Style: core.Style{FlexGrow: 1, FlexShrink: 1}, Children: []core.Node{
				core.View{Class: "px-8 pb-8 gap-5", Children: []core.Node{
					core.Show{When: func() bool { return g.page.Get() == "home" }, Then: g.home},
					core.Show{When: func() bool { return g.page.Get() == "devices" }, Then: func(cx *core.Scope) core.Node { return g.devices(cx, device) }},
					core.Show{When: func() bool { return g.page.Get() == "settings" }, Then: g.settings},
				}},
			}},
		}},
		g.passwordDialog(cx),
		g.promptDialog(cx),
		ui.Toasts{Toaster: g.toasts},
	}}
}

func (g *gui) sidebar(cx *core.Scope, device *core.Signal[string]) core.Node {
	nav := func(id, label string, svg func() *core.SVG) core.Node {
		return base.Button{
			Class: "flex-row items-center gap-3 px-3 h-[40px] rounded-md hover:bg-muted",
			StyleFunc: func() core.Style {
				style := core.Style{FocusVisible: ui.FocusRing(cx, -2)}
				if g.page.Get() == id && !(id == "devices" && g.mem().InGroup) {
					style.Background = core.RGBA(0x35aeca, 0.18)
				}
				return style
			},
			Selected: func() bool { return g.page.Get() == id },
			Label:    label, OnPress: func(core.PressEvent) { g.page.Set(id) },
			Children: []core.Node{icon(svg, "w-[18px] h-[18px]"), text("text-sm font-medium", label)},
		}
	}
	return core.View{Class: "bg-[#f4f7f8] dark:bg-card border-r border-border p-3 gap-1", StyleFunc: func() core.Style {
		width := core.Points(168)
		if g.page.Get() == "devices" {
			width = 224
		}
		return core.Style{Width: core.Pt(width)}
	}, Desktop: mainWindowDrag(), Children: []core.Node{
		core.View{Class: "flex-row items-center gap-1 px-1 pt-3 pb-6", StyleFunc: func() core.Style {
			return core.Style{Margin: core.Edges{Top: desktop.TitleBar(cx).Top}}
		}, Children: []core.Node{
			assets.Logo(),
			text("text-base font-semibold", "Remote Go"),
		}},
		nav("home", "远程控制", lucide.Monitor),
		nav("devices", "我的设备", lucide.MonitorSmartphone),
		core.Show{When: func() bool { return g.page.Get() == "devices" && g.mem().InGroup }, Then: func(cx *core.Scope) core.Node { return g.deviceNavigation(cx, device) }},
		core.View{Class: "flex-1"},
		nav("settings", "设置", lucide.Settings),
		core.View{Class: "flex-row items-center gap-2 px-2 py-3 border-t border-border", Children: []core.Node{
			core.View{ClassFunc: func() string {
				s := g.st.Get()
				switch {
				case s.Net == "error":
					return "w-[8px] h-[8px] rounded-full bg-red-500"
				case s.Net == "ready":
					return "w-[8px] h-[8px] rounded-full bg-green-500"
				}
				return "w-[8px] h-[8px] rounded-full bg-amber-500"
			}},
			textf("text-xs text-muted-foreground shrink", func() string {
				s := g.st.Get()
				switch {
				case s.Net == "error":
					return "中继不可用"
				case s.Net == "ready" && s.Region != "":
					return s.Region + "中继"
				case s.Net == "ready":
					return "中继已就绪"
				}
				return "正在配置中继…"
			}),
		}},
	}}
}

func (g *gui) banners(cx *core.Scope) core.Node {
	return core.View{Children: []core.Node{
		core.Show{When: func() bool { s := g.st.Get(); return s.Net == "error" || s.HostError != "" }, Then: func(cx *core.Scope) core.Node {
			return core.View{Class: "px-7 py-3 bg-red-500/10 border-b border-border", Children: []core.Node{
				textf("text-xs text-red-600", func() string {
					s := g.st.Get()
					if s.NetError != "" {
						return s.NetError
					}
					return "被控服务启动失败：" + s.HostError
				}),
			}}
		}},
		core.Show{When: func() bool { return len(g.st.Get().Viewers) > 0 }, Then: func(cx *core.Scope) core.Node {
			return core.View{Class: "flex-row items-center gap-4 px-7 py-3 bg-amber-500/15 border-b border-border", Children: []core.Node{
				core.View{Class: "flex-1 shrink gap-1", Children: []core.Node{
					textf("text-sm font-semibold", func() string {
						var who []string
						for _, v := range g.st.Get().Viewers {
							n := v.Node
							if v.Login != "" {
								n += "（" + v.Login + "）"
							}
							who = append(who, n)
						}
						return strings.Join(who, "、") + " 正在远程控制本机"
					}),
					text("text-xs text-muted-foreground", "对方可以看到并操作你的屏幕"),
				}},
				ui.Button{Text: "断开", Variant: ui.ButtonOutline, OnPress: func() { go g.be.KickViewers() }},
			}}
		}},
	}}
}

// ------------------------------------------------------------------ home

// connect checks a typed device code and asks for its verification code.
func (g *gui) connect(code string) {
	code = tunnel.Normalize(code)
	switch {
	case code == "":
		g.toast(errors.New("请输入设备代码"))
	case !tunnel.Valid(code):
		g.toast(errors.New("设备代码应为 9 位数字"))
	case code == g.st.GetUntracked().DeviceCode:
		g.toast(errors.New("不能连接本机"))
	default:
		g.pending.Set(code)
	}
}

func (g *gui) passwordDialog(cx *core.Scope) core.Node {
	pw := core.NewSignal(cx, "")
	errText := core.NewSignal(cx, "")
	closeDialog := func() {
		g.pending.Set("")
		errText.Set("")
		pw.Set("")
	}
	submit := func() {
		code := g.pending.Get()
		if code == "" || g.busy.Get() {
			return
		}
		if strings.TrimSpace(pw.Get()) == "" {
			errText.Set("请输入验证码")
			return
		}
		g.busy.Set(true)
		errText.Set("")
		g.openSession(code, pw.Get(), func(err error) {
			g.busy.Set(false)
			if err != nil {
				errText.Set(err.Error())
				return
			}
			closeDialog()
		})
	}
	return ui.Dialog{
		Open: func() bool { return g.pending.Get() != "" },
		OnOpenChange: func(open bool, _ base.Reason) {
			if !open && !g.busy.Get() {
				closeDialog()
			}
		},
		Title: "输入验证码",
		DescriptionFunc: func() string {
			if c := g.pending.Get(); c != "" {
				return "正在连接 " + groupCode(c) + "，请输入对方「本机设备」中显示的验证码。"
			}
			return ""
		},
		OnConfirm: submit,
		Children: []core.Node{
			ui.Input{Placeholder: "对方设备上显示的验证码", Value: pw.Get, OnChange: pw.Set, AutoFocus: true, Masked: true, Label: "验证码",
				OnSubmit: func(string) { submit() }, Style: core.Style{Height: core.Pt(40), FontSize: 16}},
			core.Show{When: func() bool { return g.busy.Get() }, Then: func(cx *core.Scope) core.Node {
				return text("text-sm text-muted-foreground", "正在建立隧道…")
			}},
			core.Show{When: func() bool { return errText.Get() != "" }, Then: func(cx *core.Scope) core.Node {
				return textf("text-sm text-red-600", errText.Get)
			}},
		},
		Footer: []core.Node{
			ui.Button{Text: "取消", Variant: ui.ButtonOutline, Disabled: g.busy.Get, OnPress: closeDialog},
			assets.ConnectButton{Text: "连接", Busy: g.busy.Get, OnPress: submit},
		},
	}
}

// ------------------------------------------------------------------ settings

var qualities = []ui.Option[int]{{Value: 40, Label: "流畅"}, {Value: 70, Label: "标准"}, {Value: 90, Label: "高清"}}
var rates = []ui.Option[int]{{Value: 15, Label: "15 帧"}, {Value: 30, Label: "30 帧"}, {Value: 60, Label: "60 帧"}}
var widths = []ui.Option[int]{{Value: 1280, Label: "1280"}, {Value: 1920, Label: "1920"}, {Value: 2560, Label: "2560"}, {Value: 3840, Label: "原始"}}
