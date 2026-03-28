package app

import (
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// selection tracks mouse-driven text selection state.
type selection struct {
	active   bool // true when a selection exists
	dragging bool // true while mouse button is held

	// Start/end in viewport cell coordinates.
	// These are the raw mouse positions — normalize() orders them.
	startCol int
	startRow int
	endCol   int
	endRow   int
}

// clear resets the selection state.
func (s *selection) clear() {
	s.active = false
	s.dragging = false
}

// normalize returns the selection bounds ordered top-left to bottom-right.
func (s *selection) normalize() (r1, c1, r2, c2 int) {
	r1, c1 = s.startRow, s.startCol
	r2, c2 = s.endRow, s.endCol
	if r1 > r2 || (r1 == r2 && c1 > c2) {
		r1, c1, r2, c2 = r2, c2, r1, c1
	}
	return
}

// contains returns true if the cell at (col, row) is within the selection.
func (s *selection) contains(col, row int) bool {
	if !s.active {
		return false
	}
	r1, c1, r2, c2 := s.normalize()
	if row < r1 || row > r2 {
		return false
	}
	if row == r1 && row == r2 {
		return col >= c1 && col <= c2
	}
	if row == r1 {
		return col >= c1
	}
	if row == r2 {
		return col <= c2
	}
	return true // middle rows are fully selected
}

// extractText reads the selected cell contents from the emulator.
// scrollOffset is the current scroll position (used with cellAt helper).
func (s *selection) extractText(emu *vt.SafeEmulator, scrollOffset int) string {
	if !s.active {
		return ""
	}
	r1, c1, r2, c2 := s.normalize()
	cols := emu.Width()

	var b strings.Builder
	for row := r1; row <= r2; row++ {
		lineStart := 0
		lineEnd := cols - 1
		if row == r1 {
			lineStart = c1
		}
		if row == r2 {
			lineEnd = c2
		}

		var line strings.Builder
		for col := lineStart; col <= lineEnd; col++ {
			cell := cellAtViewport(emu, col, row, scrollOffset)
			if cell == nil {
				line.WriteByte(' ')
				continue
			}
			content := cell.Content
			if content == "" {
				content = " "
			}
			line.WriteString(content)
			// Skip continuation cells for wide chars
			if cell.Width > 1 {
				col += cell.Width - 1
			}
		}

		text := strings.TrimRight(line.String(), " ")
		b.WriteString(text)
		if row < r2 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// cellAtViewport returns the cell at viewport (col, row) accounting for scroll.
// This mirrors renderer.cellAt but is accessible from the app package.
func cellAtViewport(emu *vt.SafeEmulator, col, row, scrollOffset int) *uv.Cell {
	sbLen := emu.ScrollbackLen()
	contentIdx := sbLen - scrollOffset + row
	if contentIdx < sbLen {
		return emu.ScrollbackCellAt(col, contentIdx)
	}
	return emu.CellAt(col, contentIdx-sbLen)
}
