//go:build !darwin && !linux

package terminal

// processName fallback for platforms we don't have a foreground-PID
// resolution path for yet. Returns "" so tab title falls back to the
// shell-set OSC title or the configured default.
func processName(pid int) string {
	return ""
}
