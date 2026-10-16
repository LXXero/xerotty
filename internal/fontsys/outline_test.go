package fontsys

import "testing"

// TestFindOutlineForCodepoint: ✳ U+2733 also has an emoji presentation,
// so the color-preferring FindForCodepoint can hand back NotoColorEmoji,
// which ImGui's stb_truetype atlas draws as nothing. The outline lookup
// must return a font that has the glyph and rasterizes it monochrome.
func TestFindOutlineForCodepoint(t *testing.T) {
	for _, r := range []rune{'✳', '◐', '⚠'} {
		path := FindOutlineForCodepoint(r, "")
		if path == "" {
			t.Skipf("no outline font for %c on this box", r)
		}
		font, err := Default.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		if !font.Has(r) {
			font.Close()
			t.Fatalf("%s lacks %c", path, r)
		}
		g, err := font.Rasterize(r, 18)
		font.Close()
		if err != nil || g == nil {
			t.Fatalf("rasterize %c from %s: %v", r, path, err)
		}
		if g.IsColor {
			t.Errorf("%c: %s is a color font", r, path)
		}
		t.Logf("%c → %s", r, path)
	}
}
