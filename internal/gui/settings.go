package gui

import (
	"strings"

	"github.com/nsgo-dev/nsgo/base"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/ui"
	"github.com/nsgo-dev/nsgo/updater"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/tunnel"
)

func settingRow(title string, detail core.Node, control core.Node) core.Node {
	return core.View{Class: "flex-row items-center gap-4 pt-3 border-t border-border", Children: []core.Node{
		core.View{Class: "flex-1 shrink gap-1", Children: []core.Node{text("text-sm font-semibold", title), detail}},
		control,
	}}
}

const settingHint = "text-xs text-muted-foreground"

func (g *gui) settings(cx *core.Scope) core.Node {
	tab := core.NewSignal(cx, "general")
	return core.View{Class: "gap-4 max-w-[960px]", Children: []core.Node{
		pageHeader("设置"),
		ui.Tabs[string]{Label: "设置分类", Value: tab.Get, OnChange: tab.Set, Options: []ui.Option[string]{
			{Value: "general", Label: "常规"}, {Value: "security", Label: "安全"}, {Value: "network", Label: "网络"},
		}},
		core.Show{When: func() bool { return tab.Get() == "general" }, Then: g.generalSettings},
		core.Show{When: func() bool { return tab.Get() == "security" }, Then: g.securitySettings},
		core.Show{When: func() bool { return tab.Get() == "network" }, Then: g.networkSettings},
	}}
}

func (g *gui) generalSettings(cx *core.Scope) core.Node {
	name := core.NewSignal(cx, g.st.GetUntracked().Name)
	saving := core.NewSignal(cx, false)
	return section(
		settingRow("设备名称", text(settingHint, "显示在其他设备的连接列表中"), core.View{Class: "flex-row gap-2", Children: []core.Node{
			ui.Input{Value: name.Get, OnChange: name.Set, Label: "设备名称", Style: core.Style{Width: core.Pt(200)}},
			ui.Button{Text: "保存", Variant: ui.ButtonOutline, Busy: saving.Get, Disabled: func() bool {
				n := strings.TrimSpace(name.Get())
				return n == "" || n == g.st.Get().Name
			}, OnPress: func() {
				n := strings.TrimSpace(name.Get())
				saving.Set(true)
				g.async(func() error { return g.be.SetName(n) }, func(err error) {
					saving.Set(false)
					if err != nil {
						g.toast(err)
						return
					}
					g.notice("设备名称已保存")
				})
			}},
		}}),
		settingRow("默认画质与帧率", text(settingHint, "自动保存，对下次连接生效"), core.View{Class: "flex-row gap-2", Children: []core.Node{
			ui.Select[int]{Options: qualities, Style: core.Style{Width: core.Pt(112)}, Label: "默认画质", Value: func() int { return g.st.Get().Quality },
				OnChange: func(q int) { f := g.st.Get().FPS; go g.be.SetDefaults(q, f) }},
			ui.Select[int]{Options: rates, Style: core.Style{Width: core.Pt(112)}, Label: "默认帧率", Value: func() int { return g.st.Get().FPS },
				OnChange: func(f int) { q := g.st.Get().Quality; go g.be.SetDefaults(q, f) }},
		}}),
		settingRow("软件更新", text(settingHint, versionLabel()), ui.Button{Text: "检查更新", Variant: ui.ButtonOutline,
			Disabled: func() bool { return core.App().Development }, OnPress: func() { updater.Check(g.app) }}),
	)
}

func versionLabel() string {
	if version := core.App().Version; version != "" {
		return "当前版本 " + version
	}
	return "开发版本"
}

func (g *gui) securitySettings(cx *core.Scope) core.Node {
	open := core.NewSignal(cx, false)
	return section(
		settingRow("允许远程控制", text(settingHint, "关闭后，其他设备无法连接本机"), g.allowControlSwitch()),
		settingRow("连接本机的验证方式", textf(settingHint, func() string {
			if g.st.Get().FixedPassword {
				return "自定义验证码"
			}
			return "临时验证码 · 每次启动时生成"
		}), ui.Button{Text: "更改", Variant: ui.ButtonOutline, OnPress: func() { open.Set(true) }}),
		core.Show{When: open.Get, Then: func(cx *core.Scope) core.Node { return g.verificationEditor(cx, open) }},
	)
}

func (g *gui) verificationEditor(cx *core.Scope, open *core.Signal[bool]) core.Node {
	custom := core.NewSignal(cx, g.st.GetUntracked().FixedPassword)
	code := core.NewSignal(cx, "")
	busy := core.NewSignal(cx, false)
	problem := core.NewSignal(cx, "")
	noChange := func() bool { return !custom.Get() && !g.st.Get().FixedPassword }
	save := func() {
		if busy.Get() || noChange() {
			return
		}
		value := ""
		if custom.Get() {
			value = strings.TrimSpace(code.Get())
			if len(value) < 8 {
				problem.Set("请输入至少 8 位的自定义验证码")
				return
			}
		}
		busy.Set(true)
		g.async(func() error { return g.be.SetFixedPassword(value) }, func(err error) {
			busy.Set(false)
			if err != nil {
				problem.Set(err.Error())
				return
			}
			open.Set(false)
			g.notice("验证码已更新")
		})
	}
	return ui.Dialog{Open: open.Get, Title: "更改验证方式", OnOpenChange: func(v bool, _ base.Reason) {
		if !busy.Get() {
			open.Set(v)
		}
	},
		Children: []core.Node{
			ui.Tabs[bool]{Label: "验证方式", Options: []ui.Option[bool]{
				{Value: false, Label: "临时验证码", Disabled: busy.Get},
				{Value: true, Label: "自定义验证码", Disabled: busy.Get},
			}, Value: custom.Get, OnChange: func(v bool) { custom.Set(v); problem.Set("") }},
			core.Show{When: custom.Get, Then: func(*core.Scope) core.Node {
				return ui.Input{Label: "新验证码", Placeholder: "输入新的验证码，至少 8 位", Masked: true, NoAutoCorrect: true, Value: code.Get, OnChange: func(v string) { code.Set(v); problem.Set("") }, Disabled: busy.Get, OnSubmit: func(string) { save() }}
			}},
			text(settingHint, "更改后旧码立即失效，通过设备代码连入本机的会话会断开。"),
			core.Show{When: func() bool { return problem.Get() != "" }, Then: func(*core.Scope) core.Node { return textf("text-sm text-red-600", problem.Get) }},
		}, Footer: []core.Node{
			ui.Button{Text: "取消", Variant: ui.ButtonOutline, Disabled: busy.Get, OnPress: func() { open.Set(false) }},
			ui.Button{Text: "保存", Variant: ui.ButtonOutline, Busy: busy.Get, Disabled: noChange, OnPress: save},
		},
	}
}

func (g *gui) networkSettings(cx *core.Scope) core.Node {
	state := g.st.GetUntracked()
	region := core.NewSignal(cx, state.RelayRegion)
	address := core.NewSignal(cx, state.RelayAddress)
	busy := core.NewSignal(cx, false)
	options := []ui.Option[int]{{Value: 0, Label: "自动选择"}}
	for _, id := range []int{301, 302, 303, 304, tunnel.CustomRegion} {
		options = append(options, ui.Option[int]{Value: id, Label: backend.RelayName(id)})
	}
	changed := func() bool {
		s := g.st.Get()
		normalized, err := tunnel.NormalizeRelay(address.Get())
		return region.Get() != s.RelayRegion || err != nil || normalized != s.RelayAddress
	}
	return section(
		settingRow("本机引导中继", textf(settingHint, func() string {
			s := g.st.Get()
			if s.Net == "ready" && s.Region != "" {
				return "当前使用" + s.Region + "中继"
			}
			if s.Net == "error" {
				return "中继不可用"
			}
			return "自动选择会测试公共中继的延迟"
		}), ui.Select[int]{Label: "本机引导中继", Options: options, Value: region.Get, OnChange: region.Set, Disabled: busy.Get, Style: core.Style{Width: core.Pt(170)}}),
		settingRow("自建 DERP 地址", text(settingHint, "本机使用自建中继，或连接代码末位为 0 的设备时填写。两端地址须一致。"), ui.Input{Label: "自建 DERP 地址", Placeholder: "derp.example.com:443", NoAutoCorrect: true, Value: address.Get, OnChange: address.Set, Disabled: busy.Get, Style: core.Style{Width: core.Pt(210)}}),
		core.View{Class: "flex-row items-center gap-4 pt-3", Children: []core.Node{
			core.View{Class: "flex-1 shrink", Children: []core.Node{
				core.Show{When: func() bool {
					return changed() && (region.Get() != g.st.Get().RelayRegion || region.Get() == tunnel.CustomRegion)
				}, Then: func(*core.Scope) core.Node {
					return text(settingHint, "应用后会断开通过设备代码连入的会话，设备代码末位可能变化。")
				}},
			}},
			ui.Button{Text: "应用中继设置", Variant: ui.ButtonOutline, Disabled: func() bool { return !changed() }, Busy: busy.Get, OnPress: func() {
				id, endpoint := region.Get(), address.Get()
				busy.Set(true)
				g.async(func() error { return g.be.SetRelay(id, endpoint) }, func(err error) {
					busy.Set(false)
					if err != nil {
						g.toast(err)
						return
					}
					g.notice("中继设置已保存")
				})
			}},
		}},
	)
}
