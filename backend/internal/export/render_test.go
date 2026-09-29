package export

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/highlvmami/lumoraboard/backend/internal/board"
)

func sample() []board.Object {
	return []board.Object{
		{ID: "r", Kind: board.KindRect, X: 0, Y: 0, W: 100, H: 50, Color: "#e11d48", StrokeWidth: 4, Z: 1, Version: 1},
		{ID: "s", Kind: board.KindSticky, X: 200, Y: 0, Text: "Çarşı ığdır", Z: 2, Version: 2},
		{ID: "p", Kind: board.KindStroke, X: 0, Y: 100, Points: []board.Point{{X: 0, Y: 0}, {X: 40, Y: 20}, {X: 80, Y: 0}}, Z: 3, Version: 3},
		{ID: "a", Kind: board.KindArrow, X: 0, Y: 200, Points: []board.Point{{X: 0, Y: 0}, {X: 100, Y: 0}}, Z: 4, Version: 4},
		{ID: "e", Kind: board.KindEllipse, X: 120, Y: 200, W: 60, H: 30, Color: "#abc", Z: 5, Version: 5},
		{ID: "t", Kind: board.KindText, X: 200, Y: 200, W: 80, H: 25, Text: "merhaba\ndünya", Color: "not a colour", Z: 6, Version: 6},
	}
}

func noProgress(int, int) {}

func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestPNGCoversTheBoardAtScale(t *testing.T) {
	objs := sample()
	a := area(objs)
	data, err := render(context.Background(), FormatPNG, "b", objs, 2, noProgress)
	if err != nil {
		t.Fatal(err)
	}
	img := decodePNG(t, data)
	if got, want := img.Bounds().Dx(), int(a.w*2+0.999); got != want {
		t.Fatalf("width %d, want %d", got, want)
	}
	// The rect's left edge is red; the sticky is yellow; the margin is white.
	at := func(x, y float64) (uint32, uint32, uint32) {
		r, g, b, _ := img.At(int((x-a.x)*2), int((y-a.y)*2)).RGBA()
		return r >> 8, g >> 8, b >> 8
	}
	if r, g, b := at(0, 25); r < 200 || g > 80 || b > 120 {
		t.Fatalf("rect edge = %d,%d,%d", r, g, b)
	}
	if r, g, b := at(350, 150); r < 240 || g < 200 || b > 160 {
		t.Fatalf("sticky = %d,%d,%d", r, g, b)
	}
	if r, g, b := at(a.x+2, a.y+2); r != 255 || g != 255 || b != 255 {
		t.Fatalf("margin = %d,%d,%d", r, g, b)
	}
}

func TestPNGSizeIsCapped(t *testing.T) {
	for _, c := range []struct {
		a     box
		scale float64
		w, h  int
	}{
		{box{0, 0, 100, 50}, 2, 200, 100},
		{box{0, 0, 100, 50}, 0, 100, 50},             // at least 1x
		{box{0, 0, 100, 50}, 99, 400, 200},           // at most MaxScale
		{box{0, 0, 100_000, 100_000}, 4, 4000, 4000}, // area cap
	} {
		w, h, _ := pngSize(c.a, c.scale)
		if w != c.w || h != c.h {
			t.Errorf("%+v at %v: %dx%d, want %dx%d", c.a, c.scale, w, h, c.w, c.h)
		}
	}
}

func TestEmptyBoardIsABlankPage(t *testing.T) {
	data, err := render(context.Background(), FormatPNG, "b", nil, 1, noProgress)
	if err != nil {
		t.Fatal(err)
	}
	if b := decodePNG(t, data).Bounds(); b.Dx() != 800 || b.Dy() != 600 {
		t.Fatalf("blank page %v", b)
	}
}

func TestPDF(t *testing.T) {
	var calls int
	data, err := render(context.Background(), FormatPDF, "b", sample(), 1, func(done, total int) {
		calls++
		if done > total {
			t.Fatalf("progress %d/%d", done, total)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) || calls != len(sample()) {
		t.Fatalf("pdf %q…, %d progress calls", data[:8], calls)
	}
}

func TestRenderStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, f := range []Format{FormatPNG, FormatPDF} {
		if _, err := render(ctx, f, "b", sample(), 1, noProgress); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

func TestBackupRoundTrip(t *testing.T) {
	data, err := render(context.Background(), FormatJSON, "src", sample(), 1, noProgress)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := ParseBackup(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Objects) != len(sample()) || snap.Seq != uint64(len(sample())) {
		t.Fatalf("snapshot: %d objects, seq %d", len(snap.Objects), snap.Seq)
	}
	for i, o := range snap.Objects {
		want := sample()[i]
		if o.ID != want.ID || o.Text != want.Text || o.Z != want.Z || o.Version > snap.Seq || o.CreatedBy != "import" {
			t.Fatalf("object %d = %+v", i, o)
		}
	}
}

func TestBadBackupsAreRefused(t *testing.T) {
	obj := func(o string) string {
		return `{"format":"lumoraboard.board","version":1,"objects":[` + o + `]}`
	}
	for name, body := range map[string]string{
		"not json":     `{`,
		"other format": `{"format":"excalidraw","version":1,"objects":[]}`,
		"new version":  `{"format":"lumoraboard.board","version":2,"objects":[]}`,
		"bad kind":     obj(`{"id":"a","kind":"star"}`),
		"bad id":       obj(`{"id":"a b","kind":"rect"}`),
		"far away":     obj(`{"id":"a","kind":"rect","x":1e12}`),
		"duplicate":    obj(`{"id":"a","kind":"rect"},{"id":"a","kind":"rect"}`),
		"long text":    obj(`{"id":"a","kind":"text","text":"` + strings.Repeat("x", board.MaxTextLen+1) + `"}`),
	} {
		if _, err := ParseBackup([]byte(body)); !errors.Is(err, ErrBadBackup) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := ParseBackup([]byte(obj(``))); err != nil {
		t.Fatalf("empty backup: %v", err)
	}
}
