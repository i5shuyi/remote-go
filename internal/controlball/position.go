// Package controlball positions the session control in logical screen points.
package controlball

const (
	Size          = 40
	inset         = 12
	snapDistance  = 24
	DragThreshold = 6
)

// Position is a fraction of the available travel on each axis. An attached
// edge therefore stays attached when a window resizes or a phone rotates.
type Position struct{ X, Y float64 }

func TopRight() Position { return Position{X: 1} }

func travel(extent float64) (start, span float64) {
	free := max(0, extent-Size)
	start = min(inset, free/2)
	return start, free - 2*start
}

func (p Position) Origin(width, height float64) (x, y float64) {
	left, w := travel(width)
	top, h := travel(height)
	return left + p.X*w, top + p.Y*h
}

func fraction(at, extent, previous float64, snap bool) float64 {
	start, span := travel(extent)
	if span == 0 {
		return previous
	}
	d := min(span, max(0, at-start))
	if snap && min(d, span-d) <= snapDistance {
		if d < span/2 {
			return 0
		}
		return 1
	}
	return d / span
}

// Move follows the pointer, clamping the entire ball to the viewport.
// On release each nearby edge can attract the ball, including at corners.
func (p Position) Move(x, y, width, height float64, snap bool) Position {
	return Position{fraction(x, width, p.X, snap), fraction(y, height, p.Y, snap)}
}
