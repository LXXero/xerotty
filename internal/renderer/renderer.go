package renderer

import (
	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/charmbracelet/x/vt"
)

// Renderer draws the terminal cell grid using ImGui's DrawList.
type Renderer struct {
	Theme   Theme
	Metrics CellMetrics
	Font    *imgui.Font
	OffsetX float32
	OffsetY float32
}

// New creates a new renderer with the given theme and metrics.
func New(theme Theme, metrics CellMetrics, font *imgui.Font) *Renderer {
	return &Renderer{
		Theme:   theme,
		Metrics: metrics,
		Font:    font,
	}
}

// Draw renders the terminal's visible cells onto the background draw list.
func (r *Renderer) Draw(emu *vt.SafeEmulator, drawList *imgui.DrawList) {
	cols := emu.Width()
	rows := emu.Height()
	cellW := r.Metrics.Width
	cellH := r.Metrics.Height

	// Pass 1: Backgrounds with run-length encoding
	for row := 0; row < rows; row++ {
		y := r.OffsetY + float32(row)*cellH
		col := 0
		for col < cols {
			cell := emu.CellAt(col, row)
			if cell == nil {
				col++
				continue
			}

			bg := r.Theme.ResolveColor(cell.Style.Bg, false)
			if bg == r.Theme.Background {
				col++
				continue
			}

			// RLE: count consecutive cells with same bg
			runLen := 1
			for col+runLen < cols {
				next := emu.CellAt(col+runLen, row)
				if next == nil {
					break
				}
				nextBg := r.Theme.ResolveColor(next.Style.Bg, false)
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

	// Pass 2: Foreground text
	for row := 0; row < rows; row++ {
		y := r.OffsetY + float32(row)*cellH
		for col := 0; col < cols; col++ {
			cell := emu.CellAt(col, row)
			if cell == nil {
				continue
			}

			content := cell.Content
			if content == "" || content == " " {
				continue
			}

			fg := r.Theme.ResolveColor(cell.Style.Fg, true)
			x := r.OffsetX + float32(col)*cellW

			drawList.AddTextVec2(
				imgui.Vec2{X: x, Y: y},
				fg,
				content,
			)

			// Skip continuation cells for wide characters
			if cell.Width > 1 {
				col += cell.Width - 1
			}
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
