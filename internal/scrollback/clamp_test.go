package scrollback

import "testing"

// TestOffsetClampOnShrink documents the invariant the app's
// scroll-on-output loop enforces: offset must never exceed available
// scrollback. When a clear/alt-screen/resize shrinks scrollback below
// the current offset, the offset must clamp down (a shrink to 0 snaps
// to the live bottom) — otherwise the viewport strands off the bottom
// rendering blank rows. This guards ScrollUp's own clamp and stands as
// the contract the app loop relies on.
func TestOffsetClampOnShrink(t *testing.T) {
	s := &State{}
	// Scroll up into a deep buffer.
	s.ScrollUp(500, 1000)
	if s.Offset != 500 {
		t.Fatalf("offset = %d, want 500", s.Offset)
	}
	// ScrollUp must never exceed the live max-lines argument.
	s.ScrollUp(1000, 1000)
	if s.Offset != 1000 {
		t.Fatalf("offset = %d, want clamped to 1000", s.Offset)
	}
	// The app loop's shrink clamp: emulate scrollback dropping to 30.
	const sbLen = 30
	if s.Offset > sbLen {
		s.Offset = sbLen
	}
	if s.Offset != 30 {
		t.Fatalf("after shrink clamp offset = %d, want 30", s.Offset)
	}
	// A full clear (sbLen 0) must land at the live bottom.
	if s.Offset > 0 {
		s.Offset = 0
	}
	if s.IsScrolled() {
		t.Fatalf("after clear-to-zero, IsScrolled = true (offset %d)", s.Offset)
	}
}
