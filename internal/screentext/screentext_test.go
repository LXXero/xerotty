package screentext

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/x/ansi"
	uv "github.com/charmbracelet/ultraviolet"
)

func cell(s string, attrs uint8) uv.Cell {
	return uv.Cell{Content: s, Width: 1, Style: uv.Style{Attrs: attrs}}
}

// TestStyledLinesGhostText is the motivating case: typed text then a
// faint autocomplete suggestion must come back as separate runs so
// an agent can tell them apart.
func TestStyledLinesGhostText(t *testing.T) {
	row := []uv.Cell{
		cell("l", 0), cell("s", 0), cell(" ", 0),
		cell("-", uv.AttrFaint), cell("l", uv.AttrFaint), cell("a", uv.AttrFaint),
	}
	runs := StyledLines([][]uv.Cell{row})[0]
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d: %+v", len(runs), runs)
	}
	if runs[0].Text != "ls " || runs[0].Attrs != "" {
		t.Fatalf("typed run wrong: %+v", runs[0])
	}
	if runs[1].Text != "-la" || runs[1].Attrs != "faint" {
		t.Fatalf("ghost run wrong: %+v", runs[1])
	}
}

func TestStyledLinesColorsAndTrim(t *testing.T) {
	red := uv.Cell{Content: "E", Width: 1, Style: uv.Style{Fg: ansi.BasicColor(1)}}
	row := []uv.Cell{red, cell(" ", 0), cell(" ", 0)}
	runs := StyledLines([][]uv.Cell{row})[0]
	if len(runs) != 1 || runs[0].Fg != 1 || runs[0].Text != "E" {
		t.Fatalf("red run + trailing trim wrong: %+v", runs)
	}
	// JSON shape: terse keys, omitted defaults.
	b, _ := json.Marshal(runs[0])
	if string(b) != `{"t":"E","fg":1}` {
		t.Fatalf("json shape: %s", b)
	}
}

func TestStyledLinesWidePlaceholderSkipped(t *testing.T) {
	row := []uv.Cell{
		{Content: "宽", Width: 2},
		{}, // placeholder
		cell("x", 0),
	}
	runs := StyledLines([][]uv.Cell{row})[0]
	if len(runs) != 1 || runs[0].Text != "宽x" {
		t.Fatalf("wide handling wrong: %+v", runs)
	}
	if got := Lines([][]uv.Cell{row})[0]; got != "宽x" {
		t.Fatalf("flat wide handling wrong: %q", got)
	}
}

func TestStyledTrailingBackgroundKept(t *testing.T) {
	bgCell := uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: ansi.BasicColor(4)}}
	row := []uv.Cell{cell("a", 0), bgCell, bgCell}
	runs := StyledLines([][]uv.Cell{row})[0]
	if len(runs) != 2 || runs[1].Text != "  " || runs[1].Bg != 4 {
		t.Fatalf("styled trailing space should survive: %+v", runs)
	}
}
