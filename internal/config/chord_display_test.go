package config

import "testing"

// TestChordDisplayRoundTrip pins the display<->storage chord mapping
// the keybind editor rides. Storage space is what matchKeybind fires
// on; display space is what the user sees and types. On darwin the
// ImGui Cmd<->Ctrl swap means storage "Ctrl" = physical Cmd (shown
// "Cmd") and storage "Super" = physical Ctrl (shown "Ctrl") — before
// this mapping, a mac user typing Ctrl+T got a bind firing on Cmd+T.
func TestChordDisplayRoundTrip(t *testing.T) {
	cases := []struct {
		goos    string
		storage string
		display string
	}{
		{"linux", "Ctrl+Shift+T", "Ctrl+Shift+T"},
		{"linux", "Ctrl+Comma", "Ctrl+,"},
		{"linux", "Super+K", "Super+K"},
		{"darwin", "Ctrl+Shift+T", "Cmd+Shift+T"},
		{"darwin", "Super+K", "Ctrl+K"},
		{"darwin", "Ctrl+Comma", "Cmd+,"},
		{"darwin", "Ctrl+Period", "Cmd+."},
	}
	for _, c := range cases {
		if got := prettifyChord(c.storage, c.goos); got != c.display {
			t.Errorf("prettify(%s,%q) = %q, want %q", c.goos, c.storage, got, c.display)
		}
		if got := normalizeChord(c.display, c.goos); got != c.storage {
			t.Errorf("normalize(%s,%q) = %q, want %q", c.goos, c.display, got, c.storage)
		}
	}
	// Storage-space input passes through normalize unchanged on linux
	// (raw chords typed by hand keep working).
	if got := normalizeChord("Ctrl+Shift+T", "linux"); got != "Ctrl+Shift+T" {
		t.Errorf("linux storage passthrough broke: %q", got)
	}
}
