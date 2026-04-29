// Package sdlhack provides macOS workarounds for SDL2 input quirks that
// cimgui-go's standard backend doesn't handle. SDL2 + Cocoa occasionally
// drops mouse-up events after a window-focus shift, leaving SDL with a
// stuck "button still held" state until the user app-switches. We bypass
// the event queue by reading the OS-level button state directly.
package sdlhack

/*
#cgo pkg-config: sdl2
#include <SDL2/SDL.h>
*/
import "C"

// LeftButtonGlobalDown queries the OS-level (not SDL-event-queue-cached)
// state of the left mouse button. Returns true iff the button is currently
// physically held according to the OS.
func LeftButtonGlobalDown() bool {
	var x, y C.int
	state := C.SDL_GetGlobalMouseState(&x, &y)
	return uint32(state)&C.SDL_BUTTON_LMASK != 0
}
