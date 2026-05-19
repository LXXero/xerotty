// Linux cell-snap. On X11 we set XSizeHints.{width,height}_inc via
// XSetWMNormalHints — the WM-standard way for an app to ask a
// constrained drag-resize (xterm, gnome-terminal, xfce4-terminal all
// use this through GTK's gdk_window_set_geometry_hints). On Wayland
// it's a no-op: the protocol doesn't expose resize increments by
// design (the compositor owns sizing). Software-side cell-snap in
// the renderer covers Wayland.

#include "cellsnap.h"

#include <string.h>
#include <SDL3/SDL.h>
#include <X11/Xlib.h>
#include <X11/Xutil.h>

void platform_set_resize_increments(unsigned long window_id, int inc_w, int inc_h) {
    if (inc_w < 1) inc_w = 1;
    if (inc_h < 1) inc_h = 1;

    SDL_Window* w = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!w) return;

    // Only X11 supports it; Wayland's xdg_shell has no equivalent.
    const char* drv = SDL_GetCurrentVideoDriver();
    if (!drv || strcmp(drv, "x11") != 0) return;

    SDL_PropertiesID props = SDL_GetWindowProperties(w);
    Display* dpy = (Display*)SDL_GetPointerProperty(
        props, SDL_PROP_WINDOW_X11_DISPLAY_POINTER, NULL);
    Window xwin = (Window)SDL_GetNumberProperty(
        props, SDL_PROP_WINDOW_X11_WINDOW_NUMBER, 0);
    if (!dpy || !xwin) return;

    // Preserve whatever hints SDL/the WM has already set, just add
    // (or replace) resize_inc.
    XSizeHints* hints = XAllocSizeHints();
    if (!hints) return;
    long supplied = 0;
    XGetWMNormalHints(dpy, xwin, hints, &supplied);
    hints->flags |= PResizeInc;
    hints->width_inc = inc_w;
    hints->height_inc = inc_h;
    XSetWMNormalHints(dpy, xwin, hints);
    XFree(hints);
    XFlush(dpy);
}
