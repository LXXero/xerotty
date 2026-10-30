package app

import (
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"unicode"

	"github.com/LXXero/xerotty/internal/config"
	uv "github.com/charmbracelet/ultraviolet"
)

// urlPattern matches common URLs in terminal output.
var urlPattern = regexp.MustCompile(`https?://[^\s<>"'\x60\x7f-\x9f\]})]+`)

// maxLinkRows bounds how many rows one link can cover. A long OAuth
// URL in a narrow pane needs a few dozen; the bound stops a hover from
// walking the whole of a big block of text with no spaces in it.
const maxLinkRows = 64

// maxLinkTooltip bounds the URL text shown on hover. The start of a URL
// is the part that says where it goes.
const maxLinkTooltip = 1024

// linkSpan is the part of a link on one row. Row is a viewport row, and
// is negative or past the last row where a wrapped link runs off the
// screen. EndCol is inclusive.
type linkSpan struct {
	Row      int
	StartCol int
	EndCol   int
}

// linkHit holds the link found under a screen position.
type linkHit struct {
	URL   string
	Spans []linkSpan
	// OSC8 is set when the URL came from an OSC 8 hyperlink, so the
	// text on screen doesn't have to match it.
	OSC8 bool
}

// linkGrid is the read surface link detection needs — satisfied by
// both in-process Terminals and daemon Sources. Hit-testing MUST go
// through this (not the raw shadow emulator): daemon tabs mirror
// scrollback in the Source ring and pty tabs can spill it to disk,
// so the raw emulator's ScrollbackLen is 0/partial and scrolled-up
// rows computed negative content indices — links silently died
// anywhere above the live screen.
type linkGrid interface {
	Width() int
	Height() int
	ScrollbackLen() int
	CellAt(col, row int) *uv.Cell
	ScrollbackCellAt(col, row int) *uv.Cell
}

// scrollbackRowReader is an optional fast path: disk-backed scrollback
// decodes a WHOLE line per ScrollbackCellAt call.
type scrollbackRowReader interface {
	ScrollbackRow(row, cols int) []uv.Cell
}

// linkRows reads whole rows by content index (0 = oldest scrollback
// row), the same index space above and below the live screen.
type linkRows struct {
	g     linkGrid
	cols  int
	sbLen int
	total int
}

// row returns the cells of content row idx, always cols long, or nil
// when idx is outside the content.
func (r linkRows) row(idx int) []uv.Cell {
	if idx < 0 || idx >= r.total {
		return nil
	}
	if idx < r.sbLen {
		if sr, ok := r.g.(scrollbackRowReader); ok {
			return sr.ScrollbackRow(idx, r.cols)
		}
	}
	out := make([]uv.Cell, r.cols)
	for col := range out {
		var c *uv.Cell
		if idx < r.sbLen {
			c = r.g.ScrollbackCellAt(col, idx)
		} else {
			c = r.g.CellAt(col, idx-r.sbLen)
		}
		if c != nil {
			out[col] = *c
		}
	}
	return out
}

// detectLinkAt finds the link under a viewport position. An OSC 8
// hyperlink on the cell wins; otherwise the text is searched for a URL,
// joining rows it wraps across. scrollOffset accounts for scrollback
// position. The returned spans are in viewport rows.
func detectLinkAt(emu linkGrid, col, row, scrollOffset int) *linkHit {
	sbLen := emu.ScrollbackLen()
	r := linkRows{g: emu, cols: emu.Width(), sbLen: sbLen, total: sbLen + emu.Height()}
	base := sbLen - scrollOffset
	idx := base + row

	cells := r.row(idx)
	if col < 0 || col >= len(cells) {
		return nil
	}
	col = ownerCol(cells, col)

	var hit *linkHit
	if cells[col].Link.URL != "" {
		// A target we won't open isn't a link, even when the text
		// shown looks like one: the program said this cell goes
		// somewhere else.
		if !openableURL(cells[col].Link.URL) {
			return nil
		}
		hit = osc8Hit(r, idx, cells, col)
	} else {
		hit = textHit(r, idx, cells, col)
	}
	if hit == nil {
		return nil
	}
	for i := range hit.Spans {
		hit.Spans[i].Row -= base
	}
	return hit
}

// openableURL reports whether a link target may be opened: http and
// https only. An OSC 8 target is chosen by whatever is running in the
// terminal, and file:, ssh: or a custom handler scheme would be a way
// to reach the opener with something other than a web page.
func openableURL(target string) bool {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// linkTooltipText is the URL as shown on hover.
func linkTooltipText(target string) string {
	if len(target) <= maxLinkTooltip {
		return target
	}
	return strings.ToValidUTF8(target[:maxLinkTooltip], "") + "…"
}

// isWidePlaceholder reports whether col is the right half of a wide
// character: the cell after a width-2 cell, holding nothing itself.
func isWidePlaceholder(cells []uv.Cell, col int) bool {
	return col > 0 && cells[col-1].Width == 2 && cells[col].Content == ""
}

// ownerCol maps the right half of a wide character to its left half.
func ownerCol(cells []uv.Cell, col int) int {
	if isWidePlaceholder(cells, col) {
		return col - 1
	}
	return col
}

// hasContent reports whether a cell shows anything or belongs to a link.
func hasContent(cells []uv.Cell, col int) bool {
	c := &cells[col]
	return c.Link.URL != "" || (c.Content != "" && c.Content != " ") || isWidePlaceholder(cells, col)
}

// firstContent returns the first column with content, or -1.
func firstContent(cells []uv.Cell) int {
	for col := range cells {
		if hasContent(cells, col) {
			return col
		}
	}
	return -1
}

// lastContent returns the last column with content, or -1.
func lastContent(cells []uv.Cell) int {
	for col := len(cells) - 1; col >= 0; col-- {
		if hasContent(cells, col) {
			return col
		}
	}
	return -1
}

// linkRun returns the contiguous columns around col carrying target.
func linkRun(cells []uv.Cell, col int, target string) (start, end int) {
	same := func(c int) bool { return cells[ownerCol(cells, c)].Link.URL == target }
	start, end = col, col
	for start > 0 && same(start-1) {
		start--
	}
	for end < len(cells)-1 && same(end+1) {
		end++
	}
	return start, end
}

// osc8Hit collects every contiguous cell linked to the URL at (idx,
// col). The link carries on to a neighbouring row when it reaches the
// end of this row's content and the next row's content starts with it,
// which also covers a TUI's left and right margins.
func osc8Hit(r linkRows, idx int, cells []uv.Cell, col int) *linkHit {
	target := cells[col].Link.URL
	start, end := linkRun(cells, col, target)
	spans := []linkSpan{{idx, start, end}}

	for i, cur, s := idx-1, cells, start; i >= 0 && len(spans) < maxLinkRows; i-- {
		if s != firstContent(cur) {
			break
		}
		prev := r.row(i)
		if prev == nil {
			break
		}
		last := lastContent(prev)
		if last < 0 || prev[ownerCol(prev, last)].Link.URL != target {
			break
		}
		var e int
		s, e = linkRun(prev, last, target)
		spans = append([]linkSpan{{i, s, e}}, spans...)
		cur = prev
	}
	for i, cur, e := idx+1, cells, end; len(spans) < maxLinkRows; i++ {
		if e != lastContent(cur) {
			break
		}
		next := r.row(i)
		if next == nil {
			break
		}
		first := firstContent(next)
		if first < 0 || next[first].Link.URL != target {
			break
		}
		var s int
		s, e = linkRun(next, first, target)
		spans = append(spans, linkSpan{i, s, e})
		cur = next
	}
	return &linkHit{URL: target, Spans: spans, OSC8: true}
}

// isURLCell reports whether a cell holds text a URL could continue
// through. Stricter than urlPattern on purpose: box drawing matches the
// pattern, and a rule drawn under a URL must not join it.
func isURLCell(c *uv.Cell) bool {
	if c.Content == "" {
		return false
	}
	for _, ch := range c.Content {
		switch {
		case ch > 0x20 && ch < 0x7f:
			if strings.ContainsRune("<>\"'`]})", ch) {
				return false
			}
		case !unicode.IsLetter(ch) && !unicode.IsNumber(ch) && !unicode.IsMark(ch):
			return false
		}
	}
	return true
}

// startsURL reports whether a URL's scheme begins at col, which means a
// new URL and not the rest of the one on the row above.
func startsURL(cells []uv.Cell, col int) bool {
	var b strings.Builder
	for c := col; c < len(cells) && c < col+len("https://"); c++ {
		b.WriteString(cells[c].Content)
	}
	s := b.String()
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// wrapsOnto reports whether the text at the end of prev carries on at
// the start of next. The emulator keeps no soft-wrap flag, so this reads
// the layout: either the terminal wrapped it (last column to column 0),
// or a TUI did inside a margin, where both rows start at the same column
// and prev reaches the right edge less that same margin, or the real
// right edge if the TUI only pads the left. Text that ends exactly at
// the edge followed by unrelated text on the next row looks the same as
// a wrap and gets joined; only a flag in the emulator could tell them
// apart.
func wrapsOnto(prev, next []uv.Cell) bool {
	if prev == nil || next == nil {
		return false
	}
	last, first := lastContent(prev), firstContent(next)
	if last < 0 || first < 0 {
		return false
	}
	if !isURLCell(&prev[ownerCol(prev, last)]) || !isURLCell(&next[first]) || startsURL(next, first) {
		return false
	}
	if last == len(prev)-1 && first == 0 {
		return true
	}
	margin := firstContent(prev)
	return margin > 0 && first == margin && last >= len(prev)-1-margin
}

// textPos is where one byte of joined text sits on the grid. LastCol
// differs from Col for a wide character.
type textPos struct {
	Row     int
	Col     int
	LastCol int
}

// joinRows concatenates rows that wrap onto each other, dropping the
// margins between them, with the grid position of every byte. firstIdx
// is the content row of rows[0].
func joinRows(rows [][]uv.Cell, firstIdx int) (string, []textPos) {
	var b strings.Builder
	var pos []textPos
	for i, cells := range rows {
		from, to := 0, len(cells)-1
		if i > 0 {
			from = firstContent(cells)
		}
		if i < len(rows)-1 {
			to = lastContent(cells)
		}
		for col := from; col <= to; col++ {
			if isWidePlaceholder(cells, col) {
				continue
			}
			content := cells[col].Content
			if content == "" {
				content = " "
			}
			last := col
			if cells[col].Width == 2 && col < len(cells)-1 {
				last = col + 1
			}
			b.WriteString(content)
			for range len(content) {
				pos = append(pos, textPos{firstIdx + i, col, last})
			}
		}
	}
	return b.String(), pos
}

// textHit finds a URL in the text under (idx, col). It walks back to the
// row the wrapped text starts on, so a click on any row of a URL finds
// all of it, and forward to the row it ends on.
func textHit(r linkRows, idx int, cells []uv.Cell, col int) *linkHit {
	if !hasContent(cells, col) {
		return nil
	}
	rows := [][]uv.Cell{cells}
	first := idx
	for len(rows) < maxLinkRows {
		prev := r.row(first - 1)
		if !wrapsOnto(prev, rows[0]) {
			break
		}
		rows = append([][]uv.Cell{prev}, rows...)
		first--
	}
	for len(rows) < maxLinkRows {
		next := r.row(first + len(rows))
		if !wrapsOnto(rows[len(rows)-1], next) {
			break
		}
		rows = append(rows, next)
	}

	text, pos := joinRows(rows, first)
	for _, loc := range urlPattern.FindAllStringIndex(text, -1) {
		var spans []linkSpan
		found := false
		for _, p := range pos[loc[0]:loc[1]] {
			if p.Row == idx && col >= p.Col && col <= p.LastCol {
				found = true
			}
			if n := len(spans); n > 0 && spans[n-1].Row == p.Row {
				spans[n-1].EndCol = p.LastCol
			} else {
				spans = append(spans, linkSpan{p.Row, p.Col, p.LastCol})
			}
		}
		if found {
			return &linkHit{URL: text[loc[0]:loc[1]], Spans: spans}
		}
	}
	return nil
}

// openURL opens a URL with the given opener command.
func openURL(target, opener string) {
	if opener == "" {
		opener = config.DefaultOpener()
	}
	cmd := exec.Command(opener, target)
	_ = cmd.Start()
}
