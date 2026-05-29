package daemonsource

import (
	"testing"

	"github.com/LXXero/xerotty/internal/protocol"
	uv "github.com/charmbracelet/ultraviolet"
)

// TestUvCellFromProtoAttrs pins the protocol→ultraviolet attribute
// remap. The two enums order their bits differently (protocol:
// Bold,Italic,Faint,…; ultraviolet: Bold,Faint,Italic,…), so a raw
// bit copy silently swaps faint↔italic and reverse/strike. Regression
// guard for that swap, which made daemon-backed faint text render
// un-dimmed.
func TestUvCellFromProtoAttrs(t *testing.T) {
	cases := []struct {
		name  string
		proto uint32
		want  uint8
	}{
		{"bold", protocol.AttrBold, uv.AttrBold},
		{"italic", protocol.AttrItalic, uv.AttrItalic},
		{"faint", protocol.AttrFaint, uv.AttrFaint},
		{"blink", protocol.AttrBlink, uv.AttrBlink},
		{"reverse", protocol.AttrReverse, uv.AttrReverse},
		{"strike", protocol.AttrStrike, uv.AttrStrikethrough},
		{"conceal", protocol.AttrConceal, uv.AttrConceal},
		{"faint+bold", protocol.AttrFaint | protocol.AttrBold, uv.AttrFaint | uv.AttrBold},
	}
	for _, c := range cases {
		style := protocol.PackStyle(c.proto, 0, false, 0, false, false, 0, false)
		got := uvCellFromProto(protocol.Cell{Content: "x", Width: 1, Style: style}).Style.Attrs
		if got != c.want {
			t.Errorf("%s: got attrs %#b, want %#b", c.name, got, c.want)
		}
	}
}
