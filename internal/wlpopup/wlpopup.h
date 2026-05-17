// Native Wayland xdg_popup-backed context menu. Bypasses SDL for the
// menu's surface so the compositor handles "click anywhere outside
// dismisses" via the standard xdg_popup protocol — same machinery
// GTK menus use.
//
// Linux-only (needs libwayland-client + xdg-shell). On non-Linux or
// non-Wayland sessions Available() returns 0 and the caller falls
// back to its in-process menu.

#ifndef XEROTTY_WLPOPUP_H
#define XEROTTY_WLPOPUP_H

#include <stdint.h>

// Initialize wlpopup with handles extracted from SDL_SysWMinfo.
// display:        wl_display* the rest of the app is already using
// parent_surface: wl_surface* of the parent xerotty window
// parent_xdg:    xdg_surface* same window — popup must have an
//                 xdg_surface parent; SDL exposes this for its own
//                 windows in SDL_SysWMinfo.wl.xdg_surface
//
// Returns 0 on success, nonzero on failure (e.g. compositor doesn't
// bind required globals).
int wlpopup_init(void *display, void *parent_surface, void *parent_xdg);

// Reports whether wlpopup is initialized and ready to show menus.
int wlpopup_available(void);

// Pack menu data the caller will hand to wlpopup_show.
// Item types:
//   0 = regular item ("label" displayed, "action" returned on click)
//   1 = separator (label/action ignored)
//   2 = disabled item (drawn dimmed, not clickable)
typedef struct {
    int type;
    const char *label;
    const char *action;
} wlpopup_item;

// Show the popup at (x, y) (parent-surface-relative coords) with the
// given items. Non-blocking — returns a handle; caller polls with
// wlpopup_poll to discover when the popup is dismissed and what (if
// anything) the user chose.
//
// Returns a positive popup ID, or 0 on failure.
int wlpopup_show(int x, int y, const wlpopup_item *items, int n);

// Poll for popup result. Returns:
//   0  — still open, no result
//   1  — popup dismissed without selection (click outside / Escape)
//   2  — item selected; *action_out is set to the action string the
//        caller passed in (NUL-terminated, owned by wlpopup)
int wlpopup_poll(int popup_id, const char **action_out);

// Manually dismiss the popup (e.g. user opened a peer menu).
void wlpopup_dismiss(int popup_id);

// Drive the wayland event loop once. Caller must invoke from its
// main loop while any popup is open so events get dispatched.
void wlpopup_pump(void);

#endif
