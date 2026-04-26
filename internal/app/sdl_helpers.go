package app

/*
#cgo pkg-config: sdl2
#include <SDL2/SDL.h>

void xerottySetFullscreen(int enable) {
	SDL_Window *win = SDL_GL_GetCurrentWindow();
	if (win) {
		if (enable) {
			SDL_SetWindowFullscreen(win, SDL_WINDOW_FULLSCREEN_DESKTOP);
		} else {
			SDL_SetWindowFullscreen(win, 0);
		}
	}
}

void xerottyQuit(void) {
	SDL_Event event;
	event.type = SDL_QUIT;
	SDL_PushEvent(&event);
}

// SDL_SetWindowSize on X11 (and some other WMs) can leave the window
// without keyboard focus after the unmap/remap cycle the WM uses to
// honour size changes. Raise + request input focus to recover it.
void xerottyRaiseWindow(void) {
	SDL_Window *win = SDL_GL_GetCurrentWindow();
	if (win) {
		SDL_RaiseWindow(win);
		SDL_SetWindowInputFocus(win);
	}
}

void xerottySetResizable(int resizable) {
	SDL_Window *win = SDL_GL_GetCurrentWindow();
	if (win) {
		SDL_SetWindowResizable(win, resizable ? SDL_TRUE : SDL_FALSE);
	}
}

void xerottySetCursor(int cursorID) {
	static SDL_Cursor *cursors[SDL_NUM_SYSTEM_CURSORS];
	if (cursorID < 0 || cursorID >= SDL_NUM_SYSTEM_CURSORS) return;
	if (!cursors[cursorID]) {
		cursors[cursorID] = SDL_CreateSystemCursor((SDL_SystemCursor)cursorID);
	}
	SDL_SetCursor(cursors[cursorID]);
}

void xerottyGetWindowPos(int *x, int *y) {
	SDL_Window *win = SDL_GL_GetCurrentWindow();
	if (win) {
		SDL_GetWindowPosition(win, x, y);
	}
}
*/
import "C"

func sdlSetFullscreen(enable bool) {
	if enable {
		C.xerottySetFullscreen(1)
	} else {
		C.xerottySetFullscreen(0)
	}
}

func sdlQuit() {
	C.xerottyQuit()
}

func sdlRaiseWindow() {
	C.xerottyRaiseWindow()
}

func sdlSetResizable(resizable bool) {
	if resizable {
		C.xerottySetResizable(1)
	} else {
		C.xerottySetResizable(0)
	}
}

const (
	sdlCursorArrow    = int(C.SDL_SYSTEM_CURSOR_ARROW)
	sdlCursorIBeam    = int(C.SDL_SYSTEM_CURSOR_IBEAM)
	sdlCursorHand     = int(C.SDL_SYSTEM_CURSOR_HAND)
	sdlCursorSizeNS   = int(C.SDL_SYSTEM_CURSOR_SIZENS)
	sdlCursorSizeWE   = int(C.SDL_SYSTEM_CURSOR_SIZEWE)
	sdlCursorSizeNESW = int(C.SDL_SYSTEM_CURSOR_SIZENESW)
	sdlCursorSizeNWSE = int(C.SDL_SYSTEM_CURSOR_SIZENWSE)
)

func sdlSetCursor(cursorID int) {
	C.xerottySetCursor(C.int(cursorID))
}

func sdlGetWindowPos() (int, int) {
	var x, y C.int
	C.xerottyGetWindowPos(&x, &y)
	return int(x), int(y)
}
