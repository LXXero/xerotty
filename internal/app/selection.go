package app

import (
	"strings"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// selectionMode controls how a held drag extends the current selection.
// iTerm2-style: the mode is set when the user enters drag (single click
// for char-precise, double click for word-snapped, triple click for
// line-snapped) and persists for the lifetime of that drag.
type selectionMode int

const (
	modeChar selectionMode = iota // single-click drag — cell-precise
	modeWord                      // double-click drag — drag end snaps to word/whitespace runs
	modeLine                      // triple-click drag — selection covers full rows
)

// selection tracks mouse-driven text selection state.
type selection struct {
	active   bool // true when a selection exists
	dragging bool // true while mouse button is held
	mode     selectionMode

	// Anchor: the original click bounds. For char mode this is a single
	// cell; for word mode it's the word/whitespace run under the click;
	// for line mode it's the full clicked row. Stays put for the entire
	// drag — the cursor end is what extends.
	anchorR1, anchorC1, anchorR2, anchorC2 int

	// Current selection bounds, ordered relative to anchor by extendDrag.
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

// startCharDrag begins a fresh single-click drag at (row, col). Mode
// is char-precise; the anchor is a single cell.
func (s *selection) startCharDrag(row, col int) {
	s.mode = modeChar
	s.anchorR1, s.anchorC1 = row, col
	s.anchorR2, s.anchorC2 = row, col
	s.startRow, s.startCol = row, col
	s.endRow, s.endCol = row, col
	s.dragging = true
	s.active = false
}

// selectWord selects the word under (col, row) using word-character rules.
// Sets mode=word, the anchor brackets the word, and active=true.
func (s *selection) selectWord(emu *vt.SafeEmulator, col, row, scrollOffset int) {
	cell := cellAtViewport(emu, col, row, scrollOffset)
	if cell == nil || !isSelWordChar(cell.Content) {
		return
	}
	startCol, endCol := wordBoundsAt(emu, col, row, scrollOffset)
	s.mode = modeWord
	s.anchorR1, s.anchorR2 = row, row
	s.anchorC1, s.anchorC2 = startCol, endCol
	s.startRow, s.startCol = row, startCol
	s.endRow, s.endCol = row, endCol
	s.active = true
	s.dragging = false
}

// selectSpace selects contiguous whitespace under (col, row).
// xfce4-terminal behavior: double-click on blank = select the blank run.
// Mode stays modeWord so drag-extend treats the run as a single unit
// and snaps to neighbouring word/whitespace runs.
func (s *selection) selectSpace(emu *vt.SafeEmulator, col, row, scrollOffset int) {
	cell := cellAtViewport(emu, col, row, scrollOffset)
	if cell != nil && isSelWordChar(cell.Content) {
		return // not blank — selectWord should handle this
	}
	startCol, endCol := wordBoundsAt(emu, col, row, scrollOffset)
	s.mode = modeWord
	s.anchorR1, s.anchorR2 = row, row
	s.anchorC1, s.anchorC2 = startCol, endCol
	s.startRow, s.startCol = row, startCol
	s.endRow, s.endCol = row, endCol
	s.active = true
	s.dragging = false
}

// selectLine selects the entire line at the given row.
// xfce4-terminal behavior: triple-click = select line.
func (s *selection) selectLine(emu *vt.SafeEmulator, row, scrollOffset int) {
	cols := emu.Width()
	s.mode = modeLine
	s.anchorR1, s.anchorR2 = row, row
	s.anchorC1, s.anchorC2 = 0, cols-1
	s.startRow, s.startCol = row, 0
	s.endRow, s.endCol = row, cols-1
	s.active = true
	s.dragging = false
}

// extendDrag updates the selection to span from the anchor to the
// cursor at (curRow, curCol). Mode controls how the cursor end snaps:
// modeChar uses the exact cell, modeWord expands to the word /
// whitespace run under the cursor, modeLine selects whole rows.
func (s *selection) extendDrag(curRow, curCol int, emu *vt.SafeEmulator, scrollOffset int) {
	cols := emu.Width()
	if curCol < 0 {
		curCol = 0
	}
	if curCol >= cols {
		curCol = cols - 1
	}

	var cursorStartCol, cursorEndCol int
	switch s.mode {
	case modeWord:
		cursorStartCol, cursorEndCol = wordBoundsAt(emu, curCol, curRow, scrollOffset)
	case modeLine:
		cursorStartCol, cursorEndCol = 0, cols-1
	default: // modeChar
		cursorStartCol, cursorEndCol = curCol, curCol
	}

	// Decide which side the cursor is on relative to the anchor and
	// build the selection so the anchor end stays put. "Inside" the
	// anchor → just keep the original anchor selection.
	switch {
	case curRow < s.anchorR1 || (curRow == s.anchorR1 && cursorEndCol < s.anchorC1):
		s.startRow, s.startCol = curRow, cursorStartCol
		s.endRow, s.endCol = s.anchorR2, s.anchorC2
	case curRow > s.anchorR2 || (curRow == s.anchorR2 && cursorStartCol > s.anchorC2):
		s.startRow, s.startCol = s.anchorR1, s.anchorC1
		s.endRow, s.endCol = curRow, cursorEndCol
	default:
		s.startRow, s.startCol = s.anchorR1, s.anchorC1
		s.endRow, s.endCol = s.anchorR2, s.anchorC2
	}

	if s.startRow != s.endRow || s.startCol != s.endCol {
		s.active = true
	}
}

func isSelWordChar(content string) bool {
	if content == "" {
		return false
	}
	r := []rune(content)[0]
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// charClass partitions cells into three buckets for word-boundary
// selection. Words and whitespace runs are treated as single tokens;
// punctuation/symbols stand alone so double-clicking '$' selects just
// the dollar sign (not '$' plus the surrounding whitespace, the way a
// 2-class word/non-word split would).
type charClass int

const (
	classWord  charClass = iota // letters, digits, underscore
	classSpace                  // whitespace (incl. empty/missing cells)
	classPunct                  // everything else — each cell stands alone
)

func cellClass(c *uv.Cell) charClass {
	if c == nil || c.Content == "" {
		return classSpace
	}
	r := []rune(c.Content)[0]
	switch {
	case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
		return classWord
	case unicode.IsSpace(r):
		return classSpace
	default:
		return classPunct
	}
}

// wordBoundsAt returns the column range [startCol, endCol] of the
// "token" at (col, row) per cellClass rules: a contiguous word run, a
// contiguous whitespace run, or a single punctuation cell. Used by
// selectWord / selectSpace for the initial pick and by extendDrag in
// modeWord.
func wordBoundsAt(emu *vt.SafeEmulator, col, row, scrollOffset int) (startCol, endCol int) {
	cols := emu.Width()
	class := cellClass(cellAtViewport(emu, col, row, scrollOffset))
	if class == classPunct {
		return col, col
	}
	startCol = col
	for startCol > 0 {
		if cellClass(cellAtViewport(emu, startCol-1, row, scrollOffset)) != class {
			break
		}
		startCol--
	}
	endCol = col
	for endCol < cols-1 {
		if cellClass(cellAtViewport(emu, endCol+1, row, scrollOffset)) != class {
			break
		}
		endCol++
	}
	return startCol, endCol
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
