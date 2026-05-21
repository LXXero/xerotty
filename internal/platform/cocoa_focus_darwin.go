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
