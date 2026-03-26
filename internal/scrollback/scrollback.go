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
	Matches   []Match
	MatchIdx  int
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
}

// NextMatch advances to the next match.
func (s *State) NextMatch() {
	if len(s.Matches) == 0 {
		return
	}
	s.MatchIdx = (s.MatchIdx + 1) % len(s.Matches)
}

// PrevMatch goes to the previous match.
func (s *State) PrevMatch() {
	if len(s.Matches) == 0 {
		return
	}
	s.MatchIdx = (s.MatchIdx - 1 + len(s.Matches)) % len(s.Matches)
}

// CurrentMatch returns the current match or nil.
func (s *State) CurrentMatch() *Match {
	if len(s.Matches) == 0 {
		return nil
	}
	return &s.Matches[s.MatchIdx]
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
