package mobile

import (
	"errors"
	"strings"

	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
	"github.com/nsgo-dev/nsgo/updater"

	"github.com/i5shuyi/remote-go/assets"
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
	a.syncMembers() // the home screen appears at launch and after each session
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
			core.View{Class: "flex-row items-center gap-1 pt-5", Children: []core.Node{
				assets.Logo(), text("text-base font-semibold", "Remote Go"),
			}},
			core.View{Class: "gap-4", Children: []core.Node{
				text("text-2xl font-semibold", "连接其他设备"),
				field("对方的设备代码", ui.Input{Placeholder: "9 位数字", Value: code.Get, OnChange: code.Set, Label: "设备代码",
					KeyboardType: core.KeyboardNumber, MaxLength: 11, ReturnKeyType: core.ReturnKeyNext,
					OnSubmit: func(string) { pwRef.Focus() }, Style: inputStyle}),
				field("验证码", ui.Input{Placeholder: "对方设备上显示的验证码", Value: pw.Get, OnChange: pw.Set, Label: "验证码",
					Ref: pwRef, NoAutoCorrect: true, AutoCapitalize: core.AutoCapitalizeNone, ReturnKeyType: core.ReturnKeyGo,
					OnSubmit: func(string) { connect() }, Style: inputStyle}),
				core.Show{When: func() bool { return errText.Get() != "" }, Then: func(*core.Scope) core.Node {
					return textf("text-sm text-red-500", errText.Get)
				}},
				assets.ConnectButton{Text: "连接", Size: ui.SizeLarge, Busy: busy.Get, OnPress: connect, Style: core.Style{Height: core.Pt(46)}},
				core.Show{When: busy.Get, Then: func(*core.Scope) core.Node {
					return textf("text-xs text-muted-foreground text-center", func() string {
						c := tunnel.Normalize(code.Get())
						return "正在通过" + backend.RegionName(c) + "中继连接…"
					})
				}},
			}},

			a.devicesCard(cx, code.GetUntracked, pw.GetUntracked),

			// Recent devices
			core.Show{When: func() bool { return len(a.recent.Get()) > 0 }, Then: func(cx *core.Scope) core.Node {
				return core.View{Class: "gap-2", Children: []core.Node{
					text("text-base font-semibold", "最近连接"),
					core.View{Class: "overflow-hidden border-t border-border", Children: []core.Node{
						core.Each(a.recent.Get, func(r Recent) string { return r.Code }, func(cx *core.Scope, r func() Recent, _ func() int) core.Node {
							return core.View{Label: "连接", OnPress: func(core.PressEvent) {
								code.Set(r().Code)
								errText.Set("")
								pwRef.Focus()
							}, Class: "flex-row items-center gap-3 py-3 border-b border-border", Children: []core.Node{
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
				text("text-base font-semibold", "默认设置"),
				core.View{Class: "border-t border-border", Children: []core.Node{
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
		}},
	}}}}
}

func (a *app) settingRow(label string, control core.Node) core.Node {
	return core.View{Class: "flex-row items-center justify-between py-2", Children: []core.Node{
		text("text-base", label), control,
	}}
}

var errNoDecoder = errors.New("此设备的 WebView 不支持 WebCodecs 硬件解码")
