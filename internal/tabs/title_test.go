package tabs

import "testing"

func TestIsShellProcess(t *testing.T) {
	shells := []string{"zsh", "bash", "-zsh", "-bash", "fish", "sh", "dash", "nu"}
	for _, s := range shells {
		if !isShellProcess(s) {
			t.Errorf("isShellProcess(%q) = false, want true", s)
		}
	}
	apps := []string{"vim", "nvim", "less", "top", "claude", "node", "python", ""}
	for _, a := range apps {
		if isShellProcess(a) {
			t.Errorf("isShellProcess(%q) = true, want false", a)
		}
	}
}
