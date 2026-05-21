// xgrab — see xgrab.h. Plain C, cgo CFLAGS.

#include "xgrab.h"

#include <stdio.h>
#include <X11/Xlib.h>

static Display* g_display = NULL;
static int      g_grabbed = 0;

int xgrab_popup(void* display, unsigned long window) {
    g_display = (Display*)display;
    if (!g_display || !window) return 0;

    // owner_events=False forces every pointer event to route to our
    // popup window regardless of where the cursor is — that's how we
    // see outside clicks at all. event_mask covers the events SDL3
    // will translate into MOUSE_* SDL_Events for the popup window.
    int result = XGrabPointer(
        g_display, (Window)window, False,
        ButtonPressMask | ButtonReleaseMask | PointerMotionMask |
        EnterWindowMask | LeaveWindowMask,
        GrabModeAsync, GrabModeAsync,
        None,         // confine_to: no confinement
        None,         // cursor: keep current cursor
        CurrentTime);
    if (result != GrabSuccess) {
        const char* err = "unknown";
        switch (result) {
            case AlreadyGrabbed:   err = "AlreadyGrabbed";   break;
            case GrabInvalidTime:  err = "GrabInvalidTime";  break;
            case GrabNotViewable:  err = "GrabNotViewable";  break;
            case GrabFrozen:       err = "GrabFrozen";       break;
        }
        fprintf(stderr, "xgrab_popup: XGrabPointer failed: %s (%d)\n",
                err, result);
        return 0;
    }
    XFlush(g_display);
    g_grabbed = 1;
    return 1;
}

void xgrab_release(void) {
    if (g_grabbed && g_display) {
        XUngrabPointer(g_display, CurrentTime);
        XFlush(g_display);
        g_grabbed = 0;
    }
}
