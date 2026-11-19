package gui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nsgo-dev/nsgo/base"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/desktop"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
	"github.com/nsgo-dev/nsgo/updater"

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

func card(children ...core.Node) core.Node {
	return core.View{Class: "bg-card border border-border rounded-xl p-5 gap-4", Children: children}
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

func (g *gui) main(cx *core.Scope) core.Node {
	desktop.SetTitle(cx, "Remote Go")
	g.hideOnClose(cx)
	return core.View{Class: "flex-1 flex-row bg-background", Children: []core.Node{
		g.sidebar(cx),
		core.View{Class: "flex-1 flex-col", Children: []core.Node{
			g.banners(cx),
			core.Scroll{Style: core.Style{FlexGrow: 1, FlexShrink: 1}, Children: []core.Node{
				core.View{Class: "p-7 gap-5", Children: []core.Node{
					core.Show{When: func() bool { return g.page.Get() == "home" }, Then: g.home},
					core.Show{When: func() bool { return g.page.Get() == "settings" }, Then: g.settings},
				}},
			}},
		}},
		g.passwordDialog(cx),
		ui.Toasts{Toaster: g.toasts},
	}}
}

func (g *gui) sidebar(cx *core.Scope) core.Node {
	nav := func(id, label string, svg func() *core.SVG) core.Node {
		return core.View{
			ClassFunc: func() string {
				c := "flex-row items-center gap-3 px-3 h-[36px] rounded-lg"
				if g.page.Get() == id {
					return c + " bg-accent text-accent-foreground"
				}
				return c + " text-muted-foreground hover:bg-muted"
			},
			Role: core.RoleButton, Label: label,
			OnPress:  func(core.PressEvent) { g.page.Set(id) },
			Children: []core.Node{icon(svg, "w-[18px] h-[18px]"), text("text-sm font-medium", label)},
		}
	}
	return core.View{Class: "w-[200px] bg-muted border-r border-border p-3 gap-1", Children: []core.Node{
		core.View{Class: "flex-row items-center gap-2 px-2 pt-2 pb-5", Children: []core.Node{
			core.View{Class: "w-[30px] h-[30px] rounded-lg bg-primary items-center justify-center", Children: []core.Node{
				icon(lucide.MonitorSmartphone, "w-[18px] h-[18px] text-primary-foreground"),
			}},
			text("text-base font-semibold", "Remote Go"),
		}},
		nav("home", "远程控制", lucide.Monitor),
		nav("settings", "设置", lucide.Settings),
		core.View{Class: "flex-1"},
		core.View{Class: "flex-row items-center gap-2 px-2 py-2", Children: []core.Node{
			core.View{ClassFunc: func() string {
				s := g.st.Get()
				switch {
				case s.Net == "error" || s.HostError != "":
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
					return "tailcat · " + s.Region + "中继"
				case s.Net == "ready":
					return "tailcat 已就绪"
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

func (g *gui) home(cx *core.Scope) core.Node {
	showPw := core.NewSignal(cx, false)
	code := core.NewSignal(cx, "")
	return core.View{Class: "flex-row gap-5 items-start", Children: []core.Node{
		core.View{Class: "flex-1 shrink", Children: []core.Node{card(
			core.View{Class: "flex-row items-center", Children: []core.Node{
				text("flex-1 text-base font-semibold", "本机设备"),
				text("text-xs text-muted-foreground mr-2", "允许远程控制"),
				ui.Switch{Label: "允许远程控制", Checked: func() bool { return g.st.Get().AllowControl }, OnChange: func(on bool) {
					go g.be.SetAllowControl(on)
				}},
			}},
			core.View{Class: "gap-1", Children: []core.Node{
				text("text-xs text-muted-foreground", "设备代码"),
				core.View{Class: "flex-row items-center gap-2", Children: []core.Node{
					textf("text-3xl font-semibold tracking-wider", func() string {
						if c := g.st.Get().DeviceCode; c != "" {
							return groupCode(c)
						}
						return "分配中…"
					}),
					ui.IconButton{Icon: lucide.Copy, Label: "复制设备代码", OnPress: func() {
						cx.SetClipboard(g.st.GetUntracked().DeviceCode)
						g.notice("设备代码已复制")
					}},
				}},
			}},
			core.View{Class: "gap-1", Children: []core.Node{
				textf("text-xs text-muted-foreground", func() string {
					if g.st.Get().FixedPassword {
						return "验证码（固定）"
					}
					return "验证码（每次启动随机生成）"
				}),
				core.View{Class: "flex-row items-center gap-2", Children: []core.Node{
					textf("text-3xl font-semibold tracking-wider", func() string {
						if showPw.Get() {
							return g.st.Get().Password
						}
						return "••••••••"
					}),
					ui.IconButton{Icon: lucide.Eye, Label: "显示/隐藏验证码", Selected: showPw.Get, OnPress: func() { showPw.Set(!showPw.Get()) }},
					ui.IconButton{Icon: lucide.RefreshCw, Label: "更换验证码", OnPress: func() {
						go g.be.RefreshPassword()
						showPw.Set(true)
					}},
				}},
			}},
			textf("text-xs text-muted-foreground", func() string {
				s := g.st.Get()
				switch {
				case s.HostError != "":
					return "被控服务启动失败：" + s.HostError
				case !s.AllowControl:
					return "已关闭远程控制，其他设备无法连接本机。"
				case s.Net != "ready" || (!s.Hosting && s.HostError == ""):
					return "正在启动被控服务…"
				case !s.CanControl:
					return "本机缺少辅助功能 / 屏幕录制权限，对方只能观看。请在系统设置 › 隐私与安全性中授权后重启应用。"
				}
				return "把设备代码和验证码告诉对方，即可远程控制本机。更换验证码后，旧的验证码立即失效。"
			}),
		)}},
		core.View{Class: "flex-1 shrink", Children: []core.Node{card(
			text("text-base font-semibold", "远程控制设备"),
			core.View{Class: "flex-row gap-2", Children: []core.Node{
				ui.Input{Placeholder: "输入对方的设备代码", Value: code.Get, OnChange: code.Set, Label: "对方的设备代码",
					OnSubmit: func(string) { g.connect(code.Get()) },
					Style:    core.Style{FlexGrow: 1, FlexShrink: 1, Height: core.Pt(40), FontSize: 16}},
				ui.Button{Text: "连接", Style: core.Style{Height: core.Pt(40), Width: core.Pt(88)},
					OnPress: func() { g.connect(code.Get()) }},
			}},
			text("text-xs text-muted-foreground mt-1", "最近连接"),
			core.Show{When: func() bool { return len(g.st.Get().Recent) == 0 }, Then: func(cx *core.Scope) core.Node {
				return text("text-sm text-muted-foreground py-3", "还没有连接记录")
			}},
			core.View{Class: "gap-1", Children: []core.Node{
				core.Each(func() []backend.Recent { return g.st.Get().Recent }, func(r backend.Recent) string { return r.Code },
					func(cx *core.Scope, r func() backend.Recent, _ func() int) core.Node {
						return core.View{Class: "flex-row items-center gap-3 px-2 py-2 rounded-lg hover:bg-muted", Role: core.RoleButton,
							Label: "连接 " + r().Name, OnPress: func(core.PressEvent) { g.connect(r().Code) },
							Children: []core.Node{
								icon(lucide.Laptop, "w-[20px] h-[20px] text-muted-foreground"),
								core.View{Class: "flex-1 shrink", Children: []core.Node{
									textf("text-sm font-medium", func() string { return r().Name }),
									textf("text-xs text-muted-foreground", func() string {
										return groupCode(r().Code) + " · " + osName(r().OS)
									}),
								}},
								ui.IconButton{Icon: lucide.X, Label: "删除记录", OnPress: func() { go g.be.ForgetRecent(r().Code) }},
							}}
					}),
			}},
		)}},
	}}
}

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
			pw.Set("")
			g.pending.Set("")
		})
	}
	return ui.Dialog{
		Open: func() bool { return g.pending.Get() != "" },
		OnOpenChange: func(open bool, _ base.Reason) {
			if !open && !g.busy.Get() {
				g.pending.Set("")
				errText.Set("")
				pw.Set("")
			}
		},
		Title: "输入验证码",
		DescriptionFunc: func() string {
			if c := g.pending.Get(); c != "" {
				return fmt.Sprintf("连接 %s（经 %s中继建立加密隧道，能打洞时自动切换为点对点直连）", groupCode(c), backend.RegionName(c))
			}
			return ""
		},
		OnConfirm: submit,
		Children: []core.Node{
			ui.Input{Placeholder: "对方设备上显示的验证码", Value: pw.Get, OnChange: pw.Set, AutoFocus: true, Label: "验证码",
				OnSubmit: func(string) { submit() }, Style: core.Style{Height: core.Pt(40), FontSize: 16}},
			core.Show{When: func() bool { return g.busy.Get() }, Then: func(cx *core.Scope) core.Node {
				return text("text-sm text-muted-foreground", "正在建立隧道…")
			}},
			core.Show{When: func() bool { return errText.Get() != "" }, Then: func(cx *core.Scope) core.Node {
				return textf("text-sm text-red-600", errText.Get)
			}},
		},
		Footer: []core.Node{
			ui.Button{Text: "取消", Variant: ui.ButtonOutline, Disabled: g.busy.Get, OnPress: func() { g.pending.Set("") }},
			ui.Button{Text: "连接", Busy: g.busy.Get, OnPress: submit},
		},
	}
}

// ------------------------------------------------------------------ settings

var qualities = []ui.Option[int]{{Value: 40, Label: "流畅"}, {Value: 70, Label: "标准"}, {Value: 90, Label: "高清"}}
var rates = []ui.Option[int]{{Value: 15, Label: "15 帧"}, {Value: 30, Label: "30 帧"}, {Value: 60, Label: "60 帧"}}
var widths = []ui.Option[int]{{Value: 1280, Label: "1280"}, {Value: 1920, Label: "1920"}, {Value: 2560, Label: "2560"}, {Value: 3840, Label: "原始"}}

func (g *gui) settings(cx *core.Scope) core.Node {
	name := core.NewSignal(cx, g.st.GetUntracked().Name)
	fixed := core.NewSignal(cx, "")
	region := core.NewSignal(cx, g.st.GetUntracked().RelayRegion)
	address := core.NewSignal(cx, g.st.GetUntracked().RelayAddress)
	savingRelay := core.NewSignal(cx, false)
	relayOptions := []ui.Option[int]{{Value: 0, Label: "自动选择"}}
	for _, id := range []int{301, 302, 303, 304, tunnel.CustomRegion} {
		relayOptions = append(relayOptions, ui.Option[int]{Value: id, Label: backend.RelayName(id)})
	}
	row := func(title, detail string, control core.Node) core.Node {
		return core.View{Class: "flex-row items-center gap-6 py-4 border-b border-border", Children: []core.Node{
			core.View{Class: "flex-1 shrink gap-1", Children: []core.Node{
				text("text-sm font-semibold", title), text("text-xs text-muted-foreground", detail),
			}},
			control,
		}}
	}
	save := func(fn func() error, ok string) {
		g.async(fn, func(err error) {
			if err != nil {
				g.toast(err)
			} else {
				g.notice(ok)
			}
		})
	}
	return core.View{Class: "gap-2 max-w-[720px]", Children: []core.Node{
		text("text-2xl font-semibold mb-2", "设置"),
		row("软件更新", "当前版本 "+core.App().Version, ui.Button{Text: "检查更新", Variant: ui.ButtonOutline, Disabled: func() bool { return core.App().Development }, OnPress: func() { updater.Check(g.app) }}),
		row("设备名称", "其他设备在列表中看到的名字", core.View{Class: "flex-row gap-2", Children: []core.Node{
			ui.Input{Value: name.Get, OnChange: name.Set, Label: "设备名称", Style: core.Style{Width: core.Pt(200)}},
			ui.Button{Text: "保存", Variant: ui.ButtonOutline, OnPress: func() { n := name.Get(); save(func() error { return g.be.SetName(n) }, "已保存") }},
		}}),
		row("固定验证码", "留空则每次启动随机生成", core.View{Class: "flex-row gap-2", Children: []core.Node{
			ui.Input{Value: fixed.Get, OnChange: fixed.Set, Masked: true, Placeholder: "至少 8 位", Label: "固定验证码", Style: core.Style{Width: core.Pt(200)}},
			ui.Button{Text: "保存", Variant: ui.ButtonOutline, OnPress: func() {
				p := fixed.Get()
				save(func() error { return g.be.SetFixedPassword(p) }, "验证码已更新")
				fixed.Set("")
			}},
		}}),
		row("默认画质", "新建远程会话时使用", core.View{Class: "flex-row gap-2", Children: []core.Node{
			ui.Select[int]{Options: qualities, Label: "默认画质", Value: func() int { return g.st.Get().Quality },
				OnChange: func(q int) { f := g.st.Get().FPS; go g.be.SetDefaults(q, f) }},
			ui.Select[int]{Options: rates, Label: "默认帧率", Value: func() int { return g.st.Get().FPS },
				OnChange: func(f int) { q := g.st.Get().Quality; go g.be.SetDefaults(q, f) }},
		}}),
		row("本机引导中继", "自动测速、指定公共区域，或使用自建 DERP。切换会断开本机的入站会话，并更新设备代码的末位。", ui.Select[int]{
			Options: relayOptions, Label: "本机引导中继", Value: region.Get, OnChange: region.Set, Style: core.Style{Width: core.Pt(170)},
		}),
		row("自建 DERP 地址", "连接末位为 0 的设备时，两端需填写同一地址。默认 TLS 端口为 443。", core.View{Class: "flex-row gap-2", Children: []core.Node{
			ui.Input{Value: address.Get, OnChange: address.Set, Label: "自建 DERP 地址", Placeholder: "derp.example.com:443", NoAutoCorrect: true, Style: core.Style{Width: core.Pt(210)}},
			ui.Button{Text: "应用", Busy: savingRelay.Get, OnPress: func() {
				id, endpoint := region.GetUntracked(), address.GetUntracked()
				savingRelay.Set(true)
				g.async(func() error { return g.be.SetRelay(id, endpoint) }, func(err error) {
					savingRelay.Set(false)
					if err != nil {
						g.toast(err)
					} else {
						g.notice("中继设置已保存")
					}
				})
			}},
		}}),
		core.View{Class: "gap-1 py-4", Children: []core.Node{
			text("text-sm font-semibold", "工作原理"),
			text("text-xs text-muted-foreground", "设备代码的最后一位表示本机使用的中继，0 代表自建 DERP。被控端用「设备代码 + 验证码」经 Argon2id 推导出 WireGuard 密钥和预共享密钥，控制端输入同样的两个码就能算出同一个隧道地址；没有任何服务器保存或撮合设备。隧道先经 DERP 中继建立，随后自动打洞切换为点对点直连。画面在被控端用 GPU 硬件 H.264 编码，控制端硬件解码并用原生视图显示。"),
		}},
	}}
}
