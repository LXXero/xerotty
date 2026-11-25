package fontsys

import "testing"

// The atlas lookup must find an outline font for braille (Codex's
// spinner frames) and must never hand back a color font.
func TestFindOutlineForCodepointDarwin(t *testing.T) {
	p := FindOutlineForCodepoint('⠸', "Menlo")
	if p == "" {
		t.Fatal("no outline font for U+2838")
	}
	f, err := Default.Open(p)
	if err != nil {
		t.Fatalf("open %s: %v", p, err)
	}
	defer f.Close()
	if !f.Has('⠸') {
		t.Fatalf("%s lacks U+2838", p)
	}
	t.Logf("U+2838 -> %s", p)
	if e := FindOutlineForCodepoint('😀', "Menlo"); e != "" {
		t.Fatalf("returned %s for U+1F600; the only macOS font with it is Apple Color Emoji", e)
	}
}
