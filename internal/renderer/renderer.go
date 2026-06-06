package renderer

import (
	"unicode/utf8"

	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/fontsys"
	"github.com/LXXero/xerotty/internal/glyphcache"
	"github.com/LXXero/xerotty/internal/platform"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Renderer draws the terminal cell grid using ImGui's DrawList.
type Renderer struct {
	Theme        Theme
	Metrics      CellMetrics
	Font         *imgui.Font
	FontBold     *imgui.Font // optional; falls back to Font when nil
	FontSize     float32     // explicit font size for DrawList text (supports zoom scaling)
	OffsetX      float32
	OffsetY      float32
	BoldIsBright bool // when true, bold text also uses the bright ANSI color
	// GlyphSource is what the renderer needs from the glyph cache —
	// an interface so tests can inject synthetic glyphs and capture
	// the resulting quad stream without fonts or a GPU.
	//
	// Glyphs is the per-codepoint glyph cache. When non-nil it's the
	// authoritative source for cell text glyphs and bypasses ImGui's
	// font atlas — so emoji, Nerd Font icons, and any glyph not in the
	// primary terminal font fall back via OS-provided font discovery.
	// When nil, the renderer uses the ImGui Font field instead (legacy
	// path for builds without OS font services).
	Glyphs GlyphSource

	// Selection state for the current frame, set by the app via
	// SetSelection before Draw. When active, cells inside the range
	// render with SelectionFg/SelectionBg so selected text stays
	// readable — handled inside the normal bg+fg passes rather than as
	// an opaque overlay (which buried the text). Rows are CONTENT rows
	// (scrollback-absolute — stable under scrolling); Draw records
	// selRowBase each frame so cellSelected can translate the viewport
	// row it's drawing. The range covers line-start→line-end on each
	// row (full width on interior rows).
	selActive    bool
	selR1, selC1 int
	selR2, selC2 int
	selCols      int
	selRowBase   int // content row of viewport row 0 this frame

	// quadBuf accumulates the frame's cell-layer primitives (glyph
	// quads AND rects via the atlas white pixel); one
	// platform.DrawListAddQuads call flushes them in a single cgo
	// crossing. Reused across frames.
	quadBuf []platform.GlyphQuad

	// Cell-layer cache: when nothing draw-relevant changed since the
	// last frame (same emulator, content generation, scroll,
	// selection, dims, offsets), Draw skips the entire grid walk and
	// replays cacheQuads — the previous frame's primitive list — in
	// one cgo call. This is what makes animation-only frames (lava)
	// cost ~nothing regardless of grid size: no per-cell reads, no
	// locks, no color resolution, just a native memcpy-grade replay.
	cacheKey   drawCacheKey
	cacheQuads []platform.GlyphQuad
	cacheOK    bool
	// cfgGen invalidates the cache on out-of-band visual changes
	// (theme switch, font/zoom rebuild, bold-is-bright toggle) that
	// the key fields can't see. Bumped via InvalidateCellCache.
	cfgGen uint64
}

// drawCacheKey captures everything Draw's output depends on. Two
// equal keys produce identical primitive lists by construction.
type drawCacheKey struct {
	emu                        EmulatorView
	gen                        uint64
	cfgGen                     uint64
	scrollOff                  int
	cols, rows                 int
	offX, offY                 float32
	cellW, cellH               float32
	fontSize                   float32
	selActive                  bool
	selR1, selC1, selR2, selC2 int
}

// InvalidateCellCache forces the next Draw to rebuild — call after
// theme switches, font/zoom changes, or anything visual the cache
// key can't observe.
func (r *Renderer) InvalidateCellCache() {
	r.cfgGen++
	r.cacheOK = false
}

// SetSelection records the current selection range for the next Draw.
// Pass active=false to clear it. cols is the grid width, needed to
// resolve the "to end of line" extent on the first/last selected rows.
func (r *Renderer) SetSelection(active bool, r1, c1, r2, c2, cols int) {
	r.selActive = active
	r.selR1, r.selC1 = r1, c1
	r.selR2, r.selC2 = r2, c2
	r.selCols = cols
}

// cellSelected reports whether viewport cell (col,row) is within the
// current selection, using row-major geometry (first row from its
// start col, interior rows full width, last row up to its end col).
// The selection is stored in content rows; selRowBase (set by Draw)
// maps this frame's viewport rows onto them — that's what keeps the
// highlight glued to its TEXT when the user scrolls, instead of to
// the glass.
func (r *Renderer) cellSelected(col, row int) bool {
	row += r.selRowBase
	if !r.selActive || row < r.selR1 || row > r.selR2 {
		return false
	}
	lineStart, lineEnd := 0, r.selCols-1
	if row == r.selR1 {
		lineStart = r.selC1
	}
	if row == r.selR2 {
		lineEnd = r.selC2
	}
	return col >= lineStart && col <= lineEnd
}

// New creates a new renderer with the given theme and metrics.
func New(theme Theme, metrics CellMetrics, font *imgui.Font, fontSize float32) *Renderer {
	return &Renderer{
		Theme:    theme,
		Metrics:  metrics,
		Font:     font,
		FontSize: fontSize,
	}
}

// EmulatorView is the subset of the terminal emulator API the
// renderer needs. Both *vt.SafeEmulator (in-memory only) and
// *terminal.Terminal (in-memory + disk-backed scrollback under
// "unlimited" mode) satisfy it.
// GlyphSource abstracts glyphcache.Cache for the renderer (and for
// quad-stream tests, which substitute deterministic fake glyphs).
type GlyphSource interface {
	Get(r rune, bold bool) *glyphcache.Entry
	LineMetrics() fontsys.LineMetrics
	FbScale() float32
	WhitePage() (imgui.TextureRef, imgui.Vec2)
	PrimaryAdvance() float32
	Close()
}

type EmulatorView interface {
	Width() int
	Height() int
	CellAt(col, row int) *uv.Cell
	ScrollbackLen() int
	ScrollbackCellAt(col, row int) *uv.Cell
	// RenderGeneration is a monotonic counter the source bumps on
	// every draw-relevant content change (PTY output, applied wire
	// frames, resize, scrollback churn). Equal generations promise
	// identical cell content — the cell-layer cache keys on it.
	RenderGeneration() uint64
}

// cellAt returns the cell at viewport position (col, row) accounting for scroll offset.
// When scrollOffset > 0, top rows come from the scrollback buffer.
// cellAt resolves a viewport cell through scrollback. sbLen is
// HOISTED to once-per-Draw: this function runs per cell per pass
// (~25k times/frame on a large grid), and ScrollbackLen takes a
// mutex — re-asking per cell was ~30% of total CPU under the lava
// animation (perf-verified). The value can't change mid-Draw anyway:
// publishMu/frame ordering means the grid is stable while we walk it.
func cellAt(emu EmulatorView, col, row, sbLen, scrollOffset int) *uv.Cell {
	contentIdx := sbLen - scrollOffset + row
	if contentIdx < sbLen {
		return emu.ScrollbackCellAt(col, contentIdx)
	}
	return emu.CellAt(col, contentIdx-sbLen)
}

// resolveCellColors resolves the fg/bg colors for a cell,
// handling reverse video and dim/faint attributes.
func (r *Renderer) resolveCellColors(cell *uv.Cell, col, row int) (fg, bg uint32) {
	bold := cell.Style.Attrs&uv.AttrBold != 0
	reverse := cell.Style.Attrs&uv.AttrReverse != 0
	faint := cell.Style.Attrs&uv.AttrFaint != 0

	fg = r.Theme.ResolveColor(cell.Style.Fg, true, bold && r.BoldIsBright)
	bg = r.Theme.ResolveColor(cell.Style.Bg, false, false)

	if reverse {
		fg, bg = bg, fg
		// If bg was default (nil → theme bg), use theme fg as the reversed bg
		if cell.Style.Bg == nil {
			fg = r.Theme.Background
		}
		if cell.Style.Fg == nil {
			bg = r.Theme.Foreground
		}
	}

	if faint {
		// Reduce fg alpha to ~60%
		a := (fg >> 24) & 0xFF
		a = a * 153 / 255
		fg = (fg & 0x00FFFFFF) | (a << 24)
	}

	// Selection wins over the cell's own colors so highlighted text
	// stays legible. Applied last so it overrides reverse/faint too.
	if r.cellSelected(col, row) {
		fg, bg = r.Theme.SelectionFg, r.Theme.SelectionBg
	}

	return
}

// Draw renders the terminal's visible cells onto the background draw list.
// scrollOffset is the number of lines scrolled back (0 = live view).
func (r *Renderer) Draw(emu EmulatorView, drawList *imgui.DrawList, scrollOffset int) {
	cols := emu.Width()
	rows := emu.Height()

	// Cell-layer cache: identical inputs produce identical primitive
	// lists, so replay the previous frame's in one cgo call and skip
	// the whole grid walk. Animation-only frames (lava) hit this
	// every time content is idle. Legacy no-glyphcache builds skip
	// caching (their text goes through ImGui's own font path).
	if r.Glyphs != nil {
		key := drawCacheKey{
			emu: emu, gen: emu.RenderGeneration(), cfgGen: r.cfgGen,
			scrollOff: scrollOffset, cols: cols, rows: rows,
			offX: r.OffsetX, offY: r.OffsetY,
			cellW: r.Metrics.Width, cellH: r.Metrics.Height,
			fontSize:  r.FontSize,
			selActive: r.selActive,
			selR1:     r.selR1, selC1: r.selC1, selR2: r.selR2, selC2: r.selC2,
		}
		if r.cacheOK && key == r.cacheKey {
			platform.DrawListAddQuads(drawList, r.cacheQuads)
			return
		}
		r.cacheKey = key
	}
	// Content row of the first visible viewport row — cellSelected
	// translates against this (selection rows are content-space).
	// sbLen is also threaded into every cellAt call below (hoisted —
	// see cellAt).
	sbLen := emu.ScrollbackLen()
	r.selRowBase = sbLen - scrollOffset
	cellW := r.Metrics.Width
	cellH := r.Metrics.Height

	// Pass 1: Backgrounds with run-length encoding
	for row := 0; row < rows; row++ {
		y := r.OffsetY + float32(row)*cellH
		col := 0
		for col < cols {
			cell := cellAt(emu, col, row, sbLen, scrollOffset)
			if cell == nil {
				col++
				continue
			}

			_, bg := r.resolveCellColors(cell, col, row)
			if bg == r.Theme.Background {
				col++
				continue
			}

			// RLE: count consecutive cells with same bg
			runLen := 1
			for col+runLen < cols {
				next := cellAt(emu, col+runLen, row, sbLen, scrollOffset)
				if next == nil {
					break
				}
				_, nextBg := r.resolveCellColors(next, col+runLen, row)
				if nextBg != bg {
					break
				}
				runLen++
			}

			x := r.OffsetX + float32(col)*cellW
			r.appendRect(drawList, x, y, x+float32(runLen)*cellW, y+cellH, bg)
			col += runLen
		}
	}

	// Pass 2: Foreground text and decorations
	for row := 0; row < rows; row++ {
		y := r.OffsetY + float32(row)*cellH
		for col := 0; col < cols; col++ {
			cell := cellAt(emu, col, row, sbLen, scrollOffset)
			if cell == nil {
				continue
			}

			attrs := cell.Style.Attrs
			fg, _ := r.resolveCellColors(cell, col, row)
			x := r.OffsetX + float32(col)*cellW
			w := cellW
			if cell.Width > 1 {
				w = float32(cell.Width) * cellW
			}

			// Concealed/invisible: skip text but still draw decorations
			if attrs&uv.AttrConceal == 0 {
				content := cell.Content
				if content != "" && content != " " {
					// Block elements (U+2580-U+259F) — synthesize as
					// filled rects so adjacent cells tile seamlessly.
					if r.drawBlockGlyph(content, x, y, w, cellH, fg, drawList) {
						// drawn — fall through to underline/strikethrough
					} else if r.drawBoxDrawingGlyph(content, x, y, w, cellH, fg, drawList) {
						// drawn
					} else if r.Glyphs != nil {
						bold := attrs&uv.AttrBold != 0
						r.drawGlyphFromCache(drawList, content, x, y, cellH, w, fg, bold)
					} else {
						face := r.Font
						if attrs&uv.AttrBold != 0 && r.FontBold != nil {
							face = r.FontBold
						}
						drawList.AddTextFontPtr(
							face,
							r.FontSize,
							imgui.Vec2{X: x, Y: y},
							fg,
							content,
						)
					}
				}
			}

			// Underline
			if cell.Style.Underline != ansi.NoUnderlineStyle {
				ulColor := fg
				if cell.Style.UnderlineColor != nil {
					ulColor = ColorToABGR(cell.Style.UnderlineColor)
				}
				ulY := y + cellH - 1
				r.drawUnderline(drawList, cell.Style.Underline, x, ulY, w, ulColor)
			}

			// Strikethrough
			if attrs&uv.AttrStrikethrough != 0 {
				stY := y + cellH/2
				r.appendRect(drawList, x, stY, x+w, stY+1, fg)
			}

			// Skip continuation cells for wide characters
			if cell.Width > 1 {
				col += cell.Width - 1
			}
		}
	}

	// Flush the frame's primitives in one cgo crossing, and stash a
	// copy as the replay list for identical future frames.
	platform.DrawListAddQuads(drawList, r.quadBuf)
	if r.Glyphs != nil {
		r.cacheQuads = append(r.cacheQuads[:0], r.quadBuf...)
		r.cacheOK = true
	}
	r.quadBuf = r.quadBuf[:0]
}

// drawUnderline renders the appropriate underline style.
func (r *Renderer) drawUnderline(drawList *imgui.DrawList, style ansi.Underline, x, y, w float32, color uint32) {
	// All variants emit axis-aligned quads (via appendRect) so
	// underlines live in the cacheable primitive stream with
	// everything else. 1px quads on exact pixel rows render at least
	// as crisp as the AA lines they replace; the curly is sampled
	// from the same quadratic bezier into short segments.
	switch style {
	case ansi.SingleUnderlineStyle:
		r.appendRect(drawList, x, y, x+w, y+1, color)
	case ansi.DoubleUnderlineStyle:
		r.appendRect(drawList, x, y-2, x+w, y-1, color)
		r.appendRect(drawList, x, y, x+w, y+1, color)
	case ansi.CurlyUnderlineStyle:
		mid := x + w/2
		steps := int(w / 2)
		if steps < 4 {
			steps = 4
		}
		prevX, prevY := x, y
		for i := 1; i <= steps; i++ {
			t := float32(i) / float32(steps)
			// Quadratic bezier (x,y) (mid,y-3) (x+w,y).
			it := 1 - t
			bx := it*it*x + 2*it*t*mid + t*t*(x+w)
			by := it*it*y + 2*it*t*(y-3) + t*t*y
			x0, x1 := prevX, bx
			if x1 < x0 {
				x0, x1 = x1, x0
			}
			y0, y1 := prevY, by
			if y1 < y0 {
				y0, y1 = y1, y0
			}
			r.appendRect(drawList, x0, y0, x1+1, y1+1, color)
			prevX, prevY = bx, by
		}
	case ansi.DottedUnderlineStyle:
		for dx := float32(0); dx < w; dx += 3 {
			r.appendRect(drawList, x+dx, y, x+dx+1, y+1, color)
		}
	case ansi.DashedUnderlineStyle:
		dx := float32(0)
		for dx < w {
			end := dx + 4
			if end > w {
				end = w
			}
			r.appendRect(drawList, x+dx, y, x+end, y+1, color)
			dx += 6
		}
	}
}

// DrawCursor renders the cursor at the given position.
func (r *Renderer) DrawCursor(pos struct{ X, Y int }, style string, drawList *imgui.DrawList) {
	cellW := r.Metrics.Width
	cellH := r.Metrics.Height
	x := r.OffsetX + float32(pos.X)*cellW
	y := r.OffsetY + float32(pos.Y)*cellH

	switch style {
	case "underline":
		drawList.AddRectFilled(
			imgui.Vec2{X: x, Y: y + cellH - 2},
			imgui.Vec2{X: x + cellW, Y: y + cellH},
			r.Theme.Cursor,
		)
	case "bar":
		drawList.AddRectFilled(
			imgui.Vec2{X: x, Y: y},
			imgui.Vec2{X: x + 2, Y: y + cellH},
			r.Theme.Cursor,
		)
	default: // "block"
		drawList.AddRectFilledV(
			imgui.Vec2{X: x, Y: y},
			imgui.Vec2{X: x + cellW, Y: y + cellH},
			r.Theme.Cursor, 0, 0,
		)
	}
}

// ScrollbarParams configures scrollbar rendering.
type ScrollbarParams struct {
	X              float32 // left edge of scrollbar track
	Y              float32 // top of scrollbar area
	Width          float32
	Height         float32 // total height of scrollbar area
	ScrollOffset   int     // current scroll offset (lines from bottom)
	TotalLines     int     // total scrollback + visible lines
	VisibleLines   int     // number of visible rows
	MinThumbHeight float32
	Hovered        bool // mouse is over the scrollbar thumb
}

// DrawScrollbar renders a scrollbar track and thumb.
// Returns (thumbY, thumbH) for hit-testing.
func (r *Renderer) DrawScrollbar(p ScrollbarParams, drawList *imgui.DrawList) (float32, float32) {
	// Track background
	drawList.AddRectFilled(
		imgui.Vec2{X: p.X, Y: p.Y},
		imgui.Vec2{X: p.X + p.Width, Y: p.Y + p.Height},
		r.Theme.ScrollbarBg,
	)

	if p.TotalLines <= p.VisibleLines {
		return 0, 0
	}

	// Thumb proportional size
	ratio := float32(p.VisibleLines) / float32(p.TotalLines)
	thumbH := p.Height * ratio
	if thumbH < p.MinThumbHeight {
		thumbH = p.MinThumbHeight
	}

	// Thumb position: scrollOffset=0 → thumb at bottom, scrollOffset=max → thumb at top
	maxOffset := p.TotalLines - p.VisibleLines
	scrollFrac := float32(p.ScrollOffset) / float32(maxOffset)
	trackSpace := p.Height - thumbH
	thumbY := p.Y + trackSpace*(1.0-scrollFrac)

	thumbColor := r.Theme.ScrollbarThumb
	if p.Hovered {
		thumbColor = r.Theme.ScrollbarHover
	}
	drawList.AddRectFilledV(
		imgui.Vec2{X: p.X, Y: thumbY},
		imgui.Vec2{X: p.X + p.Width, Y: thumbY + thumbH},
		thumbColor, p.Width/2, 0,
	)

	return thumbY, thumbH
}

// drawGlyphFromCache renders a cell's text content using the OS-backed
// glyph cache. content may be a single rune or a multi-byte UTF-8
// sequence (combining marks, ZWJ sequences). For combining sequences
// we draw each rune at the same cell origin so they overlay; the cell
// width is governed by the base rune's advance.
//
// Glyph bitmaps are stored at framebuffer-pixel resolution (e.g. 2x
// on Retina). To preserve crisp rasterization through GPU sampling,
// we snap the quad's framebuffer-space corners to integer pixels —
// otherwise bilinear filtering between texels softens the glyph.
func (r *Renderer) drawGlyphFromCache(drawList *imgui.DrawList, content string, x, y, cellH, cellW float32, fg uint32, bold bool) {
	scale := r.Glyphs.FbScale()
	if scale <= 0 {
		scale = 1
	}
	ascent := r.Glyphs.LineMetrics().Ascent
	for _, rn := range content {
		entry := r.Glyphs.Get(rn, bold)
		if entry == nil || !entry.HasTex {
			continue
		}
		// Compute the framebuffer-pixel position of the glyph's top-left
		// (CoreText bearing values are already in framebuffer pixels
		// because we rasterized at pxSize*fbScale), then convert back
		// to logical units. Floor-snapping aligns texels 1:1 with
		// framebuffer pixels so the GPU sampler hits whole texels.
		fbX := floor32(x*scale) + float32(entry.BearingX)
		fbY := floor32((y+ascent)*scale) - float32(entry.BearingY)
		px := fbX / scale
		py := fbY / scale
		w := float32(entry.Width) / scale
		h := float32(entry.Height) / scale
		tint := fg
		if entry.IsColor {
			tint = 0xFFFFFFFF
			px, py, w, h = fitColorGlyphToCell(x, y, cellW, cellH, w, h, scale)
		}
		// Accumulate — flushed in one cgo call at the end of the text
		// pass (see Draw). Z-order note: glyphs land above same-pass
		// underlines/strikethroughs, which is the conventional layering.
		r.quadBuf = append(r.quadBuf, platform.GlyphQuad{
			X0: px, Y0: py, X1: px + w, Y1: py + h,
			U0: entry.UV0.X, V0: entry.UV0.Y, U1: entry.UV1.X, V1: entry.UV1.Y,
			Col: tint, Tex: uint64(entry.Tex.TexID()),
		})
	}
}

// appendRect queues an untextured rectangle into the frame's quad
// stream using the atlas white pixel, so cell backgrounds and
// decorations batch (and cache) together with glyphs. Falls back to
// an immediate AddRectFilled on the legacy no-glyphcache path.
func (r *Renderer) appendRect(drawList *imgui.DrawList, x0, y0, x1, y1 float32, col uint32) {
	if r.Glyphs == nil {
		drawList.AddRectFilled(imgui.Vec2{X: x0, Y: y0}, imgui.Vec2{X: x1, Y: y1}, col)
		return
	}
	tex, uv := r.Glyphs.WhitePage()
	r.quadBuf = append(r.quadBuf, platform.GlyphQuad{
		X0: x0, Y0: y0, X1: x1, Y1: y1,
		U0: uv.X, V0: uv.Y, U1: uv.X, V1: uv.Y,
		Col: col, Tex: uint64(tex.TexID()),
	})
}

func fitColorGlyphToCell(x, y, cellW, cellH, glyphW, glyphH, scale float32) (float32, float32, float32, float32) {
	if glyphW <= 0 || glyphH <= 0 || cellW <= 0 || cellH <= 0 {
		return x, y, glyphW, glyphH
	}
	const colorGlyphScale = 1.16
	targetH := cellH * colorGlyphScale
	targetW := targetH * glyphW / glyphH
	if cellW > cellH && targetW > cellW {
		targetW = cellW
		targetH = targetW * glyphH / glyphW
	}
	px := x + (cellW-targetW)/2
	py := y + (cellH-targetH)/2
	if scale <= 0 {
		scale = 1
	}
	px = floor32(px*scale) / scale
	py = floor32(py*scale) / scale
	targetW = round32(targetW*scale) / scale
	targetH = round32(targetH*scale) / scale
	return px, py, targetW, targetH
}

func round32(v float32) float32 {
	if v >= 0 {
		return float32(int32(v + 0.5))
	}
	return float32(int32(v - 0.5))
}

func floor32(v float32) float32 {
	if v >= 0 {
		return float32(int32(v))
	}
	return float32(int32(v - 1))
}

// drawBlockGlyph synthesizes Unicode block-element characters (U+2580-U+259F)
// as filled rectangles aligned to the cell instead of rendering the font
// glyph. Font glyphs for block elements are typically rasterized a fraction
// of a pixel narrower or shorter than the cell, producing visible gaps in
// pixel-art that's meant to tile seamlessly (e.g. claude's headcrab logo,
// progress bars, sparklines).
//
// Returns true if content matched a block element and was drawn. Shaded
// blocks (U+2591-U+2593) fall through to font rendering — they're stipple
// patterns that don't have a clean rectangle representation.
func (rd *Renderer) drawBlockGlyph(content string, x, y, w, h float32, fg uint32, drawList *imgui.DrawList) bool {
	r, size := utf8.DecodeRuneInString(content)
	if size == 0 || r < 0x2580 || r > 0x259F {
		return false
	}
	fillRect := func(x1, y1, x2, y2 float32) {
		rd.appendRect(drawList, x+x1, y+y1, x+x2, y+y2, fg)
	}
	switch r {
	case 0x2580: // ▀ UPPER HALF BLOCK
		fillRect(0, 0, w, h/2)
	case 0x2581: // ▁ LOWER ONE EIGHTH
		fillRect(0, 7*h/8, w, h)
	case 0x2582: // ▂ LOWER ONE QUARTER
		fillRect(0, 6*h/8, w, h)
	case 0x2583: // ▃ LOWER THREE EIGHTHS
		fillRect(0, 5*h/8, w, h)
	case 0x2584: // ▄ LOWER HALF
		fillRect(0, h/2, w, h)
	case 0x2585: // ▅ LOWER FIVE EIGHTHS
		fillRect(0, 3*h/8, w, h)
	case 0x2586: // ▆ LOWER THREE QUARTERS
		fillRect(0, 2*h/8, w, h)
	case 0x2587: // ▇ LOWER SEVEN EIGHTHS
		fillRect(0, h/8, w, h)
	case 0x2588: // █ FULL BLOCK
		fillRect(0, 0, w, h)
	case 0x2589: // ▉ LEFT SEVEN EIGHTHS
		fillRect(0, 0, 7*w/8, h)
	case 0x258A: // ▊ LEFT THREE QUARTERS
		fillRect(0, 0, 6*w/8, h)
	case 0x258B: // ▋ LEFT FIVE EIGHTHS
		fillRect(0, 0, 5*w/8, h)
	case 0x258C: // ▌ LEFT HALF
		fillRect(0, 0, w/2, h)
	case 0x258D: // ▍ LEFT THREE EIGHTHS
		fillRect(0, 0, 3*w/8, h)
	case 0x258E: // ▎ LEFT ONE QUARTER
		fillRect(0, 0, 2*w/8, h)
	case 0x258F: // ▏ LEFT ONE EIGHTH
		fillRect(0, 0, w/8, h)
	case 0x2590: // ▐ RIGHT HALF
		fillRect(w/2, 0, w, h)
	case 0x2591, 0x2592, 0x2593: // ░ ▒ ▓ shade — let the font handle it
		// iTerm2 / Terminal.app / kitty / alacritty all let the font
		// render shade blocks via CoreText (or FreeType on Linux).
		// Monaco's stipple pattern at typical cell sizes AA-blends to
		// look like a textured fill, which most users expect for
		// progress bars. An alpha-modulated solid rect would be a
		// flat color instead — visually different in a way that
		// confuses tools that pick shades for their texture.
		return false
	case 0x2594: // ▔ UPPER ONE EIGHTH
		fillRect(0, 0, w, h/8)
	case 0x2595: // ▕ RIGHT ONE EIGHTH
		fillRect(7*w/8, 0, w, h)
	case 0x2596: // ▖ QUADRANT LOWER LEFT
		fillRect(0, h/2, w/2, h)
	case 0x2597: // ▗ QUADRANT LOWER RIGHT
		fillRect(w/2, h/2, w, h)
	case 0x2598: // ▘ QUADRANT UPPER LEFT
		fillRect(0, 0, w/2, h/2)
	case 0x2599: // ▙ QUADRANT UPPER LEFT + LOWER LEFT + LOWER RIGHT
		fillRect(0, 0, w/2, h/2)
		fillRect(0, h/2, w, h)
	case 0x259A: // ▚ QUADRANT UPPER LEFT + LOWER RIGHT
		fillRect(0, 0, w/2, h/2)
		fillRect(w/2, h/2, w, h)
	case 0x259B: // ▛ QUADRANT UPPER LEFT + UPPER RIGHT + LOWER LEFT
		fillRect(0, 0, w, h/2)
		fillRect(0, h/2, w/2, h)
	case 0x259C: // ▜ QUADRANT UPPER LEFT + UPPER RIGHT + LOWER RIGHT
		fillRect(0, 0, w, h/2)
		fillRect(w/2, h/2, w, h)
	case 0x259D: // ▝ QUADRANT UPPER RIGHT
		fillRect(w/2, 0, w, h/2)
	case 0x259E: // ▞ QUADRANT UPPER RIGHT + LOWER LEFT
		fillRect(w/2, 0, w, h/2)
		fillRect(0, h/2, w/2, h)
	case 0x259F: // ▟ QUADRANT UPPER RIGHT + LOWER LEFT + LOWER RIGHT
		fillRect(w/2, 0, w, h/2)
		fillRect(0, h/2, w, h)
	default:
		return false
	}
	return true
}

// drawBoxDrawingGlyph synthesizes Unicode box-drawing characters
// (U+2500-U+257F) as filled rectangles aligned to the cell. Same
// motivation as drawBlockGlyph: the font's glyph for ─ rasterizes a
// hair narrower than the cell, leaving 1-pixel gaps between adjacent
// cells that read as a dashed line. Synthesizing as cell-aligned rects
// gives perfectly continuous lines.
//
// Returns true if the rune was recognized and drawn. Dashed variants
// (U+2504-U+250B, U+254C-U+254F, U+2550-U+2551 are not dashed but the
// ┄ ┅ ┆ ┇ ┈ ┉ ┊ ┋ etc. range is) and diagonal lines (U+2571-U+2573)
// fall through to the font.
func (rd *Renderer) drawBoxDrawingGlyph(content string, x, y, w, h float32, fg uint32, drawList *imgui.DrawList) bool {
	r, size := utf8.DecodeRuneInString(content)
	if size == 0 || r < 0x2500 || r > 0x257F {
		return false
	}
	top, right, bottom, left, ok := boxArms(r)
	if !ok {
		return false
	}

	// Stroke widths in logical pixels. light is ~1/10 of cell height
	// rounded up, heavy is double. Double is two parallel lights with
	// a gap of 1 light between them, so its outer span equals heavy.
	light := h / 10
	if light < 1 {
		light = 1
	}
	heavy := light * 2

	cx := x + w/2
	cy := y + h/2

	fill := func(x1, y1, x2, y2 float32) {
		rd.appendRect(drawList, x1, y1, x2, y2, fg)
	}

	// Span of one arm in the perpendicular axis.
	thickness := func(a boxArm) float32 {
		switch a {
		case armLight:
			return light
		case armHeavy:
			return heavy
		case armDouble:
			return heavy + light // two lights + gap of one light
		}
		return 0
	}

	// drawHorizArm draws the right arm if dir>0, left if dir<0. The arm
	// extends from cx (joint center) to the cell edge in that direction
	// at the correct vertical position(s) for the arm's weight.
	drawHorizArm := func(a boxArm, dir float32) {
		if a == armNone {
			return
		}
		var x1, x2 float32
		if dir > 0 {
			x1 = cx
			x2 = x + w
		} else {
			x1 = x
			x2 = cx
		}
		switch a {
		case armLight:
			fill(x1, cy-light/2, x2, cy+light/2-(light-floor32(light)))
		case armHeavy:
			fill(x1, cy-heavy/2, x2, cy+heavy/2)
		case armDouble:
			// Two parallel lights with a gap.
			top := cy - thickness(a)/2
			fill(x1, top, x2, top+light)
			fill(x1, top+2*light, x2, top+3*light)
		}
	}
	drawVertArm := func(a boxArm, dir float32) {
		if a == armNone {
			return
		}
		var y1, y2 float32
		if dir > 0 {
			y1 = cy
			y2 = y + h
		} else {
			y1 = y
			y2 = cy
		}
		switch a {
		case armLight:
			fill(cx-light/2, y1, cx+light/2-(light-floor32(light)), y2)
		case armHeavy:
			fill(cx-heavy/2, y1, cx+heavy/2, y2)
		case armDouble:
			leftEdge := cx - thickness(a)/2
			fill(leftEdge, y1, leftEdge+light, y2)
			fill(leftEdge+2*light, y1, leftEdge+3*light, y2)
		}
	}

	drawHorizArm(left, -1)
	drawHorizArm(right, +1)
	drawVertArm(top, -1)
	drawVertArm(bottom, +1)
	return true
}

// boxArm encodes one of the four arms (top/right/bottom/left) of a box-
// drawing glyph. None means the glyph has no arm in that direction.
type boxArm uint8

const (
	armNone boxArm = iota
	armLight
	armHeavy
	armDouble
)

// boxArms returns (top, right, bottom, left) arms for a box-drawing
// codepoint plus an ok flag. Rounded corners (U+256D-U+2570) are
// treated as their square equivalents — at terminal cell sizes the
// curve is barely visible and a square corner tiles cleanly. Dashed
// variants (U+2504-U+250B etc.) and diagonals (U+2571-U+2573) return
// ok=false so the caller falls back to the font (the dashes are
// intentional, the diagonals don't decompose into orthogonal arms).
func boxArms(r rune) (top, right, bottom, left boxArm, ok bool) {
	switch r {
	// Light horizontal / vertical
	case 0x2500:
		return armNone, armLight, armNone, armLight, true
	case 0x2502:
		return armLight, armNone, armLight, armNone, true
	// Heavy horizontal / vertical
	case 0x2501:
		return armNone, armHeavy, armNone, armHeavy, true
	case 0x2503:
		return armHeavy, armNone, armHeavy, armNone, true
	// Light corners (┌ ┐ └ ┘) and rounded equivalents (╭ ╮ ╯ ╰)
	case 0x250C, 0x256D:
		return armNone, armLight, armLight, armNone, true
	case 0x2510, 0x256E:
		return armNone, armNone, armLight, armLight, true
	case 0x2514, 0x2570:
		return armLight, armLight, armNone, armNone, true
	case 0x2518, 0x256F:
		return armLight, armNone, armNone, armLight, true
	// Mixed-weight corners — pick the heavier weight to keep tiling clean.
	case 0x250D:
		return armNone, armHeavy, armLight, armNone, true
	case 0x250E:
		return armNone, armLight, armHeavy, armNone, true
	case 0x250F:
		return armNone, armHeavy, armHeavy, armNone, true
	case 0x2511:
		return armNone, armNone, armLight, armHeavy, true
	case 0x2512:
		return armNone, armNone, armHeavy, armLight, true
	case 0x2513:
		return armNone, armNone, armHeavy, armHeavy, true
	case 0x2515:
		return armLight, armHeavy, armNone, armNone, true
	case 0x2516:
		return armHeavy, armLight, armNone, armNone, true
	case 0x2517:
		return armHeavy, armHeavy, armNone, armNone, true
	case 0x2519:
		return armLight, armNone, armNone, armHeavy, true
	case 0x251A:
		return armHeavy, armNone, armNone, armLight, true
	case 0x251B:
		return armHeavy, armNone, armNone, armHeavy, true
	// T junctions ├ ┤ ┬ ┴ ┼ (light)
	case 0x251C:
		return armLight, armLight, armLight, armNone, true
	case 0x2524:
		return armLight, armNone, armLight, armLight, true
	case 0x252C:
		return armNone, armLight, armLight, armLight, true
	case 0x2534:
		return armLight, armLight, armNone, armLight, true
	case 0x253C:
		return armLight, armLight, armLight, armLight, true
	// Heavy T junctions ┣ ┫ ┳ ┻ ╋
	case 0x2523:
		return armHeavy, armHeavy, armHeavy, armNone, true
	case 0x252B:
		return armHeavy, armNone, armHeavy, armHeavy, true
	case 0x2533:
		return armNone, armHeavy, armHeavy, armHeavy, true
	case 0x253B:
		return armHeavy, armHeavy, armNone, armHeavy, true
	case 0x254B:
		return armHeavy, armHeavy, armHeavy, armHeavy, true
	// Double box (═ ║ ╔ ╗ ╚ ╝ ╠ ╣ ╦ ╩ ╬)
	case 0x2550:
		return armNone, armDouble, armNone, armDouble, true
	case 0x2551:
		return armDouble, armNone, armDouble, armNone, true
	case 0x2554:
		return armNone, armDouble, armDouble, armNone, true
	case 0x2557:
		return armNone, armNone, armDouble, armDouble, true
	case 0x255A:
		return armDouble, armDouble, armNone, armNone, true
	case 0x255D:
		return armDouble, armNone, armNone, armDouble, true
	case 0x2560:
		return armDouble, armDouble, armDouble, armNone, true
	case 0x2563:
		return armDouble, armNone, armDouble, armDouble, true
	case 0x2566:
		return armNone, armDouble, armDouble, armDouble, true
	case 0x2569:
		return armDouble, armDouble, armNone, armDouble, true
	case 0x256C:
		return armDouble, armDouble, armDouble, armDouble, true
	// Half-lines (╴╵╶╷ light, ╸╹╺╻ heavy)
	case 0x2574:
		return armNone, armNone, armNone, armLight, true
	case 0x2575:
		return armLight, armNone, armNone, armNone, true
	case 0x2576:
		return armNone, armLight, armNone, armNone, true
	case 0x2577:
		return armNone, armNone, armLight, armNone, true
	case 0x2578:
		return armNone, armNone, armNone, armHeavy, true
	case 0x2579:
		return armHeavy, armNone, armNone, armNone, true
	case 0x257A:
		return armNone, armHeavy, armNone, armNone, true
	case 0x257B:
		return armNone, armNone, armHeavy, armNone, true
	}
	return armNone, armNone, armNone, armNone, false
}
