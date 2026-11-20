package gui

import (
	"log"
	"strings"

	"github.com/nsgo-dev/nsgo/base"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/members"
	"github.com/i5shuyi/remote-go/internal/tunnel"
)

// membersBackend is implemented by the in-process backend.App and by the
// worker's liveupdate.Panel.
type membersBackend interface {
	PairWith(code, password string) error
	AnswerPrompt(id int, ok, inbound bool) error
	SetMemberPolicy(members.Policy) error
	RemoveMember(id string, rotate bool) error
	RotateMemberKey() error
	SyncMembers()
	LeaveMembers() error
	RebuildMembers() error
}

func (g *gui) mem() backend.MembersDTO { return g.st.Get().Members }

func (g *gui) run(fn func() error, ok string) {
	g.async(fn, func(err error) {
		if err != nil {
			g.toast(err)
		} else if ok != "" {
			g.notice(ok)
		}
	})
}

// openMember starts a "my devices" session; done runs on the UI thread
// once it is up or has failed.
func (g *gui) openMember(id string, done func()) {
	g.notice("正在连接，对方可能需要本人确认…")
	finish := func(err error) {
		done()
		if err != nil {
			log.Printf("member %s: %v", id, err)
			g.toast(err)
		}
	}
	if g.panel != nil {
		g.async(func() error { return g.panel.OpenMember(id) }, finish)
		return
	}
	g.openSessionWith(func(cfg client.Config, h client.Handler) (*backend.Remote, error) {
		return g.local.ConnectMember(id, cfg, h)
	}, finish)
}

func platformName(d backend.DeviceDTO) string {
	switch {
	case d.Self:
		return "本机 · " + osName(d.Platform)
	case !d.Inbound && d.Platform == "android":
		return "Android · 仅控制端"
	case !d.Inbound:
		return osName(d.Platform) + " · 未开启直连"
	}
	return osName(d.Platform)
}

func (g *gui) devices(cx *core.Scope) core.Node {
	pairOpen := core.NewSignal(cx, false)
	removing := core.NewSignal(cx, backend.DeviceDTO{})
	connecting := core.NewSignal(cx, "") // the device being dialed
	policy := func(change func(*members.Policy)) {
		m := g.mem()
		p := members.Policy{AllowInbound: m.AllowInbound, Unattended: m.Unattended, ViewOnly: m.ViewOnly}
		change(&p)
		g.run(func() error { return g.be.SetMemberPolicy(p) }, "")
	}
	row := func(title string, detail func() string, control core.Node) core.Node {
		return core.View{Class: "flex-row items-center gap-4 pt-3 border-t border-border", Children: []core.Node{
			core.View{Class: "flex-1 shrink gap-1", Children: []core.Node{
				text("text-sm font-semibold", title), textf("text-xs text-muted-foreground", detail),
			}},
			control,
		}}
	}
	notReady := func() bool { return !g.mem().Ready }
	inbound := func() bool { return g.mem().AllowInbound }
	return core.View{Class: "gap-4 max-w-[960px]", Children: []core.Node{
		pageHeader("我的设备",
			ui.IconButton{Icon: lucide.RefreshCw, Label: "立即同步", Disabled: notReady, OnPress: func() { go g.be.SyncMembers() }},
			ui.Button{Text: "添加设备", Icon: lucide.Plus, Color: brandBlue, Disabled: notReady, OnPress: func() { pairOpen.Set(true) }},
		),
		core.Show{When: notReady, Then: func(cx *core.Scope) core.Node {
			return card(textf("text-sm text-muted-foreground", func() string {
				if e := g.mem().Error; e != "" {
					return "我的设备不可用：" + e
				}
				return "正在读取成员状态…（若长时间停留，请更新到最新版本后重启）"
			}))
		}},
		core.Show{When: func() bool { return g.mem().Conflict }, Then: func(cx *core.Scope) core.Node {
			return core.View{Class: "flex-row items-center gap-4 rounded-xl bg-red-500/10 p-4", Children: []core.Node{
				icon(lucide.ShieldAlert, "w-[20px] h-[20px] text-red-600"),
				text("flex-1 shrink text-sm text-red-700", "设备组出现两条互相矛盾的修改，已暂停免码连接。请在一台在手设备上重建设备组，再逐一添加要保留的设备；其他设备需要在本机分别处理。"),
				ui.Button{Text: "重建设备组", Variant: ui.ButtonOutline, OnPress: func() { g.run(g.be.RebuildMembers, "已停用旧设备组") }},
			}}
		}},
		core.Show{When: func() bool { p := g.mem().Pairing; return p.Busy || p.Error != "" || p.Result != "" }, Then: func(cx *core.Scope) core.Node {
			return core.View{Class: "flex-row items-center gap-3 rounded-xl bg-muted px-4 py-3", Children: []core.Node{
				icon(lucide.Link, "w-[18px] h-[18px] text-muted-foreground"),
				textf("flex-1 shrink text-sm", func() string {
					p := g.mem().Pairing
					switch {
					case p.Busy:
						return "正在配对，请在两台设备上核对配对码…"
					case p.Error != "":
						return "配对未完成：" + p.Error
					}
					return p.Result
				}),
			}}
		}},
		core.Show{When: func() bool { m := g.mem(); return m.Ready && !m.InGroup && !m.Conflict }, Then: func(cx *core.Scope) core.Node {
			return card(
				text("text-lg font-semibold", "免码连接自己的设备"),
				text("text-sm text-muted-foreground", "先用设备代码和验证码连上自己的另一台设备，确认两边显示的配对码一致后，以后就能直接从这里连接，无需再输入任何代码。"),
				core.View{Class: "flex-row", Children: []core.Node{
					ui.Button{Text: "添加第一台设备", Icon: lucide.UserPlus, Color: brandBlue, OnPress: func() { pairOpen.Set(true) }},
				}},
			)
		}},
		core.Show{When: func() bool { return g.mem().InGroup }, Then: func(cx *core.Scope) core.Node {
			return core.View{Class: "gap-4", Children: []core.Node{
				card(
					core.View{Class: "flex-row items-center gap-2", Children: []core.Node{
						text("flex-1 text-lg font-semibold", "设备"),
						textf("text-xs text-muted-foreground", func() string { return "组指纹 " + g.mem().Group }),
					}},
					core.Each(func() []backend.DeviceDTO { return g.mem().Devices }, func(d backend.DeviceDTO) string { return d.ID },
						func(cx *core.Scope, d func() backend.DeviceDTO, _ func() int) core.Node {
							return core.View{Class: "flex-row items-center gap-3 pt-3 border-t border-border", Children: []core.Node{
								core.Icon{SVG: func() *core.SVG {
									if d().Platform == "android" {
										return lucide.Smartphone()
									}
									return lucide.Laptop()
								}(), Class: "w-[20px] h-[20px] text-muted-foreground"},
								core.View{Class: "flex-1 shrink gap-1", Children: []core.Node{
									textf("text-sm font-semibold", func() string { return d().Name }),
									textf("text-xs text-muted-foreground", func() string {
										s := platformName(d()) + " · " + d().Short
										if !d().Self && !d().Synced {
											s += " · 待同步"
										}
										return s
									}),
								}},
								core.Show{When: func() bool { return !d().Self && d().Inbound }, Then: func(cx *core.Scope) core.Node {
									return ui.Button{Text: "连接", Variant: ui.ButtonOutline,
										Disabled: func() bool { return connecting.Get() == d().ID },
										OnPress: func() {
											id := d().ID
											connecting.Set(id)
											g.openMember(id, func() {
												if connecting.GetUntracked() == id {
													connecting.Set("")
												}
											})
										}}
								}},
								core.Show{When: func() bool { return !d().Self }, Then: func(cx *core.Scope) core.Node {
									return ui.IconButton{Icon: lucide.X, Label: "移除设备", OnPress: func() { removing.Set(d()) }}
								}},
							}}
						}),
				),
				card(text("text-lg font-semibold", "本机访问"),
					row("允许我的设备直连", func() string {
						switch m := g.mem(); {
						case !g.st.Get().AllowControl:
							return "「允许远程控制」已关闭，直连也随之停止"
						case m.AllowInbound && m.Serving:
							return "同组设备可以直接连接本机"
						case m.AllowInbound:
							return "正在启动成员监听…"
						}
						return "关闭时本机只能连接其他设备"
					}, ui.Switch{Label: "允许我的设备直连", Checked: func() bool { return g.mem().AllowInbound },
						OnChange: func(on bool) {
							policy(func(p *members.Policy) { p.AllowInbound = on; p.Unattended = p.Unattended && on })
						}}),
					// Only meaningful while members may dial in.
					core.Show{When: inbound, Then: func(cx *core.Scope) core.Node {
						return row("无人值守", func() string {
							if g.mem().Unattended {
								return "同组设备连接时不再需要本机确认"
							}
							return "默认每次连接都需要在本机点击允许"
						}, ui.Switch{Label: "无人值守", Checked: func() bool { return g.mem().Unattended },
							OnChange: func(on bool) { policy(func(p *members.Policy) { p.Unattended = on }) }})
					}},
					core.Show{When: inbound, Then: func(cx *core.Scope) core.Node {
						return row("仅观看", func() string { return "同组设备只能查看屏幕，键鼠和剪贴板均被禁用" },
							ui.Switch{Label: "仅观看", Checked: func() bool { return g.mem().ViewOnly },
								OnChange: func(on bool) { policy(func(p *members.Policy) { p.ViewOnly = on }) }})
					}},
					row("更换组密钥", func() string { return "旧地址在所有设备签收新密钥前继续提供同步，最长 90 天" },
						ui.Button{Text: "更换", Variant: ui.ButtonOutline, OnPress: func() { g.run(g.be.RotateMemberKey, "已更换组密钥") }}),
					row("退出设备组", func() string { return "通知可达的设备后清除本机的成员身份和密钥" },
						ui.Button{Text: "退出", Variant: ui.ButtonOutline, OnPress: func() { g.run(g.be.LeaveMembers, "已退出设备组") }}),
				),
			}}
		}},
		g.pairDialog(cx, pairOpen),
		g.removeDialog(cx, removing),
	}}
}

func (g *gui) removeDialog(cx *core.Scope, removing *core.Signal[backend.DeviceDTO]) core.Node {
	rotate := core.NewSignal(cx, true)
	closeIt := func() { removing.Set(backend.DeviceDTO{}); rotate.Set(true) }
	return ui.Dialog{
		Open: func() bool { return removing.Get().ID != "" },
		OnOpenChange: func(open bool, _ base.Reason) {
			if !open {
				closeIt()
			}
		},
		TitleFunc:   func() string { return "移除「" + removing.Get().Name + "」？" },
		Description: "移除在本机立即生效，其他设备收到后生效。已交给对方的数据和密钥无法远程收回；设备丢失时请保持「同时更换组密钥」。",
		Children: []core.Node{
			ui.Checkbox{Text: "同时更换组密钥", Checked: rotate.Get, OnChange: rotate.Set},
		},
		Footer: []core.Node{
			ui.Button{Text: "取消", Variant: ui.ButtonOutline, OnPress: closeIt},
			ui.Button{Text: "移除", OnPress: func() {
				id, rot := removing.GetUntracked().ID, rotate.GetUntracked()
				closeIt()
				g.run(func() error { return g.be.RemoveMember(id, rot) }, "已移除，等待其他设备同步")
			}},
		},
	}
}

func (g *gui) pairDialog(cx *core.Scope, open *core.Signal[bool]) core.Node {
	code := core.NewSignal(cx, "")
	pw := core.NewSignal(cx, "")
	errText := core.NewSignal(cx, "")
	closeIt := func() { open.Set(false); code.Set(""); pw.Set(""); errText.Set("") }
	submit := func() {
		c := tunnel.Normalize(code.Get())
		if !tunnel.Valid(c) {
			errText.Set("设备代码应为 9 位数字")
			return
		}
		if strings.TrimSpace(pw.Get()) == "" {
			errText.Set("请输入验证码")
			return
		}
		p := pw.Get()
		g.async(func() error { return g.be.PairWith(c, p) }, func(err error) {
			if err != nil {
				errText.Set(err.Error())
				return
			}
			closeIt()
		})
	}
	return ui.Dialog{
		Open: open.Get, OnOpenChange: func(o bool, _ base.Reason) {
			if !o {
				closeIt()
			}
		},
		Title:       "添加设备",
		Description: "输入要添加的设备上显示的设备代码和验证码。连接后两台设备会显示同一串配对码，请本人核对后分别确认。",
		OnConfirm:   submit,
		Children: []core.Node{
			ui.Input{Placeholder: "9 位设备代码", Value: code.Get, OnChange: code.Set, AutoFocus: true, Label: "设备代码",
				Style: core.Style{Height: core.Pt(40), FontSize: 16}},
			ui.Input{Placeholder: "验证码", Value: pw.Get, OnChange: pw.Set, Masked: true, Label: "验证码",
				OnSubmit: func(string) { submit() }, Style: core.Style{Height: core.Pt(40), FontSize: 16}},
			core.Show{When: func() bool { return errText.Get() != "" }, Then: func(cx *core.Scope) core.Node {
				return textf("text-sm text-red-600", errText.Get)
			}},
		},
		Footer: []core.Node{
			ui.Button{Text: "取消", Variant: ui.ButtonOutline, OnPress: closeIt},
			ui.Button{Text: "连接并配对", Color: brandBlue, OnPress: submit},
		},
	}
}

// promptDialog shows what the worker asks the person at this device. It
// has no default action: Return and Escape never allow anything.
func (g *gui) promptDialog(cx *core.Scope) core.Node {
	inbound := core.NewSignal(cx, false)
	current := func() backend.PromptDTO {
		if ps := g.mem().Prompts; len(ps) > 0 {
			return ps[0]
		}
		return backend.PromptDTO{}
	}
	answer := func(ok bool) {
		p := current()
		if p.ID == 0 {
			return
		}
		in := inbound.GetUntracked()
		inbound.Set(false)
		g.run(func() error { return g.be.AnswerPrompt(p.ID, ok, in) }, "")
	}
	return ui.Dialog{
		Open:                   func() bool { return current().ID != 0 },
		OnOpenChange:           func(open bool, _ base.Reason) {},
		NoCloseOnEscape:        true,
		NoCloseOnBackdropPress: true,
		TitleFunc: func() string {
			p := current()
			if p.Kind == "change" {
				return "确认修改我的设备"
			}
			if p.Kind == "control" {
				return "「" + p.Peer + "」请求控制本机"
			}
			return "核对配对码"
		},
		DescriptionFunc: func() string {
			p := current()
			switch {
			case p.Kind == "change":
				return p.Detail
			case p.Kind == "control":
				return "这是你设备组中的设备（" + p.PeerShort + "）。允许后对方可以看到并操作本机屏幕。30 秒内未允许将自动拒绝。"
			case p.Role == "recover":
				return "「" + p.Peer + "」已在同一设备组中，确认后将补齐成员历史和密钥。请核对对方身份 " + p.PeerShort + "。"
			case p.Role == "joiner":
				return "本机将加入「" + p.Peer + "」所在的设备组。只有两台设备显示完全相同的配对码时才确认。"
			case p.NewGroup:
				return "将新建设备组并添加「" + p.Peer + "」。只有两台设备显示完全相同的配对码时才确认。"
			}
			return "将把「" + p.Peer + "」添加到我的设备。只有两台设备显示完全相同的配对码时才确认。"
		},
		Children: []core.Node{
			core.Show{When: func() bool { return current().Code != "" }, Then: func(cx *core.Scope) core.Node {
				return textf("text-3xl font-semibold text-center py-2", func() string { return current().Code })
			}},
			core.Show{When: func() bool { return current().CanInbound }, Then: func(cx *core.Scope) core.Node {
				return ui.Checkbox{Text: "允许我的设备直连本机", Checked: inbound.Get, OnChange: inbound.Set}
			}},
		},
		Footer: []core.Node{
			core.Show{When: func() bool { return current().Kind == "control" || current().Kind == "change" }, Then: func(cx *core.Scope) core.Node {
				return core.View{Class: "flex-row gap-2", Children: []core.Node{
					ui.Button{Text: "拒绝", Variant: ui.ButtonOutline, OnPress: func() { answer(false) }},
					ui.Button{Text: "允许", Color: brandBlue, OnPress: func() { answer(true) }},
				}}
			}},
			core.Show{When: func() bool { return current().Kind == "pair" }, Then: func(cx *core.Scope) core.Node {
				return core.View{Class: "flex-row gap-2", Children: []core.Node{
					ui.Button{Text: "不一致，取消", Variant: ui.ButtonOutline, OnPress: func() { answer(false) }},
					ui.Button{Text: "一致，确认", Color: brandBlue, OnPress: func() { answer(true) }},
				}}
			}},
		},
	}
}
