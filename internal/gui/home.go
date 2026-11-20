package gui

import (
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"

	"github.com/i5shuyi/remote-go/internal/backend"
)

var brandBlue = ui.Fixed(core.RGB(0x405bea))

func (g *gui) home(cx *core.Scope) core.Node {
	code := core.NewSignal(cx, "")
	connect := func() { g.connect(code.Get()) }
	return core.View{Class: "gap-5 max-w-[960px]", Children: []core.Node{
		pageHeader("远程控制"),
		core.View{Class: "flex-row gap-4", Children: []core.Node{
			core.View{Class: "flex-1 shrink rounded-xl bg-[#243465] p-5 gap-4", Children: []core.Node{
				core.View{Class: "flex-row items-center gap-3", Children: []core.Node{
					icon(lucide.MonitorUp, "w-[22px] h-[22px] text-[#b6c9ff]"),
					text("text-xl font-semibold text-white", "连接设备"),
				}},
				core.View{Class: "flex-1"},
				core.View{Class: "gap-2", Children: []core.Node{
					text("text-sm text-[#e0e7ff]", "对方的设备代码"),
					ui.Input{Placeholder: "输入 9 位设备代码", Value: code.Get, OnChange: code.Set, Label: "对方的设备代码",
						OnSubmit: func(string) { connect() }, Style: core.Style{Height: core.Pt(42), FontSize: 16}},
				}},
				ui.Button{Text: "连接", Icon: lucide.ArrowUpRight, Color: brandBlue, Style: core.Style{Height: core.Pt(40), Width: core.Pct(100)}, OnPress: connect},
			}},
			core.View{Class: "flex-1 shrink rounded-xl bg-card border border-border p-5 gap-3", Children: g.deviceDetails(cx)},
		}},
		g.recentDevices(),
	}}
}

func (g *gui) deviceDetails(cx *core.Scope) []core.Node {
	reveal := core.NewSignal(cx, false)
	return []core.Node{
		core.View{Class: "flex-row items-center gap-2", Children: []core.Node{
			icon(lucide.Laptop, "w-[20px] h-[20px] text-muted-foreground"),
			text("text-lg font-semibold", "本机设备"),
		}},
		core.View{Class: "gap-1", Children: []core.Node{
			text("text-xs text-muted-foreground", "设备代码"),
			core.View{Class: "flex-row items-center gap-2", Children: []core.Node{
				textf("text-2xl font-semibold", func() string {
					if c := g.st.Get().DeviceCode; c != "" {
						return groupCode(c)
					}
					return "分配中…"
				}),
				ui.IconButton{Icon: lucide.Copy, Label: "复制设备代码", Disabled: func() bool { return g.st.Get().DeviceCode == "" }, OnPress: func() {
					cx.SetClipboard(g.st.GetUntracked().DeviceCode)
					g.notice("设备代码已复制")
				}},
			}},
		}},
		core.View{Class: "flex-row items-center gap-2", Children: []core.Node{
			textf("text-xs text-muted-foreground", func() string {
				if g.st.Get().FixedPassword {
					return "固定验证码"
				}
				return "验证码"
			}),
			textf("flex-1 shrink text-sm font-semibold", func() string {
				if reveal.Get() {
					return g.st.Get().Password
				}
				return "••••••••"
			}),
			ui.IconButton{Icon: lucide.Eye, LabelFunc: func() string {
				if reveal.Get() {
					return "隐藏验证码"
				}
				return "显示验证码"
			}, Selected: reveal.Get, OnPress: func() { reveal.Set(!reveal.Get()) }},
			ui.IconButton{Icon: lucide.RefreshCw, Label: "更换验证码，旧码立即失效", OnPress: func() { go g.be.RefreshPassword(); reveal.Set(true) }},
		}},
		core.View{Class: "flex-row items-center gap-2 pt-3 border-t border-border", Children: []core.Node{
			core.View{Class: "flex-1 shrink gap-1", Children: []core.Node{
				text("text-sm font-medium", "允许远程控制"),
				textf("text-xs text-muted-foreground", func() string {
					s := g.st.Get()
					switch {
					case !s.AllowControl:
						return "已关闭，其他设备无法连接"
					case s.HostError != "":
						return "被控服务启动失败"
					case s.Net == "error":
						return "中继不可用"
					case s.Net != "ready" || !s.Hosting:
						return "正在启动被控服务…"
					case !s.CanControl:
						return "仅可观看。请在系统设置中授予辅助功能和屏幕录制权限后重启。"
					default:
						return "将设备代码和验证码告知对方"
					}
				}),
			}},
			ui.Switch{Label: "允许远程控制", Checked: func() bool { return g.st.Get().AllowControl }, OnChange: func(on bool) {
				g.async(func() error { return g.be.SetAllowControl(on) }, func(err error) {
					if err != nil {
						g.toast(err)
					}
				})
			}},
		}},
	}
}

func (g *gui) recentDevices() core.Node {
	return card(
		text("text-lg font-semibold", "最近连接"),
		core.Show{When: func() bool { return len(g.st.Get().Recent) == 0 }, Then: func(cx *core.Scope) core.Node {
			return core.View{Class: "flex-row items-center gap-3 bg-muted rounded-lg px-4 py-4", Children: []core.Node{
				icon(lucide.Clock, "w-[20px] h-[20px] text-muted-foreground"),
				text("text-sm text-muted-foreground", "成功连接后，可以在这里快速重连。"),
			}}
		}},
		core.Each(func() []backend.Recent { return g.st.Get().Recent }, func(r backend.Recent) string { return r.Code },
			func(cx *core.Scope, r func() backend.Recent, _ func() int) core.Node {
				return core.View{Class: "flex-row items-center gap-3 pt-3 border-t border-border", Children: []core.Node{
					icon(lucide.Laptop, "w-[20px] h-[20px] text-muted-foreground"),
					core.View{Class: "flex-1 shrink gap-1", Children: []core.Node{
						textf("text-sm font-semibold", func() string { return r().Name }),
						textf("text-xs text-muted-foreground", func() string { return groupCode(r().Code) + " · " + osName(r().OS) }),
					}},
					ui.Button{Text: "重新连接", Variant: ui.ButtonOutline, OnPress: func() { g.connect(r().Code) }},
					ui.IconButton{Icon: lucide.X, Label: "删除连接记录", OnPress: func() { go g.be.ForgetRecent(r().Code) }},
				}}
			}),
	)
}
