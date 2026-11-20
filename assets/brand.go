package assets

import (
	_ "embed"

	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/ui"
)

//go:embed Icon.png
var logo []byte

var Accent = ui.Fixed(core.RGB(0x096e89))

// Logo sits beside the product name, which provides its accessible label.
func Logo() core.Image {
	return core.Image{Source: core.ImageSource{Data: logo}, Class: "w-[40px] h-[40px]", ElementsHidden: true}
}

// ConnectButton uses a neutral fill with light text in both appearances.
type ConnectButton ui.Button

func (b ConnectButton) Render(cx *core.Scope) core.Node {
	theme := cx.Theme().Clone()
	theme.Light.Accent = core.RGB(0x565c65)
	theme.Dark.Accent = core.RGB(0x626974)
	for _, p := range []*core.Palette{&theme.Light, &theme.Dark} {
		p.AccentText = core.RGB(0xffffff)
		p.AccentHover = ui.Mix(p.Accent, ui.Black(1), 0.08)
		p.AccentPressed = ui.Mix(p.Accent, ui.Black(1), 0.16)
	}
	cx.SetTheme(theme)
	return ui.Button(b)
}
