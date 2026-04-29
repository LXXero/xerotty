package renderer

import (
	"unicode/utf8"

	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/glyphcache"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
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
	// Glyphs is the per-codepoint glyph cache. When non-nil it's the
	// authoritative source for cell text glyphs and bypasses ImGui's
	// font atlas — so emoji, Nerd Font icons, and any glyph not in the
	// primary terminal font fall back via OS-provided font discovery.
	// When nil, the renderer uses the ImGui Font field instead (legacy
	// path for builds without OS font services).
	Glyphs *glyphcache.Cache
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

// cellAt returns the cell at viewport position (col, row) accounting for scroll offset.
// When scrollOffset > 0, top rows come from the scrollback buffer.
func cellAt(emu *vt.SafeEmulator, col, row, scrollOffset int) *uv.Cell {
	sbLen := emu.ScrollbackLen()
	contentIdx := sbLen - scrollOffset + row
	if contentIdx < sbLen {
		return emu.ScrollbackCellAt(col, contentIdx)
	}
	return emu.CellAt(col, contentIdx-sbLen)
}

// resolveCellColors resolves the fg/bg colors for a cell,
// handling reverse video and dim/faint attributes.
func (r *Renderer) resolveCellColors(cell *uv.Cell) (fg, bg uint32) {
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

	return
}

// Draw renders the terminal's visible cells onto the background draw list.
// scrollOffset is the number of lines scrolled back (0 = live view).
func (r *Renderer) Draw(emu *vt.SafeEmulator, drawList *imgui.DrawList, scrollOffset int) {
	cols := emu.Width()
	rows := emu.Height()
	cellW := r.Metrics.Width
	cellH := r.Metrics.Height

	// Pass 1: Backgrounds with run-length encoding
	for row := 0; row < rows; row++ {
		y := r.OffsetY + float32(row)*cellH
		col := 0
		for col < cols {
			cell := cellAt(emu, col, row, scrollOffset)
			if cell == nil {
				col++
				continue
			}

			_, bg := r.resolveCellColors(cell)
			if bg == r.Theme.Background {
				col++
				continue
			}

			// RLE: count consecutive cells with same bg
			runLen := 1
			for col+runLen < cols {
				next := cellAt(emu, col+runLen, row, scrollOffset)
				if next == nil {
					break
				}
				_, nextBg := r.resolveCellColors(next)
				if nextBg != bg {
					break
				}
				runLen++
			}

			x := r.OffsetX + float32(col)*cellW
			drawList.AddRectFilled(
				imgui.Vec2{X: x, Y: y},
				imgui.Vec2{X: x + float32(runLen)*cellW, Y: y + cellH},
				bg,
			)
			col += runLen
		}
	}

	// Pass 2: Foreground text and decorations
	for row := 0; row < rows; row++ {
		y := r.OffsetY + float32(row)*cellH
		for col := 0; col < cols; col++ {
			cell := cellAt(emu, col, row, scrollOffset)
			if cell == nil {
				continue
			}

			attrs := cell.Style.Attrs
			fg, _ := r.resolveCellColors(cell)
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
					if drawBlockGlyph(content, x, y, w, cellH, fg, drawList) {
						// drawn — fall through to underline/strikethrough
					} else if drawBoxDrawingGlyph(content, x, y, w, cellH, fg, drawList) {
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
				drawList.AddLineV(
					imgui.Vec2{X: x, Y: stY},
					imgui.Vec2{X: x + w, Y: stY},
					fg, 1.0,
				)
			}

			// Skip continuation cells for wide characters
			if cell.Width > 1 {
				col += cell.Width - 1
			}
		}
	}
}

// drawUnderline renders the appropriate underline style.
func (r *Renderer) drawUnderline(drawList *imgui.DrawList, style ansi.Underline, x, y, w float32, color uint32) {
	switch style {
	case ansi.SingleUnderlineStyle:
		drawList.AddLineV(
			imgui.Vec2{X: x, Y: y},
			imgui.Vec2{X: x + w, Y: y},
			color, 1.0,
		)
	case ansi.DoubleUnderlineStyle:
		drawList.AddLineV(
			imgui.Vec2{X: x, Y: y - 2},
			imgui.Vec2{X: x + w, Y: y - 2},
			color, 1.0,
		)
		drawList.AddLineV(
			imgui.Vec2{X: x, Y: y},
			imgui.Vec2{X: x + w, Y: y},
			color, 1.0,
		)
	case ansi.CurlyUnderlineStyle:
		mid := x + w/2
		drawList.AddBezierQuadraticV(
			imgui.Vec2{X: x, Y: y},
			imgui.Vec2{X: mid, Y: y - 3},
			imgui.Vec2{X: x + w, Y: y},
			color, 1.0, 0,
		)
	case ansi.DottedUnderlineStyle:
		for dx := float32(0); dx < w; dx += 3 {
			drawList.AddRectFilled(
				imgui.Vec2{X: x + dx, Y: y},
				imgui.Vec2{X: x + dx + 1, Y: y + 1},
				color,
			)
		}
	case ansi.DashedUnderlineStyle:
		dx := float32(0)
		for dx < w {
			end := dx + 4
			if end > w {
				end = w
			}
			drawList.AddLineV(
				imgui.Vec2{X: x + dx, Y: y},
				imgui.Vec2{X: x + end, Y: y},
				color, 1.0,
			)
			dx += 6
		}
	}
}

// SelectionBounds describes a selection range in viewport cell coordinates.
type SelectionBounds struct {
	Active   bool
	StartRow int
	StartCol int
	EndRow   int
	EndCol   int
}

// DrawSelection renders the selection highlight overlay.
// Bounds should already be normalized (start <= end).
func (r *Renderer) DrawSelection(bounds SelectionBounds, cols, rows int, drawList *imgui.DrawList) {
	if !bounds.Active {
		return
	}
	cellW := r.Metrics.Width
	cellH := r.Metrics.Height

	r1, c1 := bounds.StartRow, bounds.StartCol
	r2, c2 := bounds.EndRow, bounds.EndCol

	for row := r1; row <= r2; row++ {
		if row < 0 || row >= rows {
			continue
		}
		lineStart := 0
		lineEnd := cols - 1
		if row == r1 {
			lineStart = c1
		}
		if row == r2 {
			lineEnd = c2
		}

		x := r.OffsetX + float32(lineStart)*cellW
		y := r.OffsetY + float32(row)*cellH
		w := float32(lineEnd-lineStart+1) * cellW

		drawList.AddRectFilled(
			imgui.Vec2{X: x, Y: y},
			imgui.Vec2{X: x + w, Y: y + cellH},
			r.Theme.SelectionBg,
		)
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
		drawList.AddImageV(
			entry.Tex,
			imgui.Vec2{X: px, Y: py},
			imgui.Vec2{X: px + w, Y: py + h},
			imgui.Vec2{X: 0, Y: 0},
			imgui.Vec2{X: 1, Y: 1},
			tint,
		)
	}
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
func drawBlockGlyph(content string, x, y, w, h float32, fg uint32, drawList *imgui.DrawList) bool {
	r, size := utf8.DecodeRuneInString(content)
	if size == 0 || r < 0x2580 || r > 0x259F {
		return false
	}
	fillRect := func(x1, y1, x2, y2 float32) {
		drawList.AddRectFilled(
			imgui.Vec2{X: x + x1, Y: y + y1},
			imgui.Vec2{X: x + x2, Y: y + y2},
			fg,
		)
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
func drawBoxDrawingGlyph(content string, x, y, w, h float32, fg uint32, drawList *imgui.DrawList) bool {
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
		drawList.AddRectFilled(
			imgui.Vec2{X: x1, Y: y1},
			imgui.Vec2{X: x2, Y: y2},
			fg,
		)
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
