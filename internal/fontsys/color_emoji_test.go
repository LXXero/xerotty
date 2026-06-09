package fontsys

import "testing"

// TestColorEmojiIsColor: any glyph rendered from a color bitmap font
// must report IsColor, because IsColor gates the renderer's
// fit-to-cell downscaling — a bitmap STRIKE comes back at ~136px and
// without the flag renders at that full size, spilling past its cell.
// Regression for the giant-plug bug: the old detector sampled pixel
// colors (r!=g||g!=b) and flagged muted emoji like 🔌 as monochrome.
func TestColorEmojiIsColor(t *testing.T) {
	for _, r := range []rune{'🔌', '⚡', '🦀', '🍕', '🌋'} {
		path, err := Default.FindForCodepoint(r, "")
		if err != nil || path == "" {
			t.Skipf("no font on this box for %c (%v)", r, err)
		}
		font, err := Default.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		g, err := font.Rasterize(r, 18)
		font.Close()
		if err != nil || g == nil {
			t.Fatalf("rasterize %c: %v", r, err)
		}
		// A color emoji from a bitmap strike must be flagged color so
		// the renderer downscales it.
		if !g.IsColor {
			t.Errorf("%c rasterized %dx%d but IsColor=false — would render at full strike size",
				r, g.Width, g.Height)
		}
	}
}
