package scrollback

import (
	"fmt"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// fakeGrid: N scrollback rows of "line-<n> ..." content.
type fakeGrid struct{ sb, w, h int }

func (g fakeGrid) Width() int         { return g.w }
func (g fakeGrid) Height() int        { return g.h }
func (g fakeGrid) ScrollbackLen() int { return g.sb }
func (g fakeGrid) CellAt(col, row int) *uv.Cell {
	return &uv.Cell{Content: " ", Width: 1}
}
func (g fakeGrid) ScrollbackCellAt(col, row int) *uv.Cell {
	line := fmt.Sprintf("needle-%d padding", row)
	if col < len(line) {
		return &uv.Cell{Content: string(line[col]), Width: 1}
	}
	return nil
}

func waitSettled(t *testing.T, s *State, g fakeGrid, gen uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.EnsureSearch(g, g.h, gen)
		if !s.SearchPending {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("async search never settled")
}

// EnsureSearch must (a) find matches asynchronously, (b) not re-scan
// when nothing changed, (c) re-scan on content-generation bumps
// while preserving the navigation position.
func TestEnsureSearchAsync(t *testing.T) {
	g := fakeGrid{sb: 5000, w: 40, h: 10}
	s := New()
	s.Searching = true
	s.Query = "needle-42 "

	waitSettled(t, s, g, 1)
	if len(s.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(s.Matches))
	}

	// Unchanged query+gen: no new scan kicked.
	s.EnsureSearch(g, g.h, 1)
	if s.SearchPending {
		t.Fatal("re-scanned without any change")
	}

	// Content bump: re-scan, MatchIdx preserved.
	s.MatchIdx = 0
	waitSettled(t, s, g, 2)
	if len(s.Matches) != 1 || s.MatchIdx != 0 {
		t.Fatalf("after gen bump: matches=%d idx=%d", len(s.Matches), s.MatchIdx)
	}

	// Query change: matches reset and re-found.
	s.Query = "needle-7 "
	waitSettled(t, s, g, 2)
	if len(s.Matches) != 1 {
		t.Fatalf("after query change: matches = %d, want 1", len(s.Matches))
	}

	// Closing search cancels cleanly.
	s.CloseSearch()
	s.EnsureSearch(g, g.h, 3)
	if s.SearchPending {
		t.Fatal("pending after close")
	}
}
