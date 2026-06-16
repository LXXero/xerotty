package renderer

import "testing"

// A color emoji must never be drawn wider than its cell box, or it
// bleeds onto the adjacent glyph (the 🐱🔧 / 🐾✨ overlap bug). Covers
// a square emoji in a typical ~1:2.2 monospace cell, both 1- and
// 2-cell wide.
func TestColorGlyphNeverExceedsCellBox(t *testing.T) {
	const cellBaseW, cellH = 7.2, 16.0 // NotoSansMono @ 12-ish
	const glyphW, glyphH = 128.0, 128.0
	for _, width := range []int{1, 2} {
		boxW := cellBaseW * float32(width)
		_, _, gotW, _ := fitColorGlyphToCell(0, 0, boxW, cellH, glyphW, glyphH, 1)
		if gotW > boxW+0.01 {
			t.Errorf("width=%d: drawn %.2f exceeds box %.2f (would overlap neighbor)", width, gotW, boxW)
		}
	}
}
