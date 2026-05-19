// xgrab — X11 parallel of wlgrab: XGrabPointer on a popup window so
// clicks outside the popup still arrive as ButtonPress events to it
// (instead of being routed to whatever app the cursor is over).
//
// Same SDL3 gap as on Wayland: SDL_WINDOW_POPUP_MENU is documented to
// "behave like a popup menu" but SDL3 doesn't actually call
// XGrabPointer for it, so click-outside-dismiss is silently broken on
// X11 too. We use the X11 Display + Window SDL3 exposes via window
// properties and call XGrabPointer ourselves.
//
// With owner_events=False the grab routes all pointer events to our
// popup window — SDL3 surfaces them as SDL_EVENT_MOUSE_BUTTON_DOWN
// events with coords (which may be negative or outside the popup
// rect for clicks elsewhere on the screen). Our popup loop checks
// those bounds and dismisses on out-of-rect clicks.
//
// No-op on non-X11 video drivers.

#ifndef XEROTTY_PLATFORM_XGRAB_H
#define XEROTTY_PLATFORM_XGRAB_H

#ifdef __cplusplus
extern "C" {
#endif

// xgrab_popup grabs the X11 pointer on the given window. display is
// from SDL_PROP_WINDOW_X11_DISPLAY_POINTER; window is from
// SDL_PROP_WINDOW_X11_WINDOW_NUMBER (cast to unsigned long).
// Returns 1 on success, 0 on failure.
int xgrab_popup(void* display, unsigned long window);

void xgrab_release(void);

#ifdef __cplusplus
}
#endif

#endif
