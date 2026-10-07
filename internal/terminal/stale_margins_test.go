package terminal

import (
	"testing"

	"github.com/charmbracelet/x/vt"
)

// TestStaleMarginsAfterShrinkDoNotPanic guards the vt/ultraviolet pins:
// an app re-sending its old-height DECSTBM right after a shrink, then
// scrolling, used to index past the screen buffer and panic the whole
// daemon (every xlin session lost, 2026-10-06). The fix lives in the
// forks; this makes sure a future re-pin doesn't bring the bug back.
func TestStaleMarginsAfterShrinkDoNotPanic(t *testing.T) {
	emu := vt.NewSafeEmulator(150, 49)
	_, _ = emu.Write([]byte("\x1b[1;49r"))
	emu.Resize(150, 48)
	_, _ = emu.Write([]byte("\x1b[1;49r\x1b[1;1H\x1bM"))
	_, _ = emu.Write([]byte("\x1b[?69h\x1b[1;200s\x1b[1;1H\x1bM"))
	if emu.Height() != 48 || emu.Width() != 150 {
		t.Fatalf("size after shrink = %dx%d", emu.Width(), emu.Height())
	}
}
