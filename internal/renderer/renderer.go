package renderer

import (
	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/charmbracelet/x/ansi"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// Renderer draws the terminal cell grid using ImGui's DrawList.
type Renderer struct {
	Theme        Theme
	Metrics      CellMetrics
	Font         *imgui.Font
	FontSize     float32 // explicit font size for DrawList text (supports zoom scaling)
	OffsetX      float32
	OffsetY      float32
	BoldIsBright bool // when true, bold text uses the bright ANSI color
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
					drawList.AddTextFontPtr(
						r.Font,
						r.FontSize,
						imgui.Vec2{X: x, Y: y},
						fg,
						content,
					)
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
	Hovered        bool    // mouse is over the scrollbar thumb
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
