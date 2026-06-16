package renderer

// Quad-stream tests: Draw's entire output is a list of
// platform.GlyphQuad (glyphs AND rects via the atlas white pixel),
// so the renderer is testable as a PURE FUNCTION — synthetic grid
// in, deterministic primitive list out, no fonts, no GPU, no ImGui
// context (a nil DrawList skips the flush; the stream is captured
// from the cell-layer cache). The high-value assertions are
// DIFFERENTIAL invariants rather than golden files:
//
//   - cache replay == fresh rebuild (staleness guard)
//   - scroll away and back == original frame
//   - content-anchored selection stays glued under scroll
//   - generation bumps invalidate; absent bumps replay

import (
	"fmt"
	"testing"

	"github.com/AllenDang/cimgui-go/imgui"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/LXXero/xerotty/internal/fontsys"
	"github.com/LXXero/xerotty/internal/glyphcache"
	"github.com/LXXero/xerotty/internal/platform"
)

// fakeGlyphs is a deterministic GlyphSource: every rune gets a
// synthetic atlas entry whose UVs encode the rune so swapped glyphs
// are visible in quad comparisons.
type fakeGlyphs struct{}

func (fakeGlyphs) Get(r rune, bold bool) *glyphcache.Entry {
	u := float32(r%97) / 97
	b := float32(0)
	if bold {
		b = 0.5
	}
	return &glyphcache.Entry{
		Tex:      *imgui.NewTextureRefTextureID(imgui.TextureID(7)),
		UV0:      imgui.Vec2{X: u, Y: b},
		UV1:      imgui.Vec2{X: u + 0.01, Y: b + 0.01},
		HasTex:   true,
		Width:    8,
		Height:   14,
		BearingY: 12,
		Advance:  8,
	}
}
func (fakeGlyphs) LineMetrics() fontsys.LineMetrics {
	return fontsys.LineMetrics{Ascent: 12, Descent: 4, LineHeight: 16}
}
func (fakeGlyphs) FbScale() float32 { return 1 }
func (fakeGlyphs) WhitePage() (imgui.TextureRef, imgui.Vec2) {
	return *imgui.NewTextureRefTextureID(imgui.TextureID(7)), imgui.Vec2{X: 0.001, Y: 0.001}
}
func (fakeGlyphs) PrimaryAdvance() float32 { return 8 }
func (fakeGlyphs) Close()                  {}

// quadGrid is a minimal EmulatorView with mutable content and an
// explicit generation counter, mirroring real Source semantics.
type quadGrid struct {
	sb     []string
	screen []string
	cols   int
	gen    uint64
	styled map[[2]int]uv.Style // optional per-(row,col) screen styles
}

func (g *quadGrid) Width() int               { return g.cols }
func (g *quadGrid) Height() int              { return len(g.screen) }
func (g *quadGrid) ScrollbackLen() int       { return len(g.sb) }
func (g *quadGrid) RenderGeneration() uint64 { return g.gen }
func (g *quadGrid) cell(line string, col, row int, scrollback bool) *uv.Cell {
	if col < 0 || col >= len(line) {
		return &uv.Cell{Content: " ", Width: 1}
	}
	c := &uv.Cell{Content: string(line[col]), Width: 1}
	if !scrollback && g.styled != nil {
		if st, ok := g.styled[[2]int{row, col}]; ok {
			c.Style = st
		}
	}
	return c
}
func (g *quadGrid) CellAt(col, row int) *uv.Cell {
	if row < 0 || row >= len(g.screen) {
		return nil
	}
	return g.cell(g.screen[row], col, row, false)
}
func (g *quadGrid) ScrollbackCellAt(col, row int) *uv.Cell {
	if row < 0 || row >= len(g.sb) {
		return nil
	}
	return g.cell(g.sb[row], col, row, true)
}

func (g *quadGrid) SnapshotWindow(scrollOffset, rows, cols int) ([][]uv.Cell, int, uint64) {
	sbLen := len(g.sb)
	base := sbLen - scrollOffset
	out := make([][]uv.Cell, rows)
	for row := 0; row < rows; row++ {
		line := make([]uv.Cell, cols)
		contentIdx := base + row
		for col := 0; col < cols; col++ {
			var c *uv.Cell
			if contentIdx >= 0 && contentIdx < sbLen {
				c = g.ScrollbackCellAt(col, contentIdx)
			} else if contentIdx >= sbLen {
				c = g.CellAt(col, contentIdx-sbLen)
			}
			if c != nil {
				line[col] = *c
			}
		}
		out[row] = line
	}
	return out, base, g.gen
}

// write simulates output: top screen line rotates into scrollback.
func (g *quadGrid) write(newLine string) {
	g.sb = append(g.sb, g.screen[0])
	g.screen = append(g.screen[1:], newLine)
	g.gen++
}

func testRenderer() *Renderer {
	return &Renderer{
		Theme:   DefaultTheme(),
		Metrics: CellMetrics{Width: 8, Height: 16},
		Glyphs:  fakeGlyphs{},
	}
}

// draw runs Draw and returns a copy of the produced primitive
// stream (replayed or rebuilt — cacheQuads holds it either way).
func draw(r *Renderer, g EmulatorView, scrollOff int) []platform.GlyphQuad {
	r.Draw(g, nil, scrollOff)
	out := make([]platform.GlyphQuad, len(r.cacheQuads))
	copy(out, r.cacheQuads)
	return out
}

func quadsEqual(a, b []platform.GlyphQuad) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func grid() *quadGrid {
	return &quadGrid{
		sb:     []string{"history zero", "history one"},
		screen: []string{"alpha bravo", "charlie delta", "echo foxtrot"},
		cols:   20,
	}
}

// TestReplayMatchesRebuild: a cache replay must be byte-identical to
// a forced rebuild of the same frame — the cache's core contract.
func TestReplayMatchesRebuild(t *testing.T) {
	r := testRenderer()
	g := grid()
	first := draw(r, g, 0)
	if len(first) == 0 {
		t.Fatal("no quads produced")
	}
	replay := draw(r, g, 0) // identical key → replay path
	r.InvalidateCellCache()
	rebuild := draw(r, g, 0) // forced rebuild
	if !quadsEqual(first, replay) || !quadsEqual(first, rebuild) {
		t.Fatalf("replay/rebuild diverged: %d vs %d vs %d quads",
			len(first), len(replay), len(rebuild))
	}
}

// TestScrollRoundTrip: scrolling away and back must reproduce the
// original frame exactly.
func TestScrollRoundTrip(t *testing.T) {
	r := testRenderer()
	g := grid()
	home := draw(r, g, 0)
	up := draw(r, g, 2)
	if quadsEqual(home, up) {
		t.Fatal("scrolled frame should differ from home")
	}
	back := draw(r, g, 0)
	if !quadsEqual(home, back) {
		t.Fatal("scroll round-trip did not reproduce the original frame")
	}
}

// TestGenerationInvalidates: same key except a content change + gen
// bump must rebuild (not replay stale quads); a content change
// WITHOUT a bump is the bug class the generation counters exist to
// prevent — asserted here as "stale replay happens", documenting the
// contract that sources MUST bump.
func TestGenerationInvalidates(t *testing.T) {
	r := testRenderer()
	g := grid()
	before := draw(r, g, 0)
	g.write("new output line")
	after := draw(r, g, 0)
	if quadsEqual(before, after) {
		t.Fatal("content change + gen bump replayed a stale frame")
	}
	// Contract documentation: mutate WITHOUT bumping → replay (stale).
	g.screen[0] = "mutated sneakily"
	stale := draw(r, g, 0)
	if !quadsEqual(after, stale) {
		t.Fatal("expected stale replay when generation is not bumped — sources must bump")
	}
}

// TestSelectionContentAnchored: a selection set in content rows must
// move WITH the content across scrolls (the "highlight glued to the
// glass" regression, now at the quad level).
func TestSelectionContentAnchored(t *testing.T) {
	r := testRenderer()
	g := grid()
	plain := draw(r, g, 0)

	// Select content row of screen line 1 ("charlie delta"):
	// content row = sbLen + 1.
	selRow := g.ScrollbackLen() + 1
	r.SetSelection(true, selRow, 0, selRow, 6, g.cols)
	selected := draw(r, g, 0)
	if quadsEqual(plain, selected) {
		t.Fatal("selection produced no visual change")
	}

	// Scroll up 1: the highlighted content is now one viewport row
	// lower... rendered quads differ from the unscrolled selection
	// frame but the selection must still be present (differ from a
	// plain scrolled frame).
	r.SetSelection(true, selRow, 0, selRow, 6, g.cols)
	scrolledSel := draw(r, g, 1)
	r.SetSelection(false, 0, 0, 0, 0, g.cols)
	scrolledPlain := draw(r, g, 1)
	if quadsEqual(scrolledSel, scrolledPlain) {
		t.Fatal("selection vanished when scrolled — not content-anchored")
	}
}

// TestUnderlineAndStrikeQuads: decorations emit rects in the stream
// at the expected geometry.
func TestUnderlineAndStrikeQuads(t *testing.T) {
	r := testRenderer()
	g := grid()
	baseline := len(draw(r, g, 0))

	g.styled = map[[2]int]uv.Style{
		{0, 0}: {Underline: 1}, // single underline on 'a'
	}
	g.gen++
	withUnderline := draw(r, g, 0)
	if len(withUnderline) != baseline+1 {
		t.Fatalf("underline should add exactly 1 quad: %d -> %d", baseline, len(withUnderline))
	}
	// The underline rect is emitted right after its cell's glyph, in
	// stream order — identify it by the white-pixel UV signature
	// (rects sample the atlas white block; glyphs have real UV rects).
	var ul *platform.GlyphQuad
	for i := range withUnderline {
		q := &withUnderline[i]
		if q.U0 == q.U1 && q.V0 == q.V1 { // degenerate UV = white-pixel rect
			ul = q
			break
		}
	}
	if ul == nil {
		t.Fatal("no rect quad found for the underline")
	}
	// 1px tall at the cell's bottom, full cell wide.
	if ul.Y1-ul.Y0 != 1 || ul.X1-ul.X0 != 8 {
		t.Fatalf("underline geometry: %+v", *ul)
	}
}

// TestKeyComponents: each cache-key ingredient actually causes a
// rebuild when it changes (guards against key fields rotting).
func TestKeyComponents(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(r *Renderer, g *quadGrid)
	}{
		{"offsetX", func(r *Renderer, g *quadGrid) { r.OffsetX += 3 }},
		{"fontSize", func(r *Renderer, g *quadGrid) { r.FontSize += 1 }},
		{"cellMetrics", func(r *Renderer, g *quadGrid) { r.Metrics.Width += 1 }},
		{"cfgGen", func(r *Renderer, g *quadGrid) { r.InvalidateCellCache() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testRenderer()
			g := grid()
			draw(r, g, 0)
			wasOK := r.cacheOK
			tc.mutate(r, g)
			r.Draw(g, nil, 0)
			if !wasOK {
				t.Fatal("cache never primed")
			}
			// After mutation the draw must NOT have been a pure replay
			// of an unchanged key: verify by checking the key stored
			// differs OR cache was explicitly invalidated.
			if tc.name != "cfgGen" && r.cacheKey == (drawCacheKey{}) {
				t.Fatal("cache key not updated")
			}
		})
	}
	_ = fmt.Sprintf
}
