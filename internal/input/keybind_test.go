package input

import (
	"strings"
	"testing"

	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/config"
)

// keyPartOf strips modifier prefixes the same way matchKeybind does,
// leaving the bare key name.
func keyPartOf(bind string) string {
	kp := bind
	for {
		switch {
		case strings.HasPrefix(kp, "Ctrl+"):
			kp = kp[5:]
		case strings.HasPrefix(kp, "Shift+"):
			kp = kp[6:]
		case strings.HasPrefix(kp, "Alt+"):
			kp = kp[4:]
		case strings.HasPrefix(kp, "Cmd+"):
			kp = kp[4:]
		case strings.HasPrefix(kp, "Super+"):
			kp = kp[6:]
		default:
			return kp
		}
	}
}

// TestEveryDefaultKeybindResolves guards the whole "shortcut silently
// does nothing" bug class: every key name in the platform default
// keybind maps must resolve to a real ImGui key. nameToImGuiKey
// returning KeyNone means matchKeybind can never fire that bind — how
// Ctrl+Comma (preferences) was dead until "Comma" was added.
func TestEveryDefaultKeybindResolves(t *testing.T) {
	// Default() returns the current platform's map; also exercise the
	// raw maps so CI on Linux still validates the darwin bindings.
	for _, m := range []map[string]string{
		config.Default().Keybinds,
		config.DefaultKeybindsForTest("linux"),
		config.DefaultKeybindsForTest("darwin"),
	} {
		for bind, action := range m {
			kp := keyPartOf(bind)
			if nameToImGuiKey(kp) == imgui.KeyNone {
				t.Errorf("keybind %q (%s): key name %q does not resolve — action is dead",
					bind, action, kp)
			}
		}
	}
}
