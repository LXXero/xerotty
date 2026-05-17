package app

/*
#cgo pkg-config: sdl2
#include <SDL2/SDL.h>
#include <stdint.h>

// Toggle fullscreen on a specific SDL_Window. Caller passes the SDL
// window ID (uint32) that ImGui's SDL2 backend stores in
// ImGuiViewport::PlatformHandle (the backend switched from raw
// SDL_Window* to the smaller WindowID in 2024-08; see upstream
// imgui_impl_sdl2.cpp:1107). We look up the SDL_Window* here so the
// Go side doesn't have to depend on SDL headers.
//
// Targeting THIS Window's SDL_Window matters because the cimgui-go
// primary SDL_Window is a hidden carrier — SDL_GL_GetCurrentWindow()
// would fullscreen the invisible one and look like a no-op.
static void xerottySetFullscreenOn(uintptr_t window_id, int enable) {
	SDL_Window *win = SDL_GetWindowFromID((Uint32)window_id);
	if (!win) return;
	if (enable) {
		SDL_SetWindowFullscreen(win, SDL_WINDOW_FULLSCREEN_DESKTOP);
	} else {
		SDL_SetWindowFullscreen(win, 0);
	}
}

static void xerottyQuit(void) {
	SDL_Event event;
	event.type = SDL_QUIT;
	SDL_PushEvent(&event);
}

// Hide the primary SDL_Window without destroying it. cimgui-go owns
// the SDL_Window so we can't SDL_DestroyWindow it. Hiding lets the
// process keep running with secondary multi-viewport windows visible
// when the user clicks close on the main window. Called once at
// startup right after CreateWindow, when SDL_GL_GetCurrentWindow IS
// the carrier — so we can use it directly here without taking a
// parameter.
static void xerottyHideMainWindow(void) {
	SDL_Window *win = SDL_GL_GetCurrentWindow();
	if (win) {
		SDL_HideWindow(win);
	}
}
*/
import "C"

func sdlSetFullscreen(handle uintptr, enable bool) {
	flag := C.int(0)
	if enable {
		flag = 1
	}
	C.xerottySetFullscreenOn(C.uintptr_t(handle), flag)
}

func sdlQuit() {
	C.xerottyQuit()
}

func sdlHideMainWindow() {
	C.xerottyHideMainWindow()
}
