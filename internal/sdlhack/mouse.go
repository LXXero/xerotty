// Package sdlhack provides macOS workarounds for SDL2 input quirks that
// cimgui-go's standard backend doesn't handle. SDL2 + Cocoa occasionally
// drops mouse-up events after a window-focus shift, leaving SDL with a
// stuck "button still held" state until the user app-switches. We bypass
// the event queue by reading the OS-level button state directly.
package sdlhack

/*
#cgo pkg-config: sdl2
#include <SDL2/SDL.h>

static int xerotty_mouse_in_main_content(void) {
	int gx, gy;
	Uint32 state = SDL_GetGlobalMouseState(&gx, &gy);
	(void)state;
	SDL_Window *win = SDL_GL_GetCurrentWindow();
	if (!win) return 0;
	int wx, wy, ww, wh;
	SDL_GetWindowPosition(win, &wx, &wy);
	SDL_GetWindowSize(win, &ww, &wh);
	if (gx < wx || gx >= wx + ww) return 0;
	if (gy < wy || gy >= wy + wh) return 0;
	return 1;
}
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

// MouseInMainContent reports whether the cursor is currently inside
// the content rect of the main SDL window (the window owning the GL
// context). Used to avoid synthesizing fake terminal clicks when the
// real click landed on a window frame, resize handle, or popped-out
// viewport — areas AppKit consumes events for and never delivers to
// SDL.
func MouseInMainContent() bool {
	return C.xerotty_mouse_in_main_content() != 0
}
