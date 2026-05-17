// Cocoa side of macOS-only cell-snap window resize. NSWindow's
// setContentResizeIncrements: is the equivalent of GTK's
// GDK_HINT_RESIZE_INC: AppKit constrains drag-resize so the content area
// only changes in multiples of the increment. Combined with our initial
// window size being cell-aligned (cols*cellW + pad + margin), every
// resulting size also lands on a cell boundary.
//
// This both gives us the xfce4-terminal-style snap-during-drag feel and
// drastically cuts down on Cocoa's live-resize image stretch (the OS
// only commits a new size at cell boundaries instead of every pixel of
// cursor movement).
#include <SDL2/SDL.h>
#include <SDL2/SDL_syswm.h>
#include <stdint.h>
#import <Cocoa/Cocoa.h>

// Caller passes the SDL window ID (uint32) that ImGui's SDL2 backend
// stores in ImGuiViewport::PlatformHandle — multi-viewport means each
// Window has its own SDL_Window and SDL_GL_GetCurrentWindow() would
// return the hidden cimgui-go carrier instead, leaving the visible
// window's drag-resize unsnapped. SDL_GetWindowFromID converts the ID
// back to the SDL_Window* we need for SDL_GetWindowWMInfo.
void xerottySetContentResizeIncrementsOn(uintptr_t window_id, double w, double h) {
	SDL_Window *win = SDL_GetWindowFromID((Uint32)window_id);
	if (!win) return;
	SDL_SysWMinfo info;
	SDL_VERSION(&info.version);
	if (!SDL_GetWindowWMInfo(win, &info)) return;
	NSWindow *nswin = info.info.cocoa.window;
	if (!nswin) return;
	if (w < 1) w = 1;
	if (h < 1) h = 1;
	[nswin setContentResizeIncrements:NSMakeSize(w, h)];
}

