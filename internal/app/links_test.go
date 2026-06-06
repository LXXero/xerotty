package app

import "testing"

// TestDetectLinkInScrollback: link hit-testing must work on rows
// scrolled up into history. Regression: detectLinkAt read the raw
// shadow emulator, whose ScrollbackLen is 0 for daemon tabs (the
// Source mirrors scrollback in its own ring) — scrolled-up rows
// computed negative content indices and links died above the live
// screen.
func TestDetectLinkInScrollback(t *testing.T) {
	g := &fakeGrid{
		sb: []string{
			"see https://example.com/x for details",
			"another history line",
		},
		screen: []string{"prompt$ ", "", ""},
		cols:   45,
	}

	// Scrolled up 2: viewport row 0 = scrollback row 0 (the URL line).
	hit := detectLinkAt(g, 10, 0, 2)
	if hit == nil {
		t.Fatal("no link found in scrolled-back row")
	}
	if hit.URL != "https://example.com/x" {
		t.Fatalf("URL = %q", hit.URL)
	}

	// Same viewport coords, not scrolled: live screen row 0 has no URL.
	if hit := detectLinkAt(g, 10, 0, 0); hit != nil {
		t.Fatalf("unexpected link on live row: %q", hit.URL)
	}
}
