package cellconv

import (
	"image/color"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func cell(s string) uv.Cell { return uv.Cell{Content: s, Width: 1} }

func TestTrimmedLen(t *testing.T) {
	bg := uv.Cell{Content: " ", Width: 1}
	bg.Style.Bg = color.RGBA{R: 255, A: 255}
	link := uv.Cell{Content: " ", Width: 1}
	link.Link.URL = "https://example.com"
	wide := uv.Cell{Content: "世", Width: 2}

	cases := []struct {
		name string
		row  []uv.Cell
		want int
	}{
		{"empty", nil, 0},
		{"all blank", []uv.Cell{{}, uv.EmptyCell, cell(" ")}, 0},
		{"text then blanks", []uv.Cell{cell("a"), cell("b"), uv.EmptyCell, {}}, 2},
		{"inner blanks kept", []uv.Cell{cell("a"), uv.EmptyCell, cell("b"), {}}, 3},
		{"background colour on the last cell is kept", []uv.Cell{cell("a"), uv.EmptyCell, bg}, 3},
		{"linked space is kept", []uv.Cell{cell("a"), link, uv.EmptyCell}, 2},
		// A wide character's second column is a zero cell: trimming it
		// leaves the row ending on the wide cell itself.
		{"ends in a wide character", []uv.Cell{cell("a"), wide, {}}, 2},
	}
	for _, tc := range cases {
		if got := TrimmedLen(tc.row); got != tc.want {
			t.Errorf("%s: TrimmedLen = %d, want %d", tc.name, got, tc.want)
		}
	}
}
