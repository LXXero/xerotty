package renderer

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/LXXero/xerotty/internal/glyphcache"
	"github.com/LXXero/xerotty/internal/platform"
)

// noGlyphs is a GlyphSource where no font has any rune, recording which
// runes the renderer asked for.
type noGlyphs struct {
	fakeGlyphs
	asked map[rune]bool
}

func (g *noGlyphs) Get(r rune, _ bool) *glyphcache.Entry {
	g.asked[r] = true
	return nil
}

// cellGrid is a one-row EmulatorView over explicit cells, so a cell can
// hold any rune sequence and width.
type cellGrid struct{ cells []uv.Cell }

func (g *cellGrid) Width() int               { return 8 }
func (g *cellGrid) Height() int              { return 1 }
func (g *cellGrid) ScrollbackLen() int       { return 0 }
func (g *cellGrid) RenderGeneration() uint64 { return 1 }
func (g *cellGrid) at(col int) *uv.Cell {
	if col < len(g.cells) {
		c := g.cells[col]
		return &c
	}
	return &uv.Cell{Content: " ", Width: 1}
}
func (g *cellGrid) CellAt(col, row int) *uv.Cell {
	if row != 0 {
		return nil
	}
	return g.at(col)
}
func (g *cellGrid) ScrollbackCellAt(int, int) *uv.Cell { return nil }
func (g *cellGrid) SnapshotWindow(_, _, rows, cols int) ([][]uv.Cell, int, uint64) {
	out := make([][]uv.Cell, rows)
	for row := range out {
		out[row] = make([]uv.Cell, cols)
		if row == 0 {
			for col := 0; col < cols; col++ {
				out[row][col] = *g.at(col)
			}
		}
	}
	return out, 0, 1
}

// drawCell renders one cell (plus its spacer when wide) with no glyphs
// available and returns the untextured rects drawn and the runes asked.
func drawCell(t *testing.T, content string, width int) ([]platform.GlyphQuad, map[rune]bool) {
	t.Helper()
	g := &noGlyphs{asked: map[rune]bool{}}
	r := &Renderer{
		Theme:   DefaultTheme(),
		Metrics: CellMetrics{Width: 8, Height: 16},
		Glyphs:  g,
	}
	cells := []uv.Cell{{Content: content, Width: width}}
	for i := 1; i < width; i++ {
		cells = append(cells, uv.Cell{Content: "", Width: 0})
	}
	_, white := g.WhitePage()
	var rects []platform.GlyphQuad
	for _, q := range draw(r, &cellGrid{cells: cells}, 0) {
		if q.U0 == white.X && q.V0 == white.Y && q.U1 == white.X && q.V1 == white.Y {
			rects = append(rects, q)
		}
	}
	return rects, g.asked
}

// A rune no font has draws the four sides of a box inside its cell.
func TestMissingGlyphDrawsBox(t *testing.T) {
	rects, _ := drawCell(t, "", 1)
	if len(rects) != 4 {
		t.Fatalf("got %d rects; want the 4 sides of a box", len(rects))
	}
	for _, q := range rects {
		if q.X0 < 0 || q.X1 > 8 || q.Y0 < 0 || q.Y1 > 16 {
			t.Fatalf("box side %+v leaves the 8x16 cell", q)
		}
	}
}

// A double-width rune's box spans both cells.
func TestMissingGlyphBoxSpansWideCell(t *testing.T) {
	rects, _ := drawCell(t, "\U0002A6D7", 2) // CJK ext B ideograph
	if len(rects) != 4 {
		t.Fatalf("got %d rects; want 4", len(rects))
	}
	maxX := float32(0)
	for _, q := range rects {
		if q.X1 > maxX {
			maxX = q.X1
		}
	}
	if maxX <= 8 || maxX > 16 {
		t.Fatalf("box right edge at %v; want inside the second cell (8,16]", maxX)
	}
}

// A base rune followed by missing combining/invisible runes: still one
// box, not one per rune.
func TestMissingGlyphOneBoxPerCell(t *testing.T) {
	rects, _ := drawCell(t, "́️", 1)
	if len(rects) != 4 {
		t.Fatalf("got %d rects; want one box (4)", len(rects))
	}
}

// Invisible and combining runes get no box when no font has them.
func TestNoPlaceholderForInvisibleRunes(t *testing.T) {
	cases := map[string]rune{
		"zero width joiner":          0x200D,
		"zero width non-joiner":      0x200C,
		"zero width space":           0x200B,
		"word joiner":                0x2060,
		"BOM / ZWNBSP":               0xFEFF,
		"variation selector 1":       0xFE00,
		"variation selector 16":      0xFE0F,
		"variation selector 17":      0xE0100,
		"variation selector 256":     0xE01EF,
		"combining acute":            0x0301,
		"combining enclosing keycap": 0x20E3,
		"devanagari vowel sign aa":   0x093E, // spacing combining mark (Mc)
		"soft hyphen":                0x00AD,
		"left-to-right mark":         0x200E,
		"tag latin small a":          0xE0061,
		"hangul filler":              0x3164,
		"ideographic space":          0x3000,
		"no-break space":             0x00A0,
		"line separator":             0x2028,
	}
	for name, r := range cases {
		if needsPlaceholder(r) {
			t.Errorf("%s U+%04X: needsPlaceholder = true; want false", name, r)
		}
		rects, _ := drawCell(t, string(r), 1)
		if len(rects) != 0 {
			t.Errorf("%s U+%04X: drew %d rects; want none", name, r, len(rects))
		}
	}
	for _, r := range []rune{'a', 0xE0A0, 0x0600, 0x2591, 0x1F600} {
		if !needsPlaceholder(r) {
			t.Errorf("U+%04X: needsPlaceholder = false; want true", r)
		}
	}
}

// Box drawing and block elements keep their own synthesized path: the
// glyph cache is never asked for them, so they never get a box.
func TestBoxAndBlockSkipGlyphCache(t *testing.T) {
	for _, r := range []rune{0x2500, 0x2502, 0x253C, 0x2588, 0x2580, 0x2596} {
		rects, asked := drawCell(t, string(r), 1)
		if asked[r] {
			t.Errorf("U+%04X went to the glyph cache; want its own renderer path", r)
		}
		if len(rects) == 0 {
			t.Errorf("U+%04X drew nothing", r)
		}
	}
	// Full block is one cell-filling rect, not a 4-sided outline.
	if rects, _ := drawCell(t, "█", 1); len(rects) != 1 {
		t.Errorf("█ drew %d rects; want 1", len(rects))
	}
	// Shades fall through to the font; missing there, they get a box.
	rects, asked := drawCell(t, "░", 1)
	if !asked[0x2591] || len(rects) != 4 {
		t.Errorf("░ asked=%v rects=%d; want the font path and a box", asked[0x2591], len(rects))
	}
}
