package app

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

// fakeGrid is a minimal cellGrid: a scrollback of lines plus a
// visible screen, both as plain strings.
type fakeGrid struct {
	sb     []string
	screen []string
	cols   int
}

func (f *fakeGrid) Width() int  { return f.cols }
func (f *fakeGrid) Height() int { return len(f.screen) }
func (f *fakeGrid) ScrollbackLen() int {
	return len(f.sb)
}
func cellOf(line string, col int) *uv.Cell {
	if col < 0 || col >= len(line) {
		return nil
	}
	return &uv.Cell{Content: string(line[col]), Width: 1}
}
func (f *fakeGrid) CellAt(col, row int) *uv.Cell {
	if row < 0 || row >= len(f.screen) {
		return nil
	}
	return cellOf(f.screen[row], col)
}
func (f *fakeGrid) ScrollbackCellAt(col, row int) *uv.Cell {
	if row < 0 || row >= len(f.sb) {
		return nil
	}
	return cellOf(f.sb[row], col)
}

// scrollOneLine simulates the terminal scrolling: the top screen line
// moves into scrollback, a new line appears at the bottom.
func (f *fakeGrid) scrollOneLine(newLine string) {
	f.sb = append(f.sb, f.screen[0])
	f.screen = append(f.screen[1:], newLine)
}

// TestSelectionStableUnderScrollAndOutput is the "highlight doesn't
// move when I scroll" regression: a selection anchored in content
// rows must keep extracting the SAME text after the user scrolls
// (which no longer even reaches extractText) and after new output
// pushes lines into scrollback.
func TestSelectionStableUnderScrollAndOutput(t *testing.T) {
	g := &fakeGrid{
		sb:     []string{"old history line"},
		screen: []string{"alpha bravo", "charlie delta", "echo foxtrot"},
		cols:   20,
	}

	var sel selection
	// User double-clicks "charlie" on viewport row 1 with no scroll:
	// content row = sbLen(1) + 1 = 2.
	cRow := contentRowAt(g, 1, 0)
	if cRow != 2 {
		t.Fatalf("content row = %d, want 2", cRow)
	}
	sel.selectWord(g, 2, cRow)
	if !sel.active {
		t.Fatal("selection not active")
	}
	if got := sel.extractText(g, true); got != "charlie" {
		t.Fatalf("initial extract = %q", got)
	}

	// New output scrolls two lines into history. The selected text's
	// content row is unchanged by construction — extract must still
	// say "charlie", NOT whatever now occupies viewport row 1.
	g.scrollOneLine("new output 1")
	g.scrollOneLine("new output 2")
	if got := sel.extractText(g, true); got != "charlie" {
		t.Fatalf("after output, extract = %q, want charlie", got)
	}

	// And scrolling the VIEW (any offset) doesn't touch extraction at
	// all anymore — there's no offset parameter to get wrong. The
	// renderer side maps content→viewport via selRowBase instead.
	// Simulate that mapping: with scrollOffset 2, viewport row 0 shows
	// content row sbLen-2.
	base := g.ScrollbackLen() - 2
	r1, _, r2, _ := sel.normalize()
	onScreenRow := -1
	for vp := 0; vp < g.Height(); vp++ {
		if base+vp >= r1 && base+vp <= r2 {
			onScreenRow = vp
		}
	}
	if onScreenRow == -1 {
		t.Fatal("selection should be visible at offset 2")
	}
	if line := g.sb[2]; line != "charlie delta" {
		t.Fatalf("content drifted: %q", line)
	}

	// Drag extension across a scroll: extend to "echo" (content row 3
	// after the two scrolls) — still content-coherent.
	sel.dragging = true
	sel.extendDrag(3, 3, g)
	if got := sel.extractText(g, true); got != "charlie delta\necho" {
		t.Fatalf("extended extract = %q", got)
	}
}
