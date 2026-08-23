//go:build !linux

package terminal

// displayEnvDefaults is Linux-only: macOS GUIs don't use DISPLAY-style
// env at all (launchd's locale gap is handled separately in spawnPTY),
// and BSDs are untested territory — better to leave env untouched than
// to guess.
func displayEnvDefaults() []string { return nil }
