//go:build darwin

package platform

/*
#cgo darwin LDFLAGS: -framework Cocoa
#include "cellsnap.h"
*/
import "C"

// SetResizeIncrements installs (incW, incH) as the NSWindow content
// resize increments via Cocoa's setContentResizeIncrements:, so
// AppKit constrains user-drag resize to whole-cell boundaries.
// Same idea as XSetWMNormalHints+PResizeInc on X11.
//
// windowID is the SDL_WindowID stored in ImGuiViewport.PlatformHandle.
// Returns false if the native window is not ready yet.
func SetResizeIncrements(windowID uintptr, incW, incH int) bool {
	return C.platform_set_resize_increments(C.ulong(windowID), C.int(incW), C.int(incH)) != 0
}
