// Package scrollback manages the scrollback buffer and search.
package scrollback

import (
	"strings"

	"github.com/charmbracelet/x/vt"
)

// State tracks the scroll position and search state for a terminal.
type State struct {
	Offset    int  // lines scrolled back from bottom (0 = live)
	Searching bool
	Query     string
	Matches        []Match
	MatchIdx       int
}

// Match represents a search match in the terminal/scrollback.
type Match struct {
	Line int // negative = scrollback, 0+ = visible screen
	Col  int
	Len  int
}

// New creates a new scrollback state.
func New() *State {
	return &State{}
}

// ScrollUp scrolls up by n lines.
func (s *State) ScrollUp(n int, maxLines int) {
	s.Offset += n
	if s.Offset > maxLines {
		s.Offset = maxLines
	}
}

// ScrollDown scrolls down by n lines.
func (s *State) ScrollDown(n int) {
	s.Offset -= n
	if s.Offset < 0 {
		s.Offset = 0
	}
}

// PageUp scrolls up by a full page.
func (s *State) PageUp(pageSize int, maxLines int) {
	s.ScrollUp(pageSize, maxLines)
}

// PageDown scrolls down by a full page.
func (s *State) PageDown(pageSize int) {
	s.ScrollDown(pageSize)
}

// IsScrolled returns true if not at the live position.
func (s *State) IsScrolled() bool {
	return s.Offset > 0
}

// Reset returns to the live position.
func (s *State) Reset() {
	s.Offset = 0
}

// OpenSearch enters search mode.
func (s *State) OpenSearch() {
	s.Searching = true
	s.Query = ""
	s.Matches = nil
	s.MatchIdx = 0
}

// CloseSearch exits search mode.
func (s *State) CloseSearch() {
	s.Searching = false
	s.Query = ""
	s.Matches = nil
	s.MatchIdx = 0
}

// Search runs an incremental search across visible screen + scrollback.
func (s *State) Search(emu *vt.SafeEmulator) {
	s.Matches = nil
	s.MatchIdx = 0

	if s.Query == "" {
		return
	}

	query := strings.ToLower(s.Query)
	cols := emu.Width()

	// Search scrollback (negative line indices, oldest first)
	sbLen := emu.ScrollbackLen()
	for row := 0; row < sbLen; row++ {
		line := extractScrollbackLine(emu, row, cols)
		findMatches(&s.Matches, line, query, -(sbLen - row), cols)
	}

	// Search visible screen
	rows := emu.Height()
	for row := 0; row < rows; row++ {
		line := extractScreenLine(emu, row, cols)
		findMatches(&s.Matches, line, query, row, cols)
	}

	// Start at the nearest match to the current viewport, not the oldest.
	// Viewport top is at line -s.Offset (scrollback) or 0 (live).
	if len(s.Matches) > 0 {
		viewTop := -s.Offset
		best := 0
		bestDist := abs(s.Matches[0].Line - viewTop)
		for i, m := range s.Matches {
			d := abs(m.Line - viewTop)
			if d < bestDist {
				best = i
				bestDist = d
			}
		}
		s.MatchIdx = best
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// NextMatch advances to the next match (clamped, no wrap).
func (s *State) NextMatch() {
	if len(s.Matches) == 0 {
		return
	}
	if s.MatchIdx < len(s.Matches)-1 {
		s.MatchIdx++
	}
}

// PrevMatch goes to the previous match (clamped, no wrap).
func (s *State) PrevMatch() {
	if len(s.Matches) == 0 {
		return
	}
	if s.MatchIdx > 0 {
		s.MatchIdx--
	}
}

// CurrentMatch returns the current match or nil.
func (s *State) CurrentMatch() *Match {
	if len(s.Matches) == 0 {
		return nil
	}
	return &s.Matches[s.MatchIdx]
}

// ScrollToCurrentMatch adjusts the scroll offset so the current match is visible.
func (s *State) ScrollToCurrentMatch(visibleRows int) {
	m := s.CurrentMatch()
	if m == nil {
		return
	}

	// screenRow = m.Line + s.Offset; we need screenRow in [0, visibleRows)
	screenRow := m.Line + s.Offset
	if screenRow < 0 {
		// Match is above visible area — scroll up
		s.Offset = -m.Line
	} else if screenRow >= visibleRows {
		// Match is below visible area — scroll down
		s.Offset = -m.Line + visibleRows - 1
	}
	if s.Offset < 0 {
		s.Offset = 0
	}
}

func extractScreenLine(emu *vt.SafeEmulator, row, cols int) string {
	var b strings.Builder
	for col := 0; col < cols; col++ {
		cell := emu.CellAt(col, row)
		if cell != nil && cell.Content != "" {
			b.WriteString(cell.Content)
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func extractScrollbackLine(emu *vt.SafeEmulator, row, cols int) string {
	var b strings.Builder
	for col := 0; col < cols; col++ {
		cell := emu.ScrollbackCellAt(col, row)
		if cell != nil && cell.Content != "" {
			b.WriteString(cell.Content)
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func findMatches(matches *[]Match, line, query string, lineIdx, cols int) {
	lower := strings.ToLower(line)
	offset := 0
	for {
		idx := strings.Index(lower[offset:], query)
		if idx < 0 {
			break
		}
		col := offset + idx
		*matches = append(*matches, Match{
			Line: lineIdx,
			Col:  col,
			Len:  len(query),
		})
		offset = col + 1
		if offset >= len(lower) {
			break
		}
	}
}
