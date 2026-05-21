//go:build !darwin

package platform

// Non-darwin: no Cocoa, so RaiseWindow is the only available path.
// app.go calls CocoaFocusWindow unconditionally — make it a no-op
// here so the Linux/Windows builds don't fail.
func CocoaFocusWindow(_ uintptr) {}
