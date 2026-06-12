// Package scrollback manages the scrollback buffer and search.
package scrollback

import (
	"regexp"
	"strings"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
)

// Grid is the read surface search needs — satisfied by both
// in-process Terminals and daemon Sources. Search MUST go through
// this, not the raw shadow emulator: daemon tabs keep scrollback in
// the Source's mirror ring (the shadow emulator's ScrollbackLen is
// 0), so reading the emulator directly searched only the visible
// screen. Same shape the renderer and link detection use.
type Grid interface {
	Width() int
	Height() int
	ScrollbackLen() int
	CellAt(col, row int) *uv.Cell
	ScrollbackCellAt(col, row int) *uv.Cell
}

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

	// Async search machinery. The naive design re-scanned the ENTIRE
	// scrollback every frame on the render thread to keep match
	// coordinates fresh against streaming output — unusable on large
	// (especially disk-backed) histories. EnsureSearch re-scans only
	// when the query/options/content actually change, and the scan
	// runs on a goroutine with cancellation so typing in the search
	// box never blocks a frame.
	SearchPending bool          // a scan is in flight (UI can show progress)
	OnAsyncDone   func()        // called from the worker when results land (wake the render loop)
	searchID      uint64        // identifies the latest scan; stale results are dropped
	searchCancel  chan struct{} // closed to abort the in-flight scan
	searchRes     chan asyncResult
	lastQuery     string
	lastCase      bool
	lastRegex     bool
	lastWord      bool
	lastGen       uint64
	haveFP        bool
}

type asyncResult struct {
	id      uint64
	matches []Match
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

// Search runs a synchronous search across visible screen +
// scrollback. Kept for tests and direct callers; the GUI frame path
// uses EnsureSearch (async + change-gated).
func (s *State) Search(emu Grid, visibleRows int) {
	s.Matches = nil
	s.MatchIdx = 0
	if s.Query == "" {
		return
	}
	s.Matches, _ = scanGrid(emu, s.Query, s.CaseSensitive, s.UseRegex, s.WholeWord, nil)
	s.selectNearest(visibleRows)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// EnsureSearch keeps s.Matches in sync with the query, options, and
// terminal content, re-scanning only when one of them changed
// (contentGen is the Source's RenderGeneration). The scan runs on a
// background goroutine; results are adopted here on a later frame.
// Coordinate freshness is the same as the old scan-every-frame
// design — content changes bump contentGen, which triggers a
// re-scan — without the O(scrollback) per-frame cost.
func (s *State) EnsureSearch(emu Grid, visibleRows int, contentGen uint64) {
	if !s.Searching || s.Query == "" {
		s.cancelSearch()
		return
	}

	queryChanged := !s.haveFP || s.Query != s.lastQuery ||
		s.CaseSensitive != s.lastCase || s.UseRegex != s.lastRegex ||
		s.WholeWord != s.lastWord
	if queryChanged || contentGen != s.lastGen {
		s.kickSearch(emu, queryChanged)
		s.lastQuery, s.lastCase, s.lastRegex, s.lastWord = s.Query, s.CaseSensitive, s.UseRegex, s.WholeWord
		s.lastGen = contentGen
		s.haveFP = true
	}

	// Adopt a finished scan, if any.
	if s.searchRes != nil {
		select {
		case res := <-s.searchRes:
			if res.id != s.searchID {
				return // stale scan, a newer one is in flight
			}
			s.SearchPending = false
			prevIdx := s.MatchIdx
			hadMatches := len(s.Matches) > 0
			s.Matches = res.matches
			if hadMatches && prevIdx < len(s.Matches) {
				// Content refresh: keep the user's navigation position.
				s.MatchIdx = prevIdx
			} else {
				s.selectNearest(visibleRows)
			}
		default:
		}
	}
}

// kickSearch aborts any in-flight scan and starts a new one over a
// snapshot of the query/options. The Grid implementations are
// internally locked (SafeEmulator / disk mutex), so reading from a
// goroutine is safe; rows appended mid-scan are picked up by the
// next contentGen-triggered scan.
func (s *State) kickSearch(emu Grid, queryChanged bool) {
	s.cancelInFlight()
	if queryChanged {
		// New query: stale matches would highlight the wrong thing.
		s.Matches = nil
		s.MatchIdx = 0
	}
	s.searchID++
	s.SearchPending = true
	if s.searchRes == nil {
		s.searchRes = make(chan asyncResult, 1)
	}
	cancel := make(chan struct{})
	s.searchCancel = cancel
	id := s.searchID
	query, caseSens, useRegex, wholeWord := s.Query, s.CaseSensitive, s.UseRegex, s.WholeWord
	res := s.searchRes
	done := s.OnAsyncDone
	go func() {
		matches, ok := scanGrid(emu, query, caseSens, useRegex, wholeWord, cancel)
		if !ok {
			return // cancelled — a newer scan owns the channel now
		}
		select {
		case res <- asyncResult{id: id, matches: matches}:
		case <-cancel:
			return
		}
		if done != nil {
			done()
		}
	}()
}

func (s *State) cancelInFlight() {
	if s.searchCancel != nil {
		close(s.searchCancel)
		s.searchCancel = nil
	}
	// Drain a result that landed after the last adopt — it belongs
	// to a superseded scan.
	if s.searchRes != nil {
		select {
		case <-s.searchRes:
		default:
		}
	}
	s.SearchPending = false
}

// cancelSearch tears down async state when search closes or empties.
func (s *State) cancelSearch() {
	s.cancelInFlight()
	s.haveFP = false
}

// selectNearest picks the match closest to the viewport bottom —
// same heuristic the synchronous Search uses.
func (s *State) selectNearest(visibleRows int) {
	if len(s.Matches) == 0 {
		s.MatchIdx = 0
		return
	}
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

// scanGrid is the search core shared by the synchronous Search and
// the async path. Returns ok=false if cancelled. checkEvery rows it
// polls the cancel channel — cheap enough to keep keystroke
// cancellation snappy on million-line histories.
func scanGrid(emu Grid, query string, caseSens, useRegex, wholeWord bool, cancel <-chan struct{}) ([]Match, bool) {
	const checkEvery = 2048
	var matches []Match
	cols := emu.Width()

	var re *regexp.Regexp
	if useRegex {
		pattern := query
		if !caseSens {
			pattern = "(?i)" + pattern
		}
		var err error
		re, err = regexp.Compile(pattern)
		if err != nil {
			return nil, true // invalid regex — empty result set
		}
	} else if !caseSens {
		query = strings.ToLower(query)
	}

	sbLen := emu.ScrollbackLen()
	for row := 0; row < sbLen; row++ {
		if cancel != nil && row%checkEvery == 0 {
			select {
			case <-cancel:
				return nil, false
			default:
			}
		}
		line := extractScrollbackLine(emu, row, cols)
		if useRegex {
			findMatchesRegex(&matches, line, re, -(sbLen - row), wholeWord)
		} else {
			findMatchesPlain(&matches, line, query, -(sbLen - row), caseSens, wholeWord)
		}
	}
	rows := emu.Height()
	for row := 0; row < rows; row++ {
		line := extractScreenLine(emu, row, cols)
		if useRegex {
			findMatchesRegex(&matches, line, re, row, wholeWord)
		} else {
			findMatchesPlain(&matches, line, query, row, caseSens, wholeWord)
		}
	}
	return matches, true
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

func extractScreenLine(emu Grid, row, cols int) string {
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

// scrollbackLineTexter is an optional fast path: disk-backed
// scrollback decodes a WHOLE line per ScrollbackCellAt call, so the
// per-cell loop below re-read and re-decoded each line once per
// COLUMN (200x waste, two pread syscalls each). Implementations
// return the row's text in one read.
type scrollbackLineTexter interface {
	ScrollbackLineText(row, cols int) string
}

func extractScrollbackLine(emu Grid, row, cols int) string {
	if lt, ok := emu.(scrollbackLineTexter); ok {
		return lt.ScrollbackLineText(row, cols)
	}
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
