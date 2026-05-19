//go:build darwin

package app

// macOS live-resize smooth-render watch — SDL3 stub.
//
// The old SDL2-backed implementation hooked SDL_AddEventWatch to drive
// full frames during AppKit's tracking-mode runloop (which bypasses
// NSDefaultRunLoopMode and starves the main pump). It called directly
// into cimgui-go's bundled ImGui_ImplSDL2 backend.
//
// Once the main loop migrated to the SDL3-backed internal/platform
// layer (uses ImGui_ImplSDL3, not ImGui_ImplSDL2), the SDL2 code here
// became incompatible — linking both SDL2 and SDL3 produced ambiguous
// symbol resolution that made SDL_Init silently fail with no error.
//
// The live-resize watch needs to be re-implemented against
// internal/platform's frame primitives; until then these are no-ops
// (so cgo build doesn't pull in libSDL2 alongside libSDL3). Resize on
// macOS still works, just without the smooth-during-drag rendering —
// content snaps to the new size at the end of the drag instead of
// updating continuously.
//
// TODO: reimplement smooth live-resize on top of internal/platform's
// SDL3 event watch + platform.Frame primitives.

func installLiveResizeWatch(bgR, bgG, bgB float32, frame, beforeRender func()) {}

func updateLiveResizeBg(bgR, bgG, bgB float32) {}

func liveResizeMainFrameBegin() {}
func liveResizeMainFrameEnd()   {}
func inLiveResizeWatch() bool   { return false }
