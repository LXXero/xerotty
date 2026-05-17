// Package sdlhack provides macOS workarounds for SDL2 input quirks that
// cimgui-go's standard backend doesn't handle. SDL2 + Cocoa occasionally
// drops mouse-up events after a window-focus shift, leaving SDL with a
// stuck "button still held" state until the user app-switches. We bypass
// the event queue by reading the OS-level button state directly.
package sdlhack

/*
#cgo pkg-config: sdl2
#include <SDL2/SDL.h>

// Returns 1 iff the OS-level cursor position is inside the content
// rect of whichever SDL_Window currently has mouse focus (any of the
// app's windows under multi-viewport — main or popped-out secondary).
// 0 if the cursor is on a window frame / title bar / desktop, which
// means AppKit consumed any click there and we shouldn't synthesize
// a fake terminal click out of it.
static int xerotty_mouse_in_window_content(void) {
	int gx, gy;
	SDL_GetGlobalMouseState(&gx, &gy);
	SDL_Window *win = SDL_GetMouseFocus();
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

// GlobalMousePos returns the current OS-level cursor position in
// screen coordinates. Independent of SDL's event-queue-cached
// position, so it stays accurate even when SDL drops mouse events
// during Cocoa focus shifts.
func GlobalMousePos() (int, int) {
	var x, y C.int
	C.SDL_GetGlobalMouseState(&x, &y)
	return int(x), int(y)
}

// MouseInMainContent reports whether the cursor is currently inside
// the content rect of whichever SDL_Window in this process currently
// has mouse focus — the primary window OR any popped-out multi-
// viewport window. Used to avoid synthesizing fake terminal clicks
// when the real click landed on a window frame, resize handle, or
// any non-content area AppKit consumes events for and never delivers
// to SDL.
func MouseInMainContent() bool {
	return C.xerotty_mouse_in_window_content() != 0
}
