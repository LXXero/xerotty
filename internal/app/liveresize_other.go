//go:build !darwin

package app

// installLiveResizeWatch is a no-op on platforms where the main loop
// keeps running during WM-driven window resize. macOS is the odd one
// out because AppKit's live-resize tracking mode bypasses
// NSDefaultRunLoopMode and starves SDL's pump.
func installLiveResizeWatch(bgR, bgG, bgB float32, frame, beforeRender func()) {}

func updateLiveResizeBg(bgR, bgG, bgB float32) {}

func liveResizeMainFrameBegin() {}
func liveResizeMainFrameEnd()   {}
func inLiveResizeWatch() bool   { return false }
