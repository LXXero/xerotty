// Package screentext converts terminal cell grids into the text
// shapes the MCP servers return to agents. Two flavors:
//
//   - Lines: flat strings — cheap, the default.
//   - StyledLines: per-line runs of identically-styled text, so an
//     agent can SEE presentation, not just content. The motivating
//     case: TUI apps (Claude Code among them) render autocomplete
//     "ghost text" as faint runs after the cursor — in a flat string
//     that's indistinguishable from text the user actually typed.
//     Styled runs + the cursor position make it unambiguous, and as
//     a bonus expose "this run is red" (errors) etc.
//
// Both MCP servers (internal/mcp, internal/guimcp) use this package
// so the two surfaces can't drift.
package screentext

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	uv "github.com/charmbracelet/ultraviolet"
)

// Run is a stretch of identically-styled text within one line.
// Field names are deliberately terse — agents pay tokens for them
// on every screen read.
type Run struct {
	Text string `json:"t"`
	// Fg/Bg: palette index (int, 0-255) or "#rrggbb" (string).
	// Omitted = terminal default.
	Fg any `json:"fg,omitempty"`
	Bg any `json:"bg,omitempty"`
	// Attrs: comma-joined subset of bold,faint,italic,underline,
	// blink,reverse,strike,conceal. Omitted = plain.
	Attrs string `json:"a,omitempty"`
}

// Lines flattens a cell grid to plain strings, one per row,
// trailing whitespace trimmed.
func Lines(grid [][]uv.Cell) []string {
	out := make([]string, len(grid))
	for r, row := range grid {
		var sb strings.Builder
		for c := range row {
			writeCell(&sb, row, c)
		}
		out[r] = strings.TrimRight(sb.String(), " ")
	}
	return out
}

// StyledLines converts a cell grid to per-line styled runs.
// Adjacent cells with identical style merge into one run; trailing
// runs that are default-styled whitespace are dropped (mirroring
// Lines' TrimRight). A row with no visible content is an empty
// slice, never nil, so JSON renders [] not null.
func StyledLines(grid [][]uv.Cell) [][]Run {
	out := make([][]Run, len(grid))
	for r, row := range grid {
		runs := []Run{}
		var cur *Run
		for c := range row {
			cell := &row[c]
			if cell.Width == 0 && cell.Content == "" {
				// Wide-cell placeholder — the glyph one column left
				// already accounts for this column.
				continue
			}
			fg, bg, attrs := encodeStyle(&cell.Style)
			text := cell.Content
			if text == "" {
				text = " "
			}
			if cur != nil && cur.Fg == fg && cur.Bg == bg && cur.Attrs == attrs {
				cur.Text += text
				continue
			}
			runs = append(runs, Run{Text: text, Fg: fg, Bg: bg, Attrs: attrs})
			cur = &runs[len(runs)-1]
		}
		// Trim trailing default-styled whitespace; styled trailing
		// space stays (a bg-colored status bar's padding is content).
		for len(runs) > 0 {
			last := &runs[len(runs)-1]
			if last.Fg != nil || last.Bg != nil || last.Attrs != "" {
				break
			}
			trimmed := strings.TrimRight(last.Text, " ")
			if trimmed != "" {
				last.Text = trimmed
				break
			}
			runs = runs[:len(runs)-1]
		}
		out[r] = runs
	}
	return out
}

func writeCell(sb *strings.Builder, row []uv.Cell, c int) {
	cell := &row[c]
	if cell.Width == 0 && cell.Content == "" {
		return // wide-cell placeholder
	}
	if cell.Content == "" {
		sb.WriteByte(' ')
		return
	}
	sb.WriteString(cell.Content)
}

// encodeStyle classifies a cell style for the wire: palette colors
// as ints, anything else as #rrggbb, attributes as a stable
// comma-joined string (also the run-merge comparison key).
func encodeStyle(s *uv.Style) (fg, bg any, attrs string) {
	fg = encodeColor(s.Fg)
	bg = encodeColor(s.Bg)
	var a []string
	if s.Attrs&uv.AttrBold != 0 {
		a = append(a, "bold")
	}
	if s.Attrs&uv.AttrFaint != 0 {
		a = append(a, "faint")
	}
	if s.Attrs&uv.AttrItalic != 0 {
		a = append(a, "italic")
	}
	if s.Underline != ansi.UnderlineNone {
		a = append(a, "underline")
	}
	if s.Attrs&uv.AttrBlink != 0 || s.Attrs&uv.AttrRapidBlink != 0 {
		a = append(a, "blink")
	}
	if s.Attrs&uv.AttrReverse != 0 {
		a = append(a, "reverse")
	}
	if s.Attrs&uv.AttrStrikethrough != 0 {
		a = append(a, "strike")
	}
	if s.Attrs&uv.AttrConceal != 0 {
		a = append(a, "conceal")
	}
	return fg, bg, strings.Join(a, ",")
}

func encodeColor(c interface{ RGBA() (r, g, b, a uint32) }) any {
	if c == nil {
		return nil
	}
	switch v := c.(type) {
	case ansi.BasicColor:
		return int(v)
	case ansi.IndexedColor: // ansi.ExtendedColor aliases this
		return int(v)
	default:
		r, g, b, _ := c.RGBA()
		return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
	}
}
