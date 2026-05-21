//go:build darwin

package platform

/*
#cgo LDFLAGS: -framework Cocoa
#include "cocoa_focus.h"
*/
import "C"

// CocoaFocusWindow pulls OS keyboard focus to the SDL_Window with the
// given ID via direct AppKit calls (bypassing SDL_RaiseWindow which
// is unreliable on macOS for this purpose — focus only transitions
// after the next mouse event). darwin-only.
func CocoaFocusWindow(windowID uintptr) {
	C.platform_cocoa_focus_window(C.ulong(windowID))
}

// CocoaWindowZRank returns the front-to-back order of a visible NSWindow:
// 0 is frontmost. Returns -1 if the SDL/NSWindow cannot be found.
func CocoaWindowZRank(windowID uintptr) int {
	return int(C.platform_cocoa_window_z_rank(C.ulong(windowID)))
}

// CocoaWindowInLiveResize reports whether the SDL_Window's backing
// NSWindow is inside AppKit's live-resize tracking loop.
func CocoaWindowInLiveResize(windowID uintptr) bool {
	return C.platform_cocoa_window_in_live_resize(C.ulong(windowID)) != 0
}

// CocoaEventOnChrome reports whether the most recent NSEvent
// (NSApp.currentEvent) is a mouse event whose location is on window
// chrome (title bar / resize edges) rather than inside the
// contentView. Used by the mouse mirror to skip synthetic DOWN
// injection during a title-bar drag.
func CocoaEventOnChrome() bool {
	return C.platform_cocoa_event_on_chrome() != 0
}

// CocoaAnyWindowMoved returns true if any of our visible NSWindows
// has moved (frame.origin changed) since the last call. Reads the
// live AppKit state directly, bypassing the ImGui-viewport.Pos
// staleness during continuous drags.
func CocoaAnyWindowMoved() bool {
	return C.platform_cocoa_any_window_moved() != 0
}
