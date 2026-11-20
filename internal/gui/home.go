package gui

import (
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"

	"github.com/i5shuyi/remote-go/assets"
	"github.com/i5shuyi/remote-go/internal/backend"
)

func (g *gui) home(cx *core.Scope) core.Node {
	code := core.NewSignal(cx, "")
	connect := func() { g.connect(code.Get()) }
	return core.View{Class: "gap-7 max-w-[880px]", Children: []core.Node{
		pageHeader("远程控制"),
		core.View{Class: "gap-3", Children: []core.Node{
			text("text-base font-semibold", "连接其他设备"),
			core.View{Class: "flex-row items-center gap-3", Children: []core.Node{
				core.View{Class: "flex-1 shrink", Children: []core.Node{
					ui.Input{Placeholder: "输入对方的 9 位设备代码", Label: "对方的设备代码", Value: code.Get, OnChange: code.Set,
						OnSubmit: func(string) { connect() }, Style: core.Style{Height: core.Pt(44), FontSize: 17}},
				}},
				assets.ConnectButton{Text: "连接", Icon: lucide.ArrowUpRight, Style: core.Style{Height: core.Pt(44), Width: core.Pt(96)}, OnPress: connect},
			}},
		}},
		core.View{Class: "gap-4 pt-5 border-t border-border", Children: g.deviceDetails(cx)},
		g.recentDevices(),
	}}
}

func (g *gui) deviceDetails(cx *core.Scope) []core.Node {
	reveal := core.NewSignal(cx, false)
	editorOpen := core.NewSignal(cx, false)
	return []core.Node{
		core.View{Class: "flex-row items-center gap-2", Children: []core.Node{
			text("text-base font-semibold", "本机设备"),
			textf("flex-1 shrink text-sm text-muted-foreground", func() string { return g.st.Get().Name }),
		}},
		core.View{Class: "flex-row items-center bg-muted rounded-lg px-5 py-4 gap-4", Children: []core.Node{
			core.View{Class: "flex-1 gap-1", Children: []core.Node{
				text("text-xs text-muted-foreground", "设备代码"),
				core.View{Class: "flex-row items-center gap-2", Children: []core.Node{
					textf("text-2xl font-semibold font-mono", func() string {
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
			core.View{Class: "flex-1 gap-1 pl-5 border-l border-border", Children: []core.Node{
				textf("text-xs text-muted-foreground", func() string {
					if g.st.Get().FixedPassword {
						return "自定义验证码"
					}
					return "验证码"
				}),
				core.View{Class: "flex-row items-center gap-2", Children: []core.Node{
					textf("shrink text-xl font-medium", func() string {
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
					core.Show{When: func() bool { return g.st.Get().FixedPassword }, Then: func(*core.Scope) core.Node {
						return ui.IconButton{Icon: lucide.Pencil, Label: "修改自定义验证码", OnPress: func() { editorOpen.Set(true) }}
					}, Else: func(*core.Scope) core.Node {
						return ui.IconButton{Icon: lucide.RefreshCw, Label: "更换临时验证码，旧码立即失效", OnPress: func() { go g.be.RefreshPassword(); reveal.Set(true) }}
					}},
				}},
			}},
		}},
		core.View{Class: "flex-row items-center gap-2", Children: []core.Node{
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
			g.allowControlSwitch(),
		}},
		core.Show{When: editorOpen.Get, Then: func(cx *core.Scope) core.Node { return g.verificationEditor(cx, editorOpen) }},
	}
}

func (g *gui) allowControlSwitch() core.Node {
	return ui.Switch{Label: "允许远程控制", Checked: func() bool { return g.st.Get().AllowControl }, OnChange: func(on bool) {
		g.run(func() error { return g.be.SetAllowControl(on) }, "")
	}}
}

func (g *gui) recentDevices() core.Node {
	return core.View{Class: "gap-3 pt-5 border-t border-border", Children: []core.Node{
		text("text-base font-semibold", "最近连接"),
		core.Show{When: func() bool { return len(g.st.Get().Recent) == 0 }, Then: func(cx *core.Scope) core.Node {
			return text("text-sm text-muted-foreground", "暂无连接记录")
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
	}}
}
