//go:build !darwin

package platform

// Non-darwin: no Cocoa, so RaiseWindow is the only available path.
// app.go calls CocoaFocusWindow unconditionally — make it a no-op
// here so the Linux/Windows builds don't fail.
func CocoaFocusWindow(_ uintptr)             {}
func CocoaWindowZRank(_ uintptr) int         { return -1 }
func CocoaWindowInLiveResize(_ uintptr) bool { return false }

// On non-darwin there's no AppKit-style "chrome vs contentView"
// problem to solve — the mouse-mirror itself is darwin-only, but
// this stub keeps app.go calling CocoaEventOnChrome unconditionally
// from any path that might be shared cross-platform.
func CocoaEventOnChrome() bool  { return false }
func CocoaAnyWindowMoved() bool { return false }
