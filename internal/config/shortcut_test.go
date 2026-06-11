package config

import "testing"

// Menu shortcut labels derive from the keybinds map — the parallel
// hand-maintained Shortcut strings drifted (darwin showed
// "Cmd+Shift+R" long after the binding became Cmd+R).
func TestShortcutForAction(t *testing.T) {
	kb := map[string]string{
		"Ctrl+Shift+R":    "rename_tab",
		"Ctrl+Plus":       "font_size_up",
		"Ctrl+Shift+Plus": "font_size_up",
		"Ctrl+Comma":      "preferences",
	}
	if got := ShortcutForAction(kb, "nope"); got != "" {
		t.Errorf("unbound action: got %q, want empty", got)
	}
	// fewest-modifier chord wins
	if got := prettifyChord("Ctrl+Plus", "linux"); got != "Ctrl+Plus" {
		t.Errorf("prettify linux: %q", got)
	}
	best := ""
	for chord, act := range kb {
		if act == "font_size_up" && (best == "" || chordLess(chord, best)) {
			best = chord
		}
	}
	if best != "Ctrl+Plus" {
		t.Errorf("chord preference: got %q, want Ctrl+Plus", best)
	}
	if got := prettifyChord("Ctrl+Shift+R", "darwin"); got != "Cmd+Shift+R" {
		t.Errorf("darwin transform: %q", got)
	}
	if got := prettifyChord("Ctrl+R", "darwin"); got != "Cmd+R" {
		t.Errorf("darwin transform: %q", got)
	}
	if got := prettifyChord("Ctrl+Comma", "linux"); got != "Ctrl+," {
		t.Errorf("comma prettify: %q", got)
	}
}
