//go:build linux

package platform

/*
#include "cellsnap.h"
*/
import "C"

// SetResizeIncrements asks the window manager to constrain user-drag
// resize so the window's content area only changes in whole multiples
// of (incW, incH) — typically called by the app with the terminal's
// cell dimensions. Combined with an initially cell-aligned window
// size, every resized state lands on a cell boundary.
//
// Linux behavior split by SDL video driver:
//   - x11:     XSetWMNormalHints with PResizeInc (works on real Xorg
//     and on most XWayland-style compositors).
//   - wayland: NO-OP. Wayland has no protocol for resize increments
//     by design; compositor owns sizing. Software-side
//     cell-snap in the renderer covers this case.
//
// windowID is the SDL_WindowID stored in ImGuiViewport.PlatformHandle.
// Returns false if the native window is not ready yet.
func SetResizeIncrements(windowID uintptr, incW, incH int) bool {
	return C.platform_set_resize_increments(C.ulong(windowID), C.int(incW), C.int(incH)) != 0
}
