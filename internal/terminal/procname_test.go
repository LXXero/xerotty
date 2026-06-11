//go:build darwin

package terminal

import "testing"

// Version-named binaries (Claude Code's executable is literally
// ".../versions/2.1.172") must not become tab titles — the kernel
// name gates an argv[0] resolution when it looks like a version.
func TestLooksLikeVersion(t *testing.T) {
	yes := []string{"2.1.172", "1.0", "26.3.1", "7"}
	no := []string{"claude", "node", "python3.12", "v2.1.172", "", "...", "2-1"}
	for _, s := range yes {
		if !looksLikeVersion(s) {
			t.Errorf("looksLikeVersion(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if looksLikeVersion(s) {
			t.Errorf("looksLikeVersion(%q) = true, want false", s)
		}
	}
}
