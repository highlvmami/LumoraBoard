package export

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"math"
	"sync"
	"time"

	"github.com/fogleman/gg"
	"github.com/go-pdf/fpdf"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"

	"github.com/highlvmami/lumoraboard/backend/internal/board"
)

// Format is an export file type.
type Format string

const (
	FormatPNG  Format = "png"
	FormatPDF  Format = "pdf"
	FormatJSON Format = "json"
)

func (f Format) valid() bool { return f == FormatPNG || f == FormatPDF || f == FormatJSON }

// ContentType is the MIME type of the format.
func (f Format) ContentType() string {
	switch f {
	case FormatPNG:
		return "image/png"
	case FormatPDF:
		return "application/pdf"
	default:
		return "application/json"
	}
}

const (
	// MaxScale caps PNG resolution per board unit.
	MaxScale = 4
	// maxPixels caps a PNG's area, whatever the scale; big boards get a
	// lower effective scale instead of a huge allocation. 16M pixels is
	// 64 MB of RGBA per worker while it renders.
	maxPixels = 16_000_000
	// maxPDFPoints is the largest page side PDF readers accept (200 in).
	maxPDFPoints = 14_400
)

var (
	fontOnce sync.Once
	goFont   *truetype.Font
	fontErr  error
)

func regularFont() (*truetype.Font, error) {
	fontOnce.Do(func() { goFont, fontErr = truetype.Parse(goregular.TTF) })
	return goFont, fontErr
}

// render produces the file for objs.
func render(ctx context.Context, f Format, name string, objs []board.Object, scale float64, progress func(done, total int)) ([]byte, error) {
	switch f {
	case FormatPNG:
		return renderPNG(ctx, objs, scale, progress)
	case FormatPDF:
		return renderPDF(ctx, objs, progress)
	case FormatJSON:
		progress(len(objs), len(objs))
		return json.MarshalIndent(Backup{Format: backupFormat, Version: backupVersion, Board: name, ExportedAt: time.Now().UTC(), Objects: objs}, "", "  ")
	}
	return nil, fmt.Errorf("unknown format %q", f)
}

// ---- PNG -----------------------------------------------------------------

type pngSurface struct {
	dc     *gg.Context
	origin box
	scale  float64
	font   *truetype.Font
	faces  map[float64]font.Face
}

func renderPNG(ctx context.Context, objs []board.Object, scale float64, progress func(done, total int)) ([]byte, error) {
	a := area(objs)
	w, h, scale := pngSize(a, scale)
	dc := gg.NewContext(w, h)
	dc.SetColor(color.White)
	dc.Clear()
	dc.SetLineCapRound()
	dc.SetLineJoinRound()
	f, err := regularFont()
	if err != nil {
		return nil, err
	}
	s := &pngSurface{dc: dc, origin: a, scale: scale, font: f, faces: map[float64]font.Face{}}
	if err := paint(ctx, s, objs, progress); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := dc.EncodePNG(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// pngSize clamps the requested scale to [1, MaxScale] and then lowers it
// until the image fits in maxPixels.
func pngSize(a box, scale float64) (w, h int, eff float64) {
	scale = math.Min(math.Max(scale, 1), MaxScale)
	if px := a.w * a.h * scale * scale; px > maxPixels {
		scale *= math.Sqrt(maxPixels / px)
	}
	return max(int(a.w*scale), 1), max(int(a.h*scale), 1), scale
}

func (s *pngSurface) px(x, y float64) (float64, float64) {
	return (x - s.origin.x) * s.scale, (y - s.origin.y) * s.scale
}

func (s *pngSurface) set(c rgb, w float64) {
	s.dc.SetRGB(c.r, c.g, c.b)
	s.dc.SetLineWidth(w * s.scale)
}

func (s *pngSurface) polyline(pts []board.Point, c rgb, w float64) {
	s.set(c, w)
	for i, p := range pts {
		x, y := s.px(p.X, p.Y)
		if i == 0 {
			s.dc.MoveTo(x, y)
		} else {
			s.dc.LineTo(x, y)
		}
	}
	s.dc.Stroke()
}

func (s *pngSurface) dot(x, y, r float64, c rgb) {
	s.set(c, 0)
	px, py := s.px(x, y)
	s.dc.DrawCircle(px, py, r*s.scale)
	s.dc.Fill()
}

func (s *pngSurface) rect(b box, c rgb, w float64) {
	s.set(c, w)
	x, y := s.px(b.x, b.y)
	s.dc.DrawRectangle(x, y, b.w*s.scale, b.h*s.scale)
	s.dc.Stroke()
}

func (s *pngSurface) fillRect(b box, c rgb) {
	s.set(c, 0)
	x, y := s.px(b.x, b.y)
	s.dc.DrawRectangle(x, y, b.w*s.scale, b.h*s.scale)
	s.dc.Fill()
}

func (s *pngSurface) ellipse(b box, c rgb, w float64) {
	s.set(c, w)
	x, y := s.px(b.x+b.w/2, b.y+b.h/2)
	s.dc.DrawEllipse(x, y, b.w/2*s.scale, b.h/2*s.scale)
	s.dc.Stroke()
}

func (s *pngSurface) fillPolygon(pts []board.Point, c rgb) {
	s.set(c, 0)
	for i, p := range pts {
		x, y := s.px(p.X, p.Y)
		if i == 0 {
			s.dc.MoveTo(x, y)
		} else {
			s.dc.LineTo(x, y)
		}
	}
	s.dc.ClosePath()
	s.dc.Fill()
}

func (s *pngSurface) text(lines []string, x, y, size float64, c rgb) {
	s.set(c, 0)
	// Faces are cached per size: a board usually has only a few.
	face, ok := s.faces[size]
	if !ok {
		face = truetype.NewFace(s.font, &truetype.Options{Size: size * s.scale})
		s.faces[size] = face
	}
	s.dc.SetFontFace(face)
	for i, line := range lines {
		px, py := s.px(x, y+float64(i)*size*textLine)
		// ay=1 puts the top of the line at py, like textBaseline 'top'.
		s.dc.DrawStringAnchored(line, px, py, 0, 1)
	}
}

// ---- PDF -----------------------------------------------------------------

type pdfSurface struct {
	pdf    *fpdf.Fpdf
	origin box
	scale  float64
}

func renderPDF(ctx context.Context, objs []board.Object, progress func(done, total int)) ([]byte, error) {
	a := area(objs)
	// One board unit is one point unless the page would be too large.
	scale := math.Min(1, maxPDFPoints/math.Max(a.w, a.h))
	pdf := fpdf.NewCustom(&fpdf.InitType{
		UnitStr: "pt",
		Size:    fpdf.SizeType{Wd: a.w * scale, Ht: a.h * scale},
	})
	pdf.SetMargins(0, 0, 0)
	pdf.SetAutoPageBreak(false, 0)
	pdf.SetCreator("LumoraBoard", true)
	pdf.AddUTF8FontFromBytes("go", "", goregular.TTF)
	pdf.SetFont("go", "", textSize*scale)
	pdf.SetLineCapStyle("round")
	pdf.SetLineJoinStyle("round")
	pdf.AddPage()

	if err := paint(ctx, &pdfSurface{pdf: pdf, origin: a, scale: scale}, objs, progress); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *pdfSurface) pt(x, y float64) (float64, float64) {
	return (x - s.origin.x) * s.scale, (y - s.origin.y) * s.scale
}

func (s *pdfSurface) set(c rgb, w float64) {
	r, g, b := int(c.r*255+0.5), int(c.g*255+0.5), int(c.b*255+0.5)
	s.pdf.SetDrawColor(r, g, b)
	s.pdf.SetFillColor(r, g, b)
	s.pdf.SetTextColor(r, g, b)
	s.pdf.SetLineWidth(w * s.scale)
}

func (s *pdfSurface) points(pts []board.Point) []fpdf.PointType {
	out := make([]fpdf.PointType, len(pts))
	for i, p := range pts {
		out[i].X, out[i].Y = s.pt(p.X, p.Y)
	}
	return out
}

func (s *pdfSurface) polyline(pts []board.Point, c rgb, w float64) {
	s.set(c, w)
	ps := s.points(pts)
	for i := 1; i < len(ps); i++ {
		s.pdf.Line(ps[i-1].X, ps[i-1].Y, ps[i].X, ps[i].Y)
	}
}

func (s *pdfSurface) dot(x, y, r float64, c rgb) {
	s.set(c, 0)
	px, py := s.pt(x, y)
	s.pdf.Circle(px, py, r*s.scale, "F")
}

func (s *pdfSurface) rect(b box, c rgb, w float64) {
	s.set(c, w)
	x, y := s.pt(b.x, b.y)
	s.pdf.Rect(x, y, b.w*s.scale, b.h*s.scale, "D")
}

func (s *pdfSurface) fillRect(b box, c rgb) {
	s.set(c, 0)
	x, y := s.pt(b.x, b.y)
	s.pdf.Rect(x, y, b.w*s.scale, b.h*s.scale, "F")
}

func (s *pdfSurface) ellipse(b box, c rgb, w float64) {
	s.set(c, w)
	x, y := s.pt(b.x+b.w/2, b.y+b.h/2)
	s.pdf.Ellipse(x, y, b.w/2*s.scale, b.h/2*s.scale, 0, "D")
}

func (s *pdfSurface) fillPolygon(pts []board.Point, c rgb) {
	s.set(c, 0)
	s.pdf.Polygon(s.points(pts), "F")
}

func (s *pdfSurface) text(lines []string, x, y, size float64, c rgb) {
	s.set(c, 0)
	s.pdf.SetFontSize(size * s.scale)
	for i, line := range lines {
		px, py := s.pt(x, y+float64(i)*size*textLine)
		// Text takes a baseline; the ascent of Go Regular is about 0.9 em.
		s.pdf.Text(px, py+0.9*size*s.scale, line)
	}
}
