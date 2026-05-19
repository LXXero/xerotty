// xgrab_darwin — empty stubs for the X11 grab helper on macOS.
// The real implementation lives in xgrab_linux.c and depends on libX11.
// sdl3.cpp / popup_imgui.cpp only call into xgrab when
// SDL_GetCurrentVideoDriver() == "x11", which never happens on a Cocoa
// build, so these stubs only need to satisfy the linker.

#include "xgrab.h"

int  xgrab_popup(void* display, unsigned long window) {
    (void)display; (void)window;
    return 0;
}
void xgrab_release(void) { }
