package glyphcache

import (
	"testing"

	"github.com/LXXero/xerotty/internal/fontsys"
	"github.com/LXXero/xerotty/internal/platform"
)

// fakeFont has the runes in its set.
type fakeFont struct {
	path   string
	runes  map[rune]bool
	closed bool
}

func (f *fakeFont) Has(r rune) bool                                 { return f.runes[r] }
func (f *fakeFont) Rasterize(rune, float32) (*fontsys.Glyph, error) { return nil, nil }
func (f *fakeFont) LineMetrics(float32) fontsys.LineMetrics         { return fontsys.LineMetrics{} }
func (f *fakeFont) Bold() fontsys.Font                              { return nil }
func (f *fakeFont) Close()                                          { f.closed = true }

// fakeSystem is an installed-font list plus a fixed cascade: cascade[r]
// is the path FindForCodepoint hands back for r (what CoreText or
// fontconfig would pick).
type fakeSystem struct {
	fonts   []fontsys.FontInfo
	runes   map[string][]rune // path → runes the font has
	cascade map[rune]string
	opened  map[string]*fakeFont
}

func (s *fakeSystem) Enumerate() ([]fontsys.FontInfo, error) { return s.fonts, nil }
func (s *fakeSystem) FindForCodepoint(r rune, _ string) (string, error) {
	return s.cascade[r], nil
}
func (s *fakeSystem) Open(path string) (fontsys.Font, error) {
	f := &fakeFont{path: path, runes: map[rune]bool{}}
	for _, r := range s.runes[path] {
		f.runes[r] = true
	}
	if s.opened == nil {
		s.opened = map[string]*fakeFont{}
	}
	s.opened[path] = f
	return f, nil
}

const (
	monaco    = "/System/Library/Fonts/Monaco.ttf"
	stixMath  = "/System/Library/Fonts/Supplemental/STIXTwoMath.otf"
	nerdSyms  = "/Users/xero/Library/Fonts/SymbolsNerdFontMono-Regular.ttf"
	hackNF    = "/usr/share/fonts/TTF/HackNF.ttf" // family-only match
	branchPUA = rune(0xE0A0)                      // Powerline branch icon
	integral  = rune(0x222B)                      // ∫, not private use
)

func newFakeCache(t *testing.T, s *fakeSystem) *Cache {
	t.Helper()
	c, err := New(s, platform.TextureManager{}, monaco, 12, 1)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func pathOf(f fontsys.Font) string {
	if f == nil {
		return ""
	}
	return f.(*fakeFont).path
}

// The bug on xAir: Monaco lacks U+E0A0, CoreText's cascade picks
// STIX Two Math (an unrelated math glyph there), no icon font is
// installed — the codepoint must resolve as missing, not as STIX.
func TestPrivateUseRejectsMathFont(t *testing.T) {
	s := &fakeSystem{
		fonts: []fontsys.FontInfo{
			{Family: "Monaco", Path: monaco},
			{Family: "STIX Two Math", Path: stixMath},
		},
		runes:   map[string][]rune{stixMath: {branchPUA, integral}},
		cascade: map[rune]string{branchPUA: stixMath, integral: stixMath},
	}
	c := newFakeCache(t, s)
	if got := c.fontFor(branchPUA); got != nil {
		t.Fatalf("U+E0A0 resolved to %s; want missing", pathOf(got))
	}
	// The same math font is still fine for a non-private-use rune.
	if got := pathOf(c.fontFor(integral)); got != stixMath {
		t.Fatalf("∫ resolved to %q; want %q", got, stixMath)
	}
}

// When the cascade picks a non-icon font but an icon font that has
// the codepoint is installed, that icon font wins.
func TestPrivateUseFindsInstalledIconFont(t *testing.T) {
	s := &fakeSystem{
		fonts: []fontsys.FontInfo{
			{Family: "Monaco", Path: monaco},
			{Family: "STIX Two Math", Path: stixMath},
			{Family: "Symbols Nerd Font Mono", Path: nerdSyms},
		},
		runes:   map[string][]rune{stixMath: {branchPUA}, nerdSyms: {branchPUA}},
		cascade: map[rune]string{branchPUA: stixMath},
	}
	c := newFakeCache(t, s)
	if got := pathOf(c.fontFor(branchPUA)); got != nerdSyms {
		t.Fatalf("U+E0A0 resolved to %q; want %q", got, nerdSyms)
	}
}

// An icon-font fallback the cascade picks is accepted, recognized by
// family name even when the file name says nothing.
func TestPrivateUseAcceptsIconFallback(t *testing.T) {
	s := &fakeSystem{
		fonts: []fontsys.FontInfo{
			{Family: "Monaco", Path: monaco},
			{Family: "Hack Nerd Font", Path: hackNF},
		},
		runes:   map[string][]rune{hackNF: {branchPUA}},
		cascade: map[rune]string{branchPUA: hackNF},
	}
	c := newFakeCache(t, s)
	if got := pathOf(c.fontFor(branchPUA)); got != hackNF {
		t.Fatalf("U+E0A0 resolved to %q; want %q", got, hackNF)
	}
}

// The primary font is always accepted for a private-use codepoint it
// has, whatever its name.
func TestPrivateUseAcceptsPrimary(t *testing.T) {
	s := &fakeSystem{
		fonts:   []fontsys.FontInfo{{Family: "Monaco", Path: monaco}},
		runes:   map[string][]rune{monaco: {branchPUA}},
		cascade: map[rune]string{branchPUA: stixMath},
	}
	c := newFakeCache(t, s)
	if got := pathOf(c.fontFor(branchPUA)); got != monaco {
		t.Fatalf("U+E0A0 resolved to %q; want the primary %q", got, monaco)
	}
}

// Icon fonts opened only to check for a codepoint are closed again.
func TestIconFontProbeClosesMisses(t *testing.T) {
	other := "/Library/Fonts/FontAwesome.otf" // sorts before nerdSyms, so it is probed first
	s := &fakeSystem{
		fonts: []fontsys.FontInfo{
			{Family: "Monaco", Path: monaco},
			{Family: "Font Awesome 6 Free", Path: other},
			{Family: "Symbols Nerd Font Mono", Path: nerdSyms},
		},
		runes:   map[string][]rune{stixMath: {branchPUA}, nerdSyms: {branchPUA}},
		cascade: map[rune]string{branchPUA: stixMath},
	}
	c := newFakeCache(t, s)
	if got := pathOf(c.fontFor(branchPUA)); got != nerdSyms {
		t.Fatalf("U+E0A0 resolved to %q; want %q", got, nerdSyms)
	}
	if f := s.opened[other]; f == nil || !f.closed {
		t.Fatalf("Font Awesome was opened for the probe and not closed")
	}
	if _, ok := c.fallbacks[other]; ok {
		t.Fatalf("a font without the codepoint was kept in fallbacks")
	}
}

func TestIconFontName(t *testing.T) {
	cases := map[string]bool{
		"Symbols Nerd Font Mono":        true,
		"JetBrainsMono Nerd Font":       true,
		"JetBrainsMonoNerdFont-Regular": true,
		"PowerlineSymbols":              true,
		"Font Awesome 6 Free":           true,
		"Material Design Icons":         true,
		"materialdesignicons-webfont":   true,
		"STIX Two Math":                 false,
		"STIXTwoMath":                   false,
		"Monaco":                        false,
		"Menlo":                         false,
		"DejaVu Sans":                   false,
	}
	for name, want := range cases {
		if got := iconFontName(name); got != want {
			t.Errorf("iconFontName(%q) = %v; want %v", name, got, want)
		}
	}
}

func TestIsPrivateUse(t *testing.T) {
	cases := map[rune]bool{
		0xDFFF: false, 0xE000: true, 0xE0A0: true, 0xF8FF: true, 0xF900: false,
		0xEFFFF: false, 0xF0000: true, 0xFFFFD: true, 0xFFFFE: false,
		0x100000: true, 0x10FFFD: true, 0x10FFFE: false,
		'a': false, 0x1F600: false,
	}
	for r, want := range cases {
		if got := isPrivateUse(r); got != want {
			t.Errorf("isPrivateUse(U+%04X) = %v; want %v", r, got, want)
		}
	}
}
