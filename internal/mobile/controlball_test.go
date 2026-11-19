package mobile

import (
	"testing"

	"github.com/i5shuyi/remote-go/internal/controlball"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/nsgotest"
)

func TestControlBallGestures(t *testing.T) {
	var opened int
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		return core.View{Class: "flex-1", Children: []core.Node{mobileControlBall(cx, core.NewSignal(cx, controlball.TopRight()), func() { opened++ })}}
	}, nsgotest.Size(400, 600))
	const ball = "label:工具栏"
	check := func(x, y core.Points) {
		t.Helper()
		frame := app.Find(ball).Frame
		if frame.Origin != (core.Point{X: x, Y: y}) || frame.Size.Width != controlball.Size || frame.Size.Height != controlball.Size {
			t.Fatalf("unexpected control frame %+v, wanted origin (%v,%v)", frame, x, y)
		}
	}
	check(348, 12)
	app.Pan(ball, core.Point{X: -200, Y: 100})
	check(148, 112)
	app.Pan(ball, core.Point{X: -127, Y: 427})
	check(12, 548)
	app.Resize(600, 400)
	check(12, 348)
	app.Pan(ball, core.Point{X: 2000, Y: -2000})
	check(548, 12)
	if opened != 0 {
		t.Fatalf("drag opened the panel %d times", opened)
	}
	app.Press(ball)
	if opened != 1 {
		t.Fatal("tap did not open the panel")
	}
}
