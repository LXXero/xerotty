// wlgrab_darwin — empty stubs for the Wayland helpers on macOS.
// The real implementation lives in wlgrab_linux.c and depends on
// libwayland-client. macOS has no Wayland, so sdl3.cpp / popup_imgui.cpp
// never invoke these (they gate on SDL_GetCurrentVideoDriver() == "wayland"),
// but the symbols still need to resolve at link time.

#include "wlgrab.h"

int  wlgrab_init(void* wl_display)        { (void)wl_display; return 0; }
int  wlgrab_popup(void* xdg_popup)        { (void)xdg_popup;  return 0; }
uint32_t wlgrab_last_serial(void)         { return 0; }
int  wldrag_start(void* origin_surface)   { (void)origin_surface; return 0; }
void* wldrag_target_surface(void)         { return 0; }
int  wldrag_drop_fired(void)              { return 0; }
void* wldrag_drop_target_surface(void)    { return 0; }
int  wldrag_active(void)                  { return 0; }
void wlgrab_shutdown(void)                { }
