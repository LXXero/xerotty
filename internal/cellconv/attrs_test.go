package cellconv

import (
	"testing"

	"github.com/LXXero/xerotty/internal/protocol"
	uv "github.com/charmbracelet/ultraviolet"
)

// TestAttrsFromStyle pins the ultraviolet→protocol attribute remap.
// Paired with daemonsource's TestUvCellFromProtoAttrs, this locks the
// full wire round-trip so the two enums' differing bit orders can't
// silently swap faint↔italic / reverse / strike again.
func TestAttrsFromStyle(t *testing.T) {
	cases := []struct {
		name string
		uv   uint8
		want uint32
	}{
		{"bold", uv.AttrBold, protocol.AttrBold},
		{"italic", uv.AttrItalic, protocol.AttrItalic},
		{"faint", uv.AttrFaint, protocol.AttrFaint},
		{"blink", uv.AttrBlink, protocol.AttrBlink},
		{"rapidblink", uv.AttrRapidBlink, protocol.AttrBlink}, // folded into blink
		{"reverse", uv.AttrReverse, protocol.AttrReverse},
		{"strike", uv.AttrStrikethrough, protocol.AttrStrike},
		{"conceal", uv.AttrConceal, protocol.AttrConceal},
		{"faint+bold", uv.AttrFaint | uv.AttrBold, protocol.AttrFaint | protocol.AttrBold},
	}
	for _, c := range cases {
		got := attrsFromStyle(&uv.Style{Attrs: c.uv})
		if got != c.want {
			t.Errorf("%s: got attrs %#b, want %#b", c.name, got, c.want)
		}
	}
}
