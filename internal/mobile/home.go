package mobile

import (
	"errors"
	"strings"

	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
	"github.com/nsgo-dev/nsgo/updater"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/tunnel"
)

var (
	qualities = []ui.Option[int]{{Value: 40, Label: "流畅"}, {Value: 70, Label: "标准"}, {Value: 90, Label: "高清"}}
	rates     = []ui.Option[int]{{Value: 15, Label: "15 帧"}, {Value: 30, Label: "30 帧"}, {Value: 60, Label: "60 帧"}}
	modes     = []ui.Option[string]{{Value: "touch", Label: "触控模式"}, {Value: "pad", Label: "触摸板模式"}}
)

func text(class, s string) core.Node                { return core.Text{Class: class, Content: s} }
func textf(class string, f func() string) core.Node { return core.Text{Class: class, ContentFunc: f} }
func icon(svg func() *core.SVG, class string) core.Node {
	return core.Icon{SVG: svg(), Class: class}
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

func (a *app) home(cx *core.Scope) core.Node {
	code := core.NewSignal(cx, "")
	pw := core.NewSignal(cx, "")
	busy := core.NewSignal(cx, false)
	errText := core.NewSignal(cx, "")
	relay := core.NewSignal(cx, a.settings.GetUntracked().RelayAddress)
	pwRef := &core.Ref{}

	connect := func() {
		if busy.Get() {
			return
		}
		c := tunnel.Normalize(code.Get())
		switch {
		case !tunnel.Valid(c):
			errText.Set("设备代码应为 9 位数字")
			return
		case strings.TrimSpace(pw.Get()) == "":
			errText.Set("请输入验证码")
			pwRef.Focus()
			return
		}
		errText.Set("")
		busy.Set(true)
		a.open(c, pw.Get(), func(err error) {
			busy.Set(false)
			if err != nil {
				errText.Set(err.Error())
				return
			}
			pw.Set("")
		})
	}

	field := func(label string, n core.Node) core.Node {
		return core.View{Class: "gap-1.5", Children: []core.Node{text("text-sm text-muted-foreground", label), n}}
	}
	inputStyle := core.Style{Height: core.Pt(46), FontSize: 18}

	// The safe area pads a view outside the scroll: inside it, the
	// content's env() terms do not apply.
	safe := core.Style{FlexGrow: 1, PaddingEnv: core.EnvEdges{Top: core.EnvSafeArea, Bottom: core.EnvSafeArea}}
	return core.View{Style: safe, Children: []core.Node{core.Scroll{Style: core.Style{FlexGrow: 1}, Children: []core.Node{
		core.View{Class: "px-5 pb-8 gap-5", Children: []core.Node{
			// Header
			core.View{Class: "flex-row items-center gap-3 pt-6", Children: []core.Node{
				core.View{Class: "w-[44px] h-[44px] rounded-xl bg-primary items-center justify-center", Children: []core.Node{
					icon(lucide.MonitorSmartphone, "w-[24px] h-[24px] text-primary-foreground"),
				}},
				core.View{Class: "gap-0.5", Children: []core.Node{
					text("text-xl font-semibold", "Remote Go"),
					text("text-xs text-muted-foreground", "远程控制电脑 · 无需账号，点对点加密"),
				}},
			}},

			// Connect card
			core.View{Class: "rounded-2xl bg-card border border-border p-5 gap-4", Children: []core.Node{
				text("text-lg font-semibold", "远程控制设备"),
				field("对方的设备代码", ui.Input{Placeholder: "9 位数字", Value: code.Get, OnChange: code.Set, Label: "设备代码",
					KeyboardType: core.KeyboardNumber, MaxLength: 11, ReturnKeyType: core.ReturnKeyNext,
					OnSubmit: func(string) { pwRef.Focus() }, Style: inputStyle}),
				field("验证码", ui.Input{Placeholder: "对方设备上显示的验证码", Value: pw.Get, OnChange: pw.Set, Label: "验证码",
					Ref: pwRef, NoAutoCorrect: true, AutoCapitalize: core.AutoCapitalizeNone, ReturnKeyType: core.ReturnKeyGo,
					OnSubmit: func(string) { connect() }, Style: inputStyle}),
				core.Show{When: func() bool { return errText.Get() != "" }, Then: func(*core.Scope) core.Node {
					return textf("text-sm text-red-500", errText.Get)
				}},
				ui.Button{Text: "连接", Size: ui.SizeLarge, Busy: busy.Get, OnPress: connect, Style: core.Style{Height: core.Pt(46)}},
				core.Show{When: busy.Get, Then: func(*core.Scope) core.Node {
					return textf("text-xs text-muted-foreground text-center", func() string {
						c := tunnel.Normalize(code.Get())
						return "正在经" + backend.RegionName(c) + "中继建立加密隧道，能打洞时自动切换为直连…"
					})
				}},
			}},

			// Recent devices
			core.Show{When: func() bool { return len(a.recent.Get()) > 0 }, Then: func(cx *core.Scope) core.Node {
				return core.View{Class: "gap-2", Children: []core.Node{
					text("text-sm text-muted-foreground px-1", "最近连接"),
					core.View{Class: "rounded-2xl bg-card border border-border overflow-hidden", Children: []core.Node{
						core.Each(a.recent.Get, func(r Recent) string { return r.Code }, func(cx *core.Scope, r func() Recent, _ func() int) core.Node {
							return core.View{Label: "连接", OnPress: func(core.PressEvent) {
								code.Set(r().Code)
								errText.Set("")
								pwRef.Focus()
							}, Class: "flex-row items-center gap-3 px-4 py-3 border-b border-border", Children: []core.Node{
								icon(lucide.Monitor, "w-[22px] h-[22px] text-muted-foreground"),
								core.View{Class: "flex-1 gap-0.5", Children: []core.Node{
									textf("text-base font-medium", func() string { return r().Name }),
									textf("text-xs text-muted-foreground", func() string {
										s := groupCode(r().Code)
										if o := osName(r().OS); o != "" {
											s += " · " + o
										}
										return s
									}),
								}},
								ui.IconButton{Icon: lucide.X, Label: "移除", OnPress: func() { a.forget(r().Code) }},
							}}
						}),
					}},
				}}
			}},

			// Defaults
			core.View{Class: "gap-2", Children: []core.Node{
				text("text-sm text-muted-foreground px-1", "默认设置"),
				core.View{Class: "rounded-2xl bg-card border border-border px-4 py-1", Children: []core.Node{
					a.settingRow("画质", ui.Select[int]{Options: qualities, Label: "画质", Value: func() int { return a.settings.Get().Quality },
						OnChange: func(v int) { st := a.settings.Get(); st.Quality = v; a.saveSettings(st) }, Style: core.Style{Width: core.Pt(130)}}),
					a.settingRow("帧率", ui.Select[int]{Options: rates, Label: "帧率", Value: func() int { return a.settings.Get().FPS },
						OnChange: func(v int) { st := a.settings.Get(); st.FPS = v; a.saveSettings(st) }, Style: core.Style{Width: core.Pt(130)}}),
					a.settingRow("当前版本 "+core.App().Version, ui.Button{Text: "检查更新", Variant: ui.ButtonOutline, Disabled: func() bool { return core.App().Development }, OnPress: func() { updater.Check(a.cx) }}),
					a.settingRow("操作方式", ui.Select[string]{Options: modes, Label: "操作方式", Value: func() string { return a.settings.Get().Mode },
						OnChange: func(v string) { st := a.settings.Get(); st.Mode = v; a.saveSettings(st) }, Style: core.Style{Width: core.Pt(130)}}),
				}},
			}},

			core.View{Class: "gap-2", Children: []core.Node{
				field("自建 DERP 地址", ui.Input{Value: relay.Get, OnChange: relay.Set, Label: "自建 DERP 地址", Placeholder: "derp.example.com:443",
					NoAutoCorrect: true, AutoCapitalize: core.AutoCapitalizeNone}),
				text("text-xs text-muted-foreground", "连接末位为 0 的设备时，填写与被控端相同的地址。"),
				ui.Button{Text: "保存中继地址", Variant: ui.ButtonOutline, OnPress: func() {
					endpoint, err := tunnel.NormalizeRelay(relay.GetUntracked())
					if err != nil {
						a.toast(err)
						return
					}
					st := a.settings.GetUntracked()
					st.RelayAddress = endpoint
					if err := a.saveSettings(st); err != nil {
						a.toast(err)
						return
					}
					relay.Set(endpoint)
					a.notice("中继地址已保存")
				}},
			}},
			text("text-xs text-muted-foreground px-1 leading-5",
				"在电脑上打开 Remote Go 桌面版，把「设备代码」和「验证码」填到这里即可连接。手机端只做控制端。"+
					"连接经 tailcat（WireGuard）建立，先走中继，能打洞时自动切换为点对点直连；没有账号，也没有我们的服务器。"),
		}},
	}}}}
}

func (a *app) settingRow(label string, control core.Node) core.Node {
	return core.View{Class: "flex-row items-center justify-between py-2", Children: []core.Node{
		text("text-base", label), control,
	}}
}

var errNoDecoder = errors.New("此设备的 WebView 不支持 WebCodecs 硬件解码")
