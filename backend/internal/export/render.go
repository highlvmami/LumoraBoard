// Package export turns boards into files (PNG, PDF, a JSON backup) on a
// bounded worker pool, and reads JSON backups back in.
package export

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/highlvmami/lumoraboard/backend/internal/board"
)

// Rendering constants mirror the browser renderer (frontend render.ts).
const (
	textSize     = 20.0
	textLine     = 1.25
	stickySize   = 160.0
	stickyPad    = 10.0
	margin       = 32.0
	defaultColor = "#1f2937"
	stickyFill   = "#fde68a"
)

// rgb is a colour with components in [0,1].
type rgb struct{ r, g, b float64 }

// parseColor reads #rgb or #rrggbb; anything else becomes the default ink,
// so a hand-edited colour never fails an export.
func parseColor(s string) rgb {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return parseColor(defaultColor)
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return parseColor(defaultColor)
	}
	return rgb{float64(v>>16&0xff) / 255, float64(v>>8&0xff) / 255, float64(v&0xff) / 255}
}

// box is an axis-aligned rectangle in board coordinates.
type box struct{ x, y, w, h float64 }

// objectBounds matches geometry.ts bounds().
func objectBounds(o board.Object) box {
	if (o.Kind == board.KindStroke || o.Kind == board.KindArrow) && len(o.Points) > 0 {
		minX, minY := math.Inf(1), math.Inf(1)
		maxX, maxY := math.Inf(-1), math.Inf(-1)
		for _, p := range o.Points {
			minX, minY = math.Min(minX, p.X), math.Min(minY, p.Y)
			maxX, maxY = math.Max(maxX, p.X), math.Max(maxY, p.Y)
		}
		pad := width(o) / 2
		if o.Kind == board.KindArrow {
			pad = math.Max(pad, arrowHead(o)) // the head sticks out
		}
		return box{o.X + minX - pad, o.Y + minY - pad, maxX - minX + 2*pad, maxY - minY + 2*pad}
	}
	if o.Kind == board.KindSticky {
		return box{o.X, o.Y, orDefault(o.W, stickySize), orDefault(o.H, stickySize)}
	}
	return box{o.X, o.Y, o.W, o.H}
}

// area is what an export covers: every object plus a margin, or a blank
// page when the board is empty.
func area(objs []board.Object) box {
	if len(objs) == 0 {
		return box{0, 0, 800, 600}
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, o := range objs {
		b := objectBounds(o)
		minX, minY = math.Min(minX, b.x), math.Min(minY, b.y)
		maxX, maxY = math.Max(maxX, b.x+b.w), math.Max(maxY, b.y+b.h)
	}
	return box{minX - margin, minY - margin, maxX - minX + 2*margin, maxY - minY + 2*margin}
}

func orDefault(v, d float64) float64 {
	if v == 0 {
		return d
	}
	return v
}

func width(o board.Object) float64 { return orDefault(o.StrokeWidth, 2) }

func arrowHead(o board.Object) float64 { return math.Max(10, width(o)*4) }

// surface is what a format draws on. Coordinates are in board units; each
// implementation maps them to its own pixels or points.
type surface interface {
	polyline(pts []board.Point, c rgb, w float64)
	dot(x, y, r float64, c rgb)
	rect(b box, c rgb, w float64)
	fillRect(b box, c rgb)
	ellipse(b box, c rgb, w float64)
	fillPolygon(pts []board.Point, c rgb)
	// text draws lines with their top-left corner at x, y.
	text(lines []string, x, y, size float64, c rgb)
}

// paint draws objs in order (they come sorted by z), reporting progress
// after each one and stopping early if ctx is cancelled.
func paint(ctx context.Context, s surface, objs []board.Object, progress func(done, total int)) error {
	for i, o := range objs {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		drawObject(s, o)
		progress(i+1, len(objs))
	}
	return ctx.Err()
}

func drawObject(s surface, o board.Object) {
	c := parseColor(orString(o.Color, defaultColor))
	w := width(o)
	switch o.Kind {
	case board.KindStroke:
		switch len(o.Points) {
		case 0:
		case 1:
			s.dot(o.X+o.Points[0].X, o.Y+o.Points[0].Y, w/2, c)
		default:
			s.polyline(smooth(o.X, o.Y, o.Points), c, w)
		}
	case board.KindRect:
		s.rect(box{o.X, o.Y, o.W, o.H}, c, w)
	case board.KindEllipse:
		s.ellipse(box{o.X, o.Y, o.W, o.H}, c, w)
	case board.KindArrow:
		if len(o.Points) < 2 {
			return
		}
		a, b := o.Points[0], o.Points[len(o.Points)-1]
		ax, ay, bx, by := o.X+a.X, o.Y+a.Y, o.X+b.X, o.Y+b.Y
		s.polyline([]board.Point{{X: ax, Y: ay}, {X: bx, Y: by}}, c, w)
		angle := math.Atan2(by-ay, bx-ax)
		head := arrowHead(o)
		s.fillPolygon([]board.Point{
			{X: bx, Y: by},
			{X: bx - head*math.Cos(angle-math.Pi/7), Y: by - head*math.Sin(angle-math.Pi/7)},
			{X: bx - head*math.Cos(angle+math.Pi/7), Y: by - head*math.Sin(angle+math.Pi/7)},
		}, c)
	case board.KindText:
		s.text(strings.Split(o.Text, "\n"), o.X, o.Y, fontSize(o), c)
	case board.KindSticky:
		b := objectBounds(o)
		s.fillRect(b, parseColor(stickyFill))
		s.text(strings.Split(o.Text, "\n"), o.X+stickyPad, o.Y+stickyPad, textSize, parseColor(defaultColor))
	}
}

// fontSize matches geometry.ts textSize(): a text object's size follows
// its box height, so resizing the box scales the text. Text that was never
// resized has h = lines*textSize*textLine and comes out at textSize.
func fontSize(o board.Object) float64 {
	lines := float64(strings.Count(o.Text, "\n") + 1)
	if o.H <= 0 {
		return textSize
	}
	return o.H / (lines * textLine)
}

func orString(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// smooth flattens the quadratic curves the browser draws through segment
// midpoints into a polyline, so every surface only needs straight lines.
func smooth(ox, oy float64, pts []board.Point) []board.Point {
	const steps = 6
	out := make([]board.Point, 0, len(pts)*steps)
	at := board.Point{X: ox + pts[0].X, Y: oy + pts[0].Y}
	out = append(out, at)
	for i := 1; i < len(pts)-1; i++ {
		ctrl := board.Point{X: ox + pts[i].X, Y: oy + pts[i].Y}
		end := board.Point{X: ox + (pts[i].X+pts[i+1].X)/2, Y: oy + (pts[i].Y+pts[i+1].Y)/2}
		for k := 1; k <= steps; k++ {
			t := float64(k) / steps
			u := 1 - t
			out = append(out, board.Point{
				X: u*u*at.X + 2*u*t*ctrl.X + t*t*end.X,
				Y: u*u*at.Y + 2*u*t*ctrl.Y + t*t*end.Y,
			})
		}
		at = end
	}
	last := pts[len(pts)-1]
	return append(out, board.Point{X: ox + last.X, Y: oy + last.Y})
}
