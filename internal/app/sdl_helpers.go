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
