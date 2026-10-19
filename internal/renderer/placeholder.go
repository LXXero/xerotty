package renderer

import (
	"unicode"

	"github.com/AllenDang/cimgui-go/imgui"
)

// needsPlaceholder reports whether a rune the glyph cache couldn't
// resolve should be drawn as a missing-glyph box ("tofu"). Runes that
// are meant to be invisible, or that combine with the rune before them,
// get nothing: a box there would sit on top of a correctly drawn base
// glyph (combining marks, variation selectors, ZWJ in emoji sequences)
// or show something where the text has nothing visible.
func needsPlaceholder(r rune) bool {
	switch {
	case r < 0x20, r == 0x7F: // C0 controls, DEL
		return false
	case r >= 0x80 && r <= 0x9F: // C1 controls
		return false
	case r >= 0xFE00 && r <= 0xFE0F, // variation selectors
		r >= 0xE0100 && r <= 0xE01EF: // variation selectors supplement
		return false
	case r == 0x200B, // zero width space
		r == 0x200C, // zero width non-joiner
		r == 0x200D, // zero width joiner
		r == 0x2060, // word joiner
		r == 0xFEFF: // zero width no-break space (BOM)
		return false
	}
	// Prepended concatenation marks (U+0600-0605 and friends) are
	// format characters that DO render visibly — keep them boxable.
	if unicode.Is(unicode.Prepended_Concatenation_Mark, r) {
		return true
	}
	if unicode.In(r,
		unicode.Mn, unicode.Me, unicode.Mc, // combining marks
		unicode.Cf, // format: soft hyphen, bidi controls, tags…
		unicode.Other_Default_Ignorable_Code_Point, // Hangul fillers, U+2065, …
		unicode.Variation_Selector,
		unicode.Zs, unicode.Zl, unicode.Zp, // spaces and separators draw nothing
	) {
		return false
	}
	return true
}

// drawMissingGlyph queues the missing-glyph box for a cell: an outlined
// rectangle inset inside the cell, in the fg color, spanning the cell
// width the character occupies (w is already 1 or 2 cells wide).
func (r *Renderer) drawMissingGlyph(drawList *imgui.DrawList, x, y, w, cellH float32, fg uint32) {
	t := floor32(cellH / 16)
	if t < 1 {
		t = 1
	}
	insetX := round32(w * 0.12)
	if insetX < 1 {
		insetX = 1
	}
	insetY := round32(cellH * 0.15)
	x0, x1 := x+insetX, x+w-insetX
	y0, y1 := y+insetY, y+cellH-insetY
	if x1-x0 < 2*t || y1-y0 < 2*t {
		return // cell too small for an outline
	}
	r.appendRect(drawList, x0, y0, x1, y0+t, fg)     // top
	r.appendRect(drawList, x0, y1-t, x1, y1, fg)     // bottom
	r.appendRect(drawList, x0, y0+t, x0+t, y1-t, fg) // left
	r.appendRect(drawList, x1-t, y0+t, x1, y1-t, fg) // right
}
