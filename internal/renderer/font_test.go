package renderer

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestUIFallbacksSkipCoveredRunes: a probe rune the primary already has
// must not pull in a fallback font. Only the runes it lacks are looked up.
func TestUIFallbacksSkipCoveredRunes(t *testing.T) {
	gap := map[rune]string{'✳': "/fonts/A.ttf", '◐': "/fonts/B.ttf", '●': "/fonts/B.ttf"}
	has := func(r rune) bool { _, ok := gap[r]; return !ok }
	var looked []rune
	find := func(r rune, _ string) string {
		looked = append(looked, r)
		if p, ok := gap[r]; ok {
			return p
		}
		return "/fonts/Unwanted.ttf"
	}

	got := uiFallbacksFor("/fonts/Primary.ttf", has, find, "/nonexistent/Menlo.ttc")
	if want := []string{"/fonts/A.ttf", "/fonts/B.ttf"}; !slices.Equal(got, want) {
		t.Errorf("fallbacks = %v, want %v", got, want)
	}
	if want := []rune{'✳', '◐', '●'}; !slices.Equal(looked, want) {
		t.Errorf("looked up %q, want only the missing runes %q", looked, want)
	}
}

// TestUIFallbacksNoneWhenPrimaryCoversAll: a primary with every probe rune
// gets no merge fonts, not even the macOS Menlo fallback.
func TestUIFallbacksNoneWhenPrimaryCoversAll(t *testing.T) {
	menlo := filepath.Join(t.TempDir(), "Menlo.ttc")
	if err := os.WriteFile(menlo, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	find := func(rune, string) string { t.Fatal("lookup for a covered rune"); return "" }
	if got := uiFallbacksFor("/fonts/Primary.ttf", func(rune) bool { return true }, find, menlo); len(got) != 0 {
		t.Errorf("fallbacks = %v, want none", got)
	}
	got := uiFallbacksFor("/fonts/Primary.ttf", func(r rune) bool { return r != '✳' }, func(rune, string) string { return "" }, menlo)
	if want := []string{menlo}; !slices.Equal(got, want) {
		t.Errorf("fallbacks = %v, want %v", got, want)
	}
}
