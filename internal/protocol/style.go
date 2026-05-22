package protocol

// Cell.Style is a 32-bit packed value laid out as:
//
//   bits  0..6   attribute flags (Bold, Italic, Faint, Blink,
//                Reverse, Strike, Conceal)
//   bits  7..9   underline style enum (3 bits)
//   bit  10      "fg-is-RGB" flag — when set, Cell.FgRGB carries
//                the real color and FgPalette is ignored
//   bits 11..19  fg palette index (9 bits → 0..511, covers the
//                standard 256-color palette with room to grow)
//   bit  20      "bg-is-RGB" flag
//   bits 21..29  bg palette index (9 bits)
//   bit  30      "fg-is-set" flag — must be 1 for fg-RGB or fg-
//                palette to apply. When 0, fg is "default" (nil),
//                regardless of the other fg bits. Needed to
//                distinguish "no color set" from "palette index
//                0 (ANSI black)".
//   bit  31      "bg-is-set" flag — same semantics for bg.
//
// 99% of terminal output uses palette colors so a Cell on the wire
// is normally just the packed Style + content + width — no RGB
// fields, no overhead.

// Attribute bits packed into the low 7 bits of Cell.Style.
const (
	AttrBold    uint32 = 1 << 0
	AttrItalic  uint32 = 1 << 1
	AttrFaint   uint32 = 1 << 2
	AttrBlink   uint32 = 1 << 3
	AttrReverse uint32 = 1 << 4
	AttrStrike  uint32 = 1 << 5
	AttrConceal uint32 = 1 << 6
)

const attrMask uint32 = 0x7F

// Underline style enum lives in bits 7..9. Values mirror
// ultraviolet/ansi.UnderlineStyle.
const (
	UnderlineShift  uint32 = 7
	UnderlineMask   uint32 = 0x7 << UnderlineShift
	UnderlineNone   uint8  = 0
	UnderlineSingle uint8  = 1
	UnderlineDouble uint8  = 2
	UnderlineCurly  uint8  = 3
	UnderlineDotted uint8  = 4
	UnderlineDashed uint8  = 5
)

// Foreground/background field bits.
const (
	FgIsRGBBit     uint32 = 1 << 10
	FgPaletteShift uint32 = 11
	FgPaletteMask  uint32 = 0x1FF << FgPaletteShift

	BgIsRGBBit     uint32 = 1 << 20
	BgPaletteShift uint32 = 21
	BgPaletteMask  uint32 = 0x1FF << BgPaletteShift

	// FgIsSetBit / BgIsSetBit gate whether the fg/bg fields apply.
	// 0 = "default color" (the cell renders with whatever the
	// terminal's default foreground/background is). 1 = "use the
	// rgb or palette field." Required to distinguish "no color"
	// from "palette index 0 (ANSI black)".
	FgIsSetBit uint32 = 1 << 30
	BgIsSetBit uint32 = 1 << 31
)

// PackStyle composes a Style uint32 from its components. fgSet/bgSet
// indicate "a color was set" — when false, fg/bg are "default" and
// the fgIdx/fgRGB/bgIdx/bgRGB fields don't apply (must distinguish
// "no color set" from "ANSI black = palette index 0"). fgIdx/bgIdx
// are palette indices in 0..511 unless the corresponding fgRGB/bgRGB
// flag is true, in which case the caller is also expected to set
// Cell.FgRGB / Cell.BgRGB and the palette field is ignored on decode.
func PackStyle(attrs uint32, underline uint8, fgSet bool, fgIdx uint16, fgRGB bool, bgSet bool, bgIdx uint16, bgRGB bool) uint32 {
	var s uint32
	s |= attrs & attrMask
	s |= (uint32(underline) & 0x7) << UnderlineShift
	if fgSet {
		s |= FgIsSetBit
		if fgRGB {
			s |= FgIsRGBBit
		} else {
			s |= (uint32(fgIdx) & 0x1FF) << FgPaletteShift
		}
	}
	if bgSet {
		s |= BgIsSetBit
		if bgRGB {
			s |= BgIsRGBBit
		} else {
			s |= (uint32(bgIdx) & 0x1FF) << BgPaletteShift
		}
	}
	return s
}

// UnpackStyle is the inverse of PackStyle. fgSet/bgSet report
// whether a color was set; when false, the corresponding fg/bg
// component is "default" and fgIdx/fgRGB/bgIdx/bgRGB are zero.
func UnpackStyle(s uint32) (attrs uint32, underline uint8, fgSet bool, fgIdx uint16, fgRGB bool, bgSet bool, bgIdx uint16, bgRGB bool) {
	attrs = s & attrMask
	underline = uint8((s & UnderlineMask) >> UnderlineShift)
	fgSet = s&FgIsSetBit != 0
	bgSet = s&BgIsSetBit != 0
	if fgSet {
		fgRGB = s&FgIsRGBBit != 0
		if !fgRGB {
			fgIdx = uint16((s & FgPaletteMask) >> FgPaletteShift)
		}
	}
	if bgSet {
		bgRGB = s&BgIsRGBBit != 0
		if !bgRGB {
			bgIdx = uint16((s & BgPaletteMask) >> BgPaletteShift)
		}
	}
	return
}
