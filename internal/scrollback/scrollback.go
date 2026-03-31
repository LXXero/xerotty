// Package scrollback manages the scrollback buffer and search.
package scrollback

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/vt"
)

// State tracks the scroll position and search state for a terminal.
type State struct {
	Offset    int // lines scrolled back from bottom (0 = live)
	PrevSBLen int // scrollback length at last frame — used to freeze viewport on new output
	Searching bool
	Query     string
	Matches   []Match
	MatchIdx  int
	// Search options
	CaseSensitive bool
	UseRegex      bool
	WholeWord     bool
	WrapAround    bool
}

// Match represents a search match in the terminal/scrollback.
type Match struct {
	Line int // negative = scrollback, 0+ = visible screen
	Col  int
	Len  int
}

// New creates a new scrollback state.
func New() *State {
	return &State{WrapAround: true}
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

// OpenSearch enters search mode, preserving search options across sessions.
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
// visibleRows is used to pick the starting match near the viewport bottom.
func (s *State) Search(emu *vt.SafeEmulator, visibleRows int) {
	s.Matches = nil
	s.MatchIdx = 0

	if s.Query == "" {
		return
	}

	cols := emu.Width()

	if s.UseRegex {
		pattern := s.Query
		if !s.CaseSensitive {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return // invalid regex — leave matches empty
		}
		sbLen := emu.ScrollbackLen()
		for row := 0; row < sbLen; row++ {
			line := extractScrollbackLine(emu, row, cols)
			findMatchesRegex(&s.Matches, line, re, -(sbLen-row), s.WholeWord)
		}
		rows := emu.Height()
		for row := 0; row < rows; row++ {
			line := extractScreenLine(emu, row, cols)
			findMatchesRegex(&s.Matches, line, re, row, s.WholeWord)
		}
	} else {
		query := s.Query
		if !s.CaseSensitive {
			query = strings.ToLower(query)
		}
		sbLen := emu.ScrollbackLen()
		for row := 0; row < sbLen; row++ {
			line := extractScrollbackLine(emu, row, cols)
			findMatchesPlain(&s.Matches, line, query, -(sbLen-row), s.CaseSensitive, s.WholeWord)
		}
		rows := emu.Height()
		for row := 0; row < rows; row++ {
			line := extractScreenLine(emu, row, cols)
			findMatchesPlain(&s.Matches, line, query, row, s.CaseSensitive, s.WholeWord)
		}
	}

	// Start at the match nearest the viewport bottom so navigation begins
	// close to where the user is reading.
	if len(s.Matches) > 0 {
		viewBottom := -s.Offset + visibleRows - 1
		best := 0
		bestDist := abs(s.Matches[0].Line - viewBottom)
		for i, m := range s.Matches {
			if d := abs(m.Line - viewBottom); d < bestDist {
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

// NextMatch advances to the next match, wrapping if WrapAround is set.
func (s *State) NextMatch() {
	if len(s.Matches) == 0 {
		return
	}
	if s.WrapAround {
		s.MatchIdx = (s.MatchIdx + 1) % len(s.Matches)
	} else if s.MatchIdx < len(s.Matches)-1 {
		s.MatchIdx++
	}
}

// PrevMatch goes to the previous match, wrapping if WrapAround is set.
func (s *State) PrevMatch() {
	if len(s.Matches) == 0 {
		return
	}
	if s.WrapAround {
		s.MatchIdx = (s.MatchIdx - 1 + len(s.Matches)) % len(s.Matches)
	} else if s.MatchIdx > 0 {
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

func isWordChar(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func atWordBoundary(line string, byteCol, byteLen int) bool {
	// col and length are byte offsets; decode runes at the boundaries directly.
	before := byteCol == 0
	if !before {
		r, _ := decodeLastRune(line[:byteCol])
		before = !isWordChar(r)
	}
	after := byteCol+byteLen >= len(line)
	if !after {
		r, _ := decodeFirstRune(line[byteCol+byteLen:])
		after = !isWordChar(r)
	}
	return before && after
}

func decodeLastRune(s string) (rune, int) {
	// walk backwards to find the start of the last rune
	for i := len(s) - 1; i >= 0; i-- {
		if s[i]&0xC0 != 0x80 { // not a continuation byte
			r := []rune(s[i:])[0]
			return r, len(s) - i
		}
	}
	return 0, 0
}

func decodeFirstRune(s string) (rune, int) {
	if len(s) == 0 {
		return 0, 0
	}
	r := []rune(s)[0]
	return r, len(string(r))
}

func findMatchesPlain(matches *[]Match, line, query string, lineIdx int, caseSensitive, wholeWord bool) {
	searchLine := line
	if !caseSensitive {
		searchLine = strings.ToLower(line)
	}
	offset := 0
	for {
		idx := strings.Index(searchLine[offset:], query)
		if idx < 0 {
			break
		}
		col := offset + idx
		if !wholeWord || atWordBoundary(line, col, len(query)) {
			*matches = append(*matches, Match{Line: lineIdx, Col: col, Len: len(query)})
		}
		offset = col + 1
		if offset >= len(searchLine) {
			break
		}
	}
}

func findMatchesRegex(matches *[]Match, line string, re *regexp.Regexp, lineIdx int, wholeWord bool) {
	locs := re.FindAllStringIndex(line, -1)
	for _, loc := range locs {
		col, end := loc[0], loc[1]
		length := end - col
		if !wholeWord || atWordBoundary(line, col, length) {
			*matches = append(*matches, Match{Line: lineIdx, Col: col, Len: length})
		}
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
