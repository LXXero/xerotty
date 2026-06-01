package daemon

import (
	"image/color"

	"github.com/LXXero/xerotty/internal/protocol"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// cellFromUV converts an ultraviolet.Cell (the daemon's in-memory
// representation) into a wire-format protocol.Cell. Handles palette
// vs RGB color packing.
//
// Nil cells (off-screen, empty) become a space with default style.
func cellFromUV(c *uv.Cell) protocol.Cell {
	if c == nil {
		return protocol.Cell{Content: " ", Width: 1}
	}
	content := c.Content
	if content == "" {
		content = " "
	}
	attrs := attrsFromStyle(&c.Style)
	underline := underlineFromStyle(&c.Style)
	fgSet, fgIdx, fgRGB, fg := colorFromStyle(c.Style.Fg)
	bgSet, bgIdx, bgRGB, bg := colorFromStyle(c.Style.Bg)
	out := protocol.Cell{
		Content: content,
		Width:   uint8(c.Width),
		Style:   protocol.PackStyle(attrs, underline, fgSet, fgIdx, fgRGB, bgSet, bgIdx, bgRGB),
	}
	if fgRGB {
		out.FgRGB = fg
	}
	if bgRGB {
		out.BgRGB = bg
	}
	return out
}

func attrsFromStyle(s *uv.Style) uint32 {
	var a uint32
	if s.Attrs&uv.AttrBold != 0 {
		a |= protocol.AttrBold
	}
	if s.Attrs&uv.AttrItalic != 0 {
		a |= protocol.AttrItalic
	}
	if s.Attrs&uv.AttrFaint != 0 {
		a |= protocol.AttrFaint
	}
	if s.Attrs&uv.AttrBlink != 0 || s.Attrs&uv.AttrRapidBlink != 0 {
		a |= protocol.AttrBlink
	}
	if s.Attrs&uv.AttrReverse != 0 {
		a |= protocol.AttrReverse
	}
	if s.Attrs&uv.AttrStrikethrough != 0 {
		a |= protocol.AttrStrike
	}
	if s.Attrs&uv.AttrConceal != 0 {
		a |= protocol.AttrConceal
	}
	return a
}

func underlineFromStyle(s *uv.Style) uint8 {
	switch s.Underline {
	case ansi.UnderlineSingle:
		return protocol.UnderlineSingle
	case ansi.UnderlineDouble:
		return protocol.UnderlineDouble
	case ansi.UnderlineCurly:
		return protocol.UnderlineCurly
	case ansi.UnderlineDotted:
		return protocol.UnderlineDotted
	case ansi.UnderlineDashed:
		return protocol.UnderlineDashed
	default:
		return protocol.UnderlineNone
	}
}

// colorFromStyle classifies a color.Color from ultraviolet.Style as
// (set-flag, palette-idx, RGB-flag, RGB-value). Palette index covers
// the standard xterm 256-color palette; anything that doesn't
// pattern-match the ansi color enums gets RGB-encoded. The set-flag
// distinguishes nil (no color) from palette index 0 (ANSI black) so
// SGR black doesn't decode as "use default color."
func colorFromStyle(col color.Color) (set bool, idx uint16, isRGB bool, rgb uint32) {
	if col == nil {
		return false, 0, false, 0
	}
	switch v := col.(type) {
	case ansi.BasicColor:
		return true, uint16(v), false, 0
	case ansi.IndexedColor: // ansi.ExtendedColor is a type alias for this
		return true, uint16(v), false, 0
	case ansi.TrueColor:
		// 0xRRGGBB packing
		r, g, b, _ := v.RGBA()
		rgb = (uint32(r>>8) << 16) | (uint32(g>>8) << 8) | uint32(b>>8)
		return true, 0, true, rgb
	default:
		// Unknown color flavor — fall back to RGB extracted from
		// the standard color interface.
		r, g, b, _ := col.RGBA()
		rgb = (uint32(r>>8) << 16) | (uint32(g>>8) << 8) | uint32(b>>8)
		return true, 0, true, rgb
	}
}
