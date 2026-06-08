package scrollback

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

// gridStub is a daemon-style grid: scrollback lives in its own ring
// (sb), the visible screen is separate. Mirrors why the bug existed
// — the raw shadow emulator's ScrollbackLen would be 0 here.
type gridStub struct {
	sb     []string
	screen []string
	cols   int
}

func (g *gridStub) Width() int         { return g.cols }
func (g *gridStub) Height() int        { return len(g.screen) }
func (g *gridStub) ScrollbackLen() int { return len(g.sb) }
func cellOf(line string, col int) *uv.Cell {
	if col < 0 || col >= len(line) {
		return &uv.Cell{Content: " ", Width: 1}
	}
	return &uv.Cell{Content: string(line[col]), Width: 1}
}
func (g *gridStub) CellAt(col, row int) *uv.Cell {
	if row < 0 || row >= len(g.screen) {
		return nil
	}
	return cellOf(g.screen[row], col)
}
func (g *gridStub) ScrollbackCellAt(col, row int) *uv.Cell {
	if row < 0 || row >= len(g.sb) {
		return nil
	}
	return cellOf(g.sb[row], col)
}

// TestSearchFindsScrollback: search must match content that has
// scrolled OFF the visible screen into the buffer. Regression for
// daemon-mode search only covering the on-screen rows.
func TestSearchFindsScrollback(t *testing.T) {
	g := &gridStub{
		sb: []string{
			"the needle is in history",
			"more old lines",
		},
		screen: []string{"current prompt", "", ""},
		cols:   30,
	}

	s := &State{Query: "needle"}
	s.Search(g, len(g.screen))

	if len(s.Matches) == 0 {
		t.Fatal("search did not find a match in scrollback")
	}
	// The match must be on a scrollback row (negative Line index in
	// this codebase's convention), not the visible screen.
	found := false
	for _, m := range s.Matches {
		if m.Line < 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("match was not in scrollback (lines: %+v)", s.Matches)
	}
}

// TestSearchFindsBothScopes: a term present in both scrollback and
// the live screen yields matches in both.
func TestSearchFindsBothScopes(t *testing.T) {
	g := &gridStub{
		sb:     []string{"hello from history"},
		screen: []string{"hello from screen", "", ""},
		cols:   30,
	}
	s := &State{Query: "hello"}
	s.Search(g, len(g.screen))
	var sawSB, sawScreen bool
	for _, m := range s.Matches {
		if m.Line < 0 {
			sawSB = true
		} else {
			sawScreen = true
		}
	}
	if !sawSB || !sawScreen {
		t.Fatalf("expected matches in both scopes, got scrollback=%v screen=%v (%+v)", sawSB, sawScreen, s.Matches)
	}
}
