package input

import "testing"

// TestValidChord pins the chord grammar the keybind editor validates
// against: optional modifier prefixes, then a key name the runtime
// matcher (nameToImGuiKey) actually knows — the editor must accept
// exactly what matchKeybind can fire.
func TestValidChord(t *testing.T) {
	valid := []string{
		"Ctrl+Shift+T", "F11", "Ctrl+Comma", "Shift+Insert",
		"Alt+1", "Cmd+Q", "Super+Left", "Ctrl+Shift+Plus",
	}
	for _, c := range valid {
		if !ValidChord(c) {
			t.Errorf("ValidChord(%q) = false, want true", c)
		}
	}
	invalid := []string{
		"", "Ctrl+", "Bogus+X", "Ctrl+NotAKey", "ctrl+shift+t", "Ctrl +T",
	}
	for _, c := range invalid {
		if ValidChord(c) {
			t.Errorf("ValidChord(%q) = true, want false", c)
		}
	}
}
