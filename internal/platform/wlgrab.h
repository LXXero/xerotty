// wlgrab — wl_seat binding + xdg_popup_grab shim.
//
// SDL3's SDL_WINDOW_POPUP_MENU creates an xdg_popup but never issues
// xdg_popup.grab(seat, serial) on it. Without that, Wayland compositors
// don't track click-outside-dismiss, so the menu never auto-closes
// (verified against labwc via WAYLAND_DEBUG — xfce4-terminal sends
// grab, our spike doesn't). This shim fills the gap: it binds its own
// wl_seat from the registry, tracks the latest input event serial from
// pointer + keyboard events, and exposes a function that calls
// xdg_popup_grab() on a popup handle SDL3 exposes via window props.
//
// libwayland is happy with multiple bindings of the same global — our
// seat and SDL3's seat are independent proxies receiving the same
// compositor events.
//
// No-op on non-Wayland video drivers (X11/Cocoa/Windows).

#ifndef XEROTTY_PLATFORM_WLGRAB_H
#define XEROTTY_PLATFORM_WLGRAB_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// wlgrab_init binds a wl_seat from the given wl_display (cast from
// SDL_PROP_WINDOW_WAYLAND_DISPLAY_POINTER) and starts tracking input
// event serials. Returns 1 on success, 0 if the seat couldn't be bound
// (e.g. compositor doesn't advertise one). Safe to call multiple times
// — subsequent calls are no-ops.
int wlgrab_init(void* wl_display);

// wlgrab_popup calls xdg_popup_grab() on the given xdg_popup pointer
// (cast from SDL_PROP_WINDOW_WAYLAND_XDG_POPUP_POINTER) using the
// tracked seat and the most recent input event serial. Returns 1 if
// the grab call was issued, 0 if the prerequisites aren't satisfied
// (no seat, no serial yet, or popup is NULL).
int wlgrab_popup(void* xdg_popup);

// wlgrab_last_serial returns the most recent input event serial seen.
// Mainly for diagnostics — caller doesn't normally need this.
uint32_t wlgrab_last_serial(void);

// wldrag_start starts a Wayland data_device drag-and-drop session
// originating from origin_surface (cast from
// SDL_PROP_WINDOW_WAYLAND_SURFACE_POINTER on the source window).
// The drag uses a single mime type "application/x-xerotty-tab" so
// other apps' drop targets reject it (intra-app only). Returns 1 if
// the drag was started, 0 if prerequisites aren't met (no
// data_device manager, no serial, no surface).
//
// The compositor handles cursor + delivers wl_data_device.enter /
// leave / drop to surfaces under the cursor. Our listener tracks
// which of our own surfaces currently has the drag-hover; query via
// wldrag_target_surface(). On drop, wldrag_drop_fired() returns 1
// for one frame so Go-side code can claim the tab.
int wldrag_start(void* origin_surface);

// wldrag_target_surface returns the wl_surface pointer the drag is
// currently hovering over (one of our own surfaces) or NULL. Cast
// the result and compare to each Window's
// SDL_PROP_WINDOW_WAYLAND_SURFACE_POINTER.
void* wldrag_target_surface(void);

// wldrag_drop_fired returns 1 once after the compositor delivered
// a drop event, then resets to 0. Lets the Go side poll for "should
// I finalize the drop now".
int wldrag_drop_fired(void);

// wldrag_drop_target_surface returns the wl_surface that was current
// when the most recent drop fired, or NULL if the drop wasn't over a
// surface known to this client.
void* wldrag_drop_target_surface(void);

// wldrag_active reports whether a drag session is in flight (between
// wldrag_start and the cancelled/drop completion).
int wldrag_active(void);

void wlgrab_shutdown(void);

#ifdef __cplusplus
}
#endif

#endif
