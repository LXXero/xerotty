// Package cellconv converts between ultraviolet.Cell (the in-memory
// cell model) and protocol.Cell (the wire/handoff encoding) — BOTH
// directions, in ONE place. The encode side grew up in
// internal/daemon and the decode side in internal/daemonsource;
// when the hot-upgrade resume path needed decode daemon-side, that
// duplication had to die (the attr-bit remap below is exactly the
// kind of subtlety that drifts when copied).
package cellconv

import (
	"image/color"

	"github.com/LXXero/xerotty/internal/protocol"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// FromUV converts an in-memory cell to its wire encoding. Handles
// palette vs RGB color packing. Nil cells (off-screen, empty)
// become a space with default style.
func FromUV(c *uv.Cell) protocol.Cell {
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

// ToUV converts a wire cell back to the ultraviolet.Cell the
// emulator expects — the exact inverse of FromUV, including the
// per-bit attr remap: protocol's attr bit layout
// (Bold,Italic,Faint,…) does NOT match ultraviolet's
// (Bold,Faint,Italic,…), so the raw bits must be translated, not
// copied. Copying them straight swapped faint↔italic and
// reverse/strike, which made faint text render un-dimmed.
//
// Width 0 stays a TRUE zero cell (wide-cell placeholder): forcing
// it to a width-1 space made scrollback rows overdraw the wide
// glyph's right half, and viewport applies (via Line.Set's
// partial-overwrite handling) blank the glyph entirely.
func ToUV(p protocol.Cell) uv.Cell {
	attrs, ulStyle, fgSet, fgIdx, fgIsRGB, bgSet, bgIdx, bgIsRGB := protocol.UnpackStyle(p.Style)
	style := uv.Style{
		Attrs:     attrsToUV(attrs),
		Underline: uv.Underline(ulStyle),
	}
	if fgSet {
		if fgIsRGB {
			style.Fg = rgbColor(p.FgRGB)
		} else {
			// Includes palette index 0 (ANSI black). The fgSet
			// bit is what distinguishes that from "no color".
			style.Fg = ansi.ExtendedColor(fgIdx)
		}
	}
	if bgSet {
		if bgIsRGB {
			style.Bg = rgbColor(p.BgRGB)
		} else {
			style.Bg = ansi.ExtendedColor(bgIdx)
		}
	}
	c := uv.Cell{
		Content: p.Content,
		Style:   style,
		Width:   int(p.Width),
	}
	if c.Width == 0 {
		return uv.Cell{}
	}
	if c.Content == "" {
		c.Content = " "
	}
	return c
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

// attrsToUV translates protocol attr bits to ultraviolet attr bits
// — explicit per-flag mapping, see ToUV's doc for why.
func attrsToUV(a uint32) uint8 {
	var out uint8
	if a&protocol.AttrBold != 0 {
		out |= uv.AttrBold
	}
	if a&protocol.AttrItalic != 0 {
		out |= uv.AttrItalic
	}
	if a&protocol.AttrFaint != 0 {
		out |= uv.AttrFaint
	}
	if a&protocol.AttrBlink != 0 {
		out |= uv.AttrBlink
	}
	if a&protocol.AttrReverse != 0 {
		out |= uv.AttrReverse
	}
	if a&protocol.AttrStrike != 0 {
		out |= uv.AttrStrikethrough
	}
	if a&protocol.AttrConceal != 0 {
		out |= uv.AttrConceal
	}
	return out
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

func rgbColor(rgb uint32) color.Color {
	return color.RGBA{
		R: uint8(rgb >> 16),
		G: uint8(rgb >> 8),
		B: uint8(rgb),
		A: 0xff,
	}
}
