package gui

import (
	"bytes"
	"image"
	"image/color"
	"image/png"

	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/desktop"
)

// Like UU Remote, closing the main window does not quit: the device stays
// reachable for remote control from the menu bar / notification area, and
// only 退出 (or Cmd+Q) ends it.
func (g *gui) runInBackground() {
	desktop.SetShutdownMode(g.app, core.ShutdownOnExplicit)
	desktop.OnQuitRequest(g.app, func(*desktop.QuitRequest) { g.quitting = true })
	desktop.NewTray(g.app, desktop.TrayOptions{
		Icon:     trayIcon(),
		Template: true,
		Tooltip:  "Remote Go",
		Menu: func() []desktop.MenuItem {
			st := g.st.GetUntracked()
			status := "设备代码 分配中…"
			if st.DeviceCode != "" {
				status = "设备代码 " + groupCode(st.DeviceCode)
			}
			if !st.Hosting {
				status += "（未允许远程控制）"
			}
			return []desktop.MenuItem{
				{Label: "打开 Remote Go", OnSelect: g.showMain},
				{Label: status, Disabled: true},
				{Separator: true},
				{Label: "退出 Remote Go", OnSelect: g.quit},
			}
		},
	})
}

// hideOnClose makes the main window's close button hide it.
func (g *gui) hideOnClose(cx *core.Scope) {
	w := desktop.CurrentWindow(cx)
	if w == nil {
		return
	}
	g.mainWin = w
	w.OnClosing(cx, func(c *desktop.WindowClosing) {
		if g.quitting {
			return
		}
		c.Cancel()
		w.Hide()
	})
}

func (g *gui) showMain() {
	if g.mainWin != nil && !g.mainWin.Closed() {
		g.mainWin.Show()
		g.mainWin.Focus()
	}
}

func (g *gui) quit() {
	g.quitting = true
	g.app.Quit()
}

// trayIcon draws a small monitor as a template image (black + alpha).
func trayIcon() []byte {
	const n = 36
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	on := color.NRGBA{A: 255}
	fill := func(x0, y0, x1, y1 int) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.Set(x, y, on)
			}
		}
	}
	// screen outline
	fill(3, 6, 33, 9)
	fill(3, 24, 33, 27)
	fill(3, 6, 6, 27)
	fill(30, 6, 33, 27)
	// play-like arrow inside: remote control
	for i := 0; i < 8; i++ {
		fill(14+i, 11+i/2, 15+i, 22-i/2)
	}
	// stand
	fill(16, 27, 20, 30)
	fill(11, 30, 25, 32)
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}
