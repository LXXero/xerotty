package daemonsource

import (
	"image/color"
	"reflect"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/LXXero/xerotty/internal/cellconv"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/screentext"
)

// shortRowFixture returns the same four scrollback rows twice: padded
// to cols as a version-10 daemon sends them, and at stored length as a
// version-11 daemon does.
func shortRowFixture(cols int) (padded, short [][]protocol.Cell, wantLens []int) {
	text := func(s string) []uv.Cell {
		var out []uv.Cell
		for _, r := range s {
			out = append(out, uv.Cell{Content: string(r), Width: 1})
		}
		return out
	}
	bg := uv.Cell{Content: " ", Width: 1}
	bg.Style.Bg = color.RGBA{G: 200, A: 255}
	rows := [][]uv.Cell{
		text("hello world"),
		{}, // empty line
		append(text("bg"), bg),
		append(text("wide"), uv.Cell{Content: "世", Width: 2}, uv.Cell{}),
	}
	wantLens = []int{11, 0, 3, 5} // the wide row loses only its zero second column
	for _, r := range rows {
		full := make([]uv.Cell, cols)
		copy(full, r)
		p := make([]protocol.Cell, cols)
		for i := range full {
			p[i] = cellconv.FromUV(&full[i])
		}
		padded = append(padded, p)
		n := cellconv.TrimmedLen(r)
		s := make([]protocol.Cell, n)
		for i := 0; i < n; i++ {
			s[i] = cellconv.FromUV(&r[i])
		}
		short = append(short, s)
	}
	return padded, short, wantLens
}

// TestShortRowsDisplayLikePaddedRows feeds one Source the padded rows a
// version-10 daemon sends and another the short rows a version-11
// daemon sends. Both must store the rows trimmed and show exactly the
// same cells through every reader of the scrollback mirror.
func TestShortRowsDisplayLikePaddedRows(t *testing.T) {
	const cols, rows = 20, 4
	padded, short, wantLens := shortRowFixture(cols)

	for _, windowed := range []bool{false, true} {
		mk := func(in [][]protocol.Cell) *Source {
			s := newSource(&Hub{windowed: windowed}, 1, cols, rows)
			s.applyScrollbackAppend(&protocol.ScrollbackAppend{ID: 1, BaseIdx: 0, Rows: in, Total: uint32(len(in))})
			return s
		}
		fromV10, fromV11 := mk(padded), mk(short)

		for _, s := range []*Source{fromV10, fromV11} {
			if got := s.ScrollbackLen(); got != len(wantLens) {
				t.Fatalf("windowed=%v: ScrollbackLen = %d, want %d", windowed, got, len(wantLens))
			}
			for i, want := range wantLens {
				if got := len(s.scrollback[i]); got != want {
					t.Errorf("windowed=%v: stored row %d has %d cells, want %d", windowed, i, got, want)
				}
			}
		}

		// Renderer path: the whole history scrolled into view.
		w10, _, _ := fromV10.SnapshotWindow(len(wantLens), 0, rows, cols)
		w11, _, _ := fromV11.SnapshotWindow(len(wantLens), 0, rows, cols)
		if !reflect.DeepEqual(w10, w11) {
			t.Errorf("windowed=%v: SnapshotWindow differs between padded and short rows", windowed)
		}
		// ...and it is the padded grid a version-10 pair showed before.
		for r := range padded {
			for c := 0; c < cols; c++ {
				want := cellconv.ToUV(padded[r][c])
				if !reflect.DeepEqual(w11[r][c], want) && !(cellconv.IsBlank(&want) && cellconv.IsBlank(&w11[r][c])) {
					t.Errorf("windowed=%v: window cell (%d,%d) = %+v, want %+v", windowed, c, r, w11[r][c], want)
				}
			}
		}
		if w11[2][2].Style.Bg == nil {
			t.Errorf("windowed=%v: trailing background-colour cell lost its colour", windowed)
		}
		if w11[3][4].Content != "世" || w11[3][4].Width != 2 {
			t.Errorf("windowed=%v: row ending in a wide character lost it: %+v", windowed, w11[3][4])
		}

		// Cell reader (selection, copy, search, link hit-testing): a
		// column past a row's end is nil, never a panic.
		for _, s := range []*Source{fromV10, fromV11} {
			if c := s.ScrollbackCellAt(10, 0); c == nil || c.Content != "d" {
				t.Errorf("windowed=%v: last cell of row 0 = %+v, want d", windowed, c)
			}
			for _, col := range []int{11, cols - 1, cols, cols + 50} {
				if c := s.ScrollbackCellAt(col, 0); c != nil {
					t.Errorf("windowed=%v: cell past the row end (col %d) = %+v, want nil", windowed, col, c)
				}
			}
			if c := s.ScrollbackCellAt(0, 1); c != nil {
				t.Errorf("windowed=%v: cell of an empty row = %+v, want nil", windowed, c)
			}
		}

		// Range reader (GUI MCP get_scrollback).
		l10 := screentext.Lines(fromV10.SnapshotScrollbackRange(0, 4))
		l11 := screentext.Lines(fromV11.SnapshotScrollbackRange(0, 4))
		want := []string{"hello world", "", "bg", "wide世"}
		if !reflect.DeepEqual(l10, want) || !reflect.DeepEqual(l11, want) {
			t.Errorf("windowed=%v: range text = %q / %q, want %q", windowed, l10, l11, want)
		}
		if !reflect.DeepEqual(screentext.StyledLines(fromV10.SnapshotScrollbackRange(0, 4)), screentext.StyledLines(fromV11.SnapshotScrollbackRange(0, 4))) {
			t.Errorf("windowed=%v: styled range differs between padded and short rows", windowed)
		}
	}
}

// TestRangeReplyRowsAreTrimmed covers the other way rows reach the
// mirror: a ScrollbackRange reply from a version-10 daemon.
func TestRangeReplyRowsAreTrimmed(t *testing.T) {
	const cols = 20
	padded, _, wantLens := shortRowFixture(cols)
	s := newSource(&Hub{windowed: true}, 1, cols, 4)
	s.scrollbackTotal = len(padded)
	s.scrollbackLen.Store(int64(len(padded)))
	s.applyScrollbackRange(&protocol.ScrollbackRange{ID: 1, From: 0, Rows: padded})
	for i, want := range wantLens {
		if got := len(s.scrollback[i]); got != want {
			t.Errorf("stored row %d has %d cells, want %d", i, got, want)
		}
	}
}
