package gui

import (
	_ "embed"

	"github.com/nsgo-dev/nsgo/base"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"

	"github.com/i5shuyi/remote-go/internal/backend"
)

//go:embed artwork/coast.webp
var desktopCover []byte

//go:embed artwork/computer.webp
var computerArt []byte

//go:embed artwork/phone.webp
var phoneArt []byte

const allDevices = "all"

func canEnterDesktop(d backend.DeviceDTO) bool {
	return d.ID != "" && !d.Self && d.Inbound
}

// A removed selection falls back to another desktop. The overview remains
// selected across membership updates until the user selects a device.
func (g *gui) selectedMember(id string) backend.DeviceDTO {
	m := g.mem()
	if id == allDevices || !m.Ready || !m.InGroup {
		return backend.DeviceDTO{}
	}
	var fallback backend.DeviceDTO
	for _, d := range m.Devices {
		if d.ID == id {
			return d
		}
		if fallback.ID == "" || (!canEnterDesktop(fallback) && canEnterDesktop(d)) {
			fallback = d
		}
	}
	return fallback
}

func deviceArt(phone bool, size core.Points) core.Image {
	data := computerArt
	if phone {
		data = phoneArt
	}
	return core.Image{Source: core.ImageSource{Data: data}, ResizeMode: core.Contain, ElementsHidden: true, Style: core.Style{Height: core.Pt(size), Width: core.Pt(size)}}
}

func deviceGlyph(platform string) *core.SVG {
	switch platform {
	case "android":
		return lucide.Smartphone()
	case "darwin":
		return lucide.AppWindowMac()
	case "windows":
		return lucide.PanelsTopLeft()
	case "linux":
		return lucide.SquareTerminal()
	default:
		return lucide.Monitor()
	}
}

func (g *gui) deviceNavigation(cx *core.Scope, selected *core.Signal[string]) core.Node {
	row := func(id string, label func() string, glyph *core.SVG) core.Node {
		active := func() bool {
			if id == allDevices {
				return selected.Get() == allDevices || g.selectedMember(selected.Get()).ID == ""
			}
			return g.selectedMember(selected.Get()).ID == id
		}
		return base.Button{LabelFunc: label, Selected: active, OnPress: func(core.PressEvent) { selected.Set(id) },
			Class: "flex-row items-center gap-2 pl-5 pr-2 h-[35px] rounded-md hover:bg-muted",
			StyleFunc: func() core.Style {
				s := core.Style{FocusVisible: ui.FocusRing(cx, -2)}
				if active() {
					s.Background = ui.ColorsOf(cx).Text().Faded(0.09)
				}
				return s
			}, Children: []core.Node{
				core.Icon{SVG: glyph, Class: "w-[16px] h-[16px] text-muted-foreground"},
				core.Text{Class: "flex-1 shrink text-sm", MaxLines: 1, ContentFunc: label},
			},
		}
	}
	return core.Scroll{Style: core.Style{FlexShrink: 1, MinHeight: core.Pt(0)}, Children: []core.Node{
		core.View{Class: "gap-1", Children: []core.Node{
			core.Each(func() []backend.DeviceDTO { return g.mem().Devices }, func(d backend.DeviceDTO) string { return d.ID }, func(_ *core.Scope, d func() backend.DeviceDTO, _ func() int) core.Node {
				return row(d().ID, func() string { return d().Name }, deviceGlyph(d().Platform))
			}),
			row(allDevices, func() string { return "全部设备" }, lucide.LayoutGrid()),
		}},
	}}
}

func (g *gui) deviceOverview(selected *core.Signal[string]) core.Node {
	return core.View{Class: "grid grid-cols-2 gap-3", Children: []core.Node{
		core.Each(func() []backend.DeviceDTO { return g.mem().Devices }, func(d backend.DeviceDTO) string { return d.ID },
			func(cx *core.Scope, d func() backend.DeviceDTO, _ func() int) core.Node {
				return base.Button{LabelFunc: func() string { return "查看" + d().Name }, OnPress: func(core.PressEvent) { selected.Set(d().ID) },
					Class: "flex-row items-center gap-3 rounded-lg border border-border p-3 hover:bg-muted", Style: core.Style{FocusVisible: ui.FocusRing(cx, 1)}, Children: []core.Node{
						deviceArt(d().Platform == "android", 64),
						core.View{Class: "flex-1 shrink gap-1", Children: []core.Node{
							core.Text{Class: "text-base font-semibold", ContentFunc: func() string { return d().Name }, MaxLines: 1},
							textf("text-xs text-muted-foreground", func() string { return platformName(d()) }),
						}},
						icon(lucide.ChevronRight, "w-[16px] h-[16px] text-muted-foreground"),
					}}
			}),
	}}
}

func (g *gui) deviceLanding(cx *core.Scope, current func() backend.DeviceDTO, connecting *core.Signal[string], connect, info, manage func()) core.Node {
	action := func(label string, glyph func() *core.SVG, f func()) core.Node {
		return base.Button{Label: label, OnPress: func(core.PressEvent) { f() }, Class: "flex-1 flex-row items-center justify-center gap-2 h-[47px] hover:bg-muted",
			Style: core.Style{FocusVisible: ui.FocusRing(cx, -2)}, Children: []core.Node{icon(glyph, "w-[17px] h-[17px]"), text("text-sm font-medium", label)}}
	}
	return core.View{Class: "mt-2 rounded-lg overflow-hidden border border-border", Children: []core.Node{
		core.View{Class: "relative overflow-hidden bg-muted", Style: core.Style{AspectRatio: 2.5}, Children: []core.Node{
			core.Show{When: func() bool { return current().Platform == "android" }, Then: func(*core.Scope) core.Node {
				return core.View{Class: "absolute inset-0 items-center justify-center gap-2", Children: []core.Node{deviceArt(true, 100), text("text-sm text-muted-foreground", "这台设备仅用于控制其他电脑")}}
			}, Else: func(*core.Scope) core.Node {
				return core.View{Class: "absolute inset-0", Children: []core.Node{
					core.Image{Source: core.ImageSource{Data: desktopCover}, ResizeMode: core.Cover, ElementsHidden: true, Class: "absolute inset-0 w-full h-full"},
					core.View{Class: "absolute inset-0", Style: core.Style{Gradients: []core.Gradient{core.LinearGradient(180, core.RGBA(0x12161c, 0), core.RGBA(0x12161c, 0), core.RGBA(0x12161c, 0.62))}}},
					core.Show{When: func() bool { return canEnterDesktop(current()) && !g.mem().Conflict }, Then: func(*core.Scope) core.Node {
						return base.Button{Class: "absolute inset-0 items-center justify-end pb-6 hover:bg-black/10", LabelFunc: func() string { return "进入" + current().Name + "的桌面" }, Busy: func() bool { return connecting.Get() == current().ID }, Disabled: func() bool { return connecting.Get() != "" }, OnPress: func(core.PressEvent) { connect() }, Style: core.Style{FocusVisible: ui.FocusRing(cx, -3)}, Children: []core.Node{
							core.View{Class: "flex-row items-center gap-3 px-4 py-1", Children: []core.Node{textf("text-lg font-semibold text-white", func() string {
								if connecting.Get() == current().ID {
									return "正在连接…"
								}
								return "进入桌面"
							}), icon(lucide.ArrowRight, "w-[16px] h-[16px] text-white")}},
						}}
					}, Else: func(*core.Scope) core.Node {
						return core.View{Class: "absolute bottom-6 self-center px-4 py-1", Children: []core.Node{textf("text-sm text-white", func() string {
							if current().Self {
								return "当前使用的设备"
							}
							if g.mem().Conflict {
								return "设备组连接已暂停"
							}
							return "此设备未开启直连"
						})}}
					}},
				}}
			}},
		}},
		core.View{Class: "flex-row items-center", Children: []core.Node{
			action("设备详情", lucide.Info, info), core.View{Class: "h-[18px] border-l border-border"},
			action("立即同步", lucide.RefreshCw, func() { go g.be.SyncMembers() }), core.View{Class: "h-[18px] border-l border-border"},
			action("设备组管理", lucide.Settings2, manage),
		}},
	}}
}

func (g *gui) deviceInfoDialog(detailID *core.Signal[string], removing *core.Signal[backend.DeviceDTO]) core.Node {
	current := func() backend.DeviceDTO {
		id := detailID.Get()
		if id != "" {
			for _, d := range g.mem().Devices {
				if d.ID == id {
					return d
				}
			}
		}
		return backend.DeviceDTO{}
	}
	closeDetail := func() { detailID.Set("") }
	return ui.Dialog{Open: func() bool { return current().ID != "" }, OnOpenChange: func(v bool, _ base.Reason) {
		if !v {
			closeDetail()
		}
	}, Title: "设备详情",
		DescriptionFunc: func() string { return current().Name }, Children: []core.Node{
			textf("text-sm", func() string { return platformName(current()) }),
			textf("text-sm font-mono", func() string { return "设备指纹  " + current().Short }),
			textf("text-xs text-muted-foreground", func() string {
				if current().Self {
					return "本机"
				}
				if current().Synced {
					return "成员信息已同步"
				}
				return "成员信息待同步"
			}),
		}, Footer: []core.Node{
			core.Show{When: func() bool { return !current().Self }, Then: func(*core.Scope) core.Node {
				return ui.Button{Text: "移除设备", Variant: ui.ButtonGhost, OnPress: func() { d := current(); closeDetail(); removing.Set(d) }}
			}},
			ui.Button{Text: "完成", Variant: ui.ButtonOutline, OnPress: closeDetail},
		}}
}
