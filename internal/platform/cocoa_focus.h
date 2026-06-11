// Direct Cocoa-level focus helper. SDL_RaiseWindow on macOS doesn't
// reliably transfer keyboard focus to a freshly-created or
// remaining-after-close NSWindow (verified: user must move the mouse
// before keyboard events route to the raised window). Bypass SDL and
// call NSWindow.makeKeyAndOrderFront: + NSApp.activateIgnoringOtherApps:
// directly to force the OS-side focus transition synchronously.
//
// Only the darwin build defines the real body (cocoa_focus_darwin.m).
// Other platforms get a stub via cocoa_focus_other.go so app.go can
// call it unconditionally.

#ifndef XEROTTY_COCOA_FOCUS_H
#define XEROTTY_COCOA_FOCUS_H

#ifdef __cplusplus
extern "C" {
#endif

// platform_cocoa_focus_window pulls keyboard focus to the SDL_Window
// with the given ID using direct AppKit calls. Idempotent; safe to
// call repeatedly. No-op if the window doesn't exist.
void platform_cocoa_focus_window(unsigned long window_id);

// platform_cocoa_window_z_rank returns the front-to-back order of the
// SDL_Window's backing NSWindow among visible app windows: 0 means
// frontmost. Returns -1 if the SDL/NSWindow cannot be found.
int platform_cocoa_window_z_rank(unsigned long window_id);

// platform_cocoa_window_in_live_resize returns 1 while the SDL_Window's
// backing NSWindow is inside AppKit's live-resize tracking loop.
int platform_cocoa_window_in_live_resize(unsigned long window_id);

// platform_cocoa_modifier_flags returns the current modifier-key
// state from NSEvent.modifierFlags. Reports the PHYSICAL state of
// the modifier keys at the keyboard, not the per-window event
// stream — so a Cmd that's held through a window-focus transition
// still reads as Cmd-down here even after macOS sent a phantom
// KEY_UP to the focus-losing window. Bit values follow the
// SDL_Keymod schema (1=shift, 2=ctrl, 4=alt, 8=super/cmd) so
// callers can use the same masks as SDL_GetModState.
unsigned int platform_cocoa_modifier_flags(void);

// platform_cocoa_any_window_moved returns 1 if any visible NSWindow
// in NSApp.windows has moved since the last call (i.e. the user is
// dragging a window). Internally caches per-window frame origins
// keyed by windowNumber.
//
// Why this is needed even though we already track viewport.Pos per
// Window in Go: ImGui's view of viewport.Pos updates only AFTER
// SDL_EVENT_WINDOW_MOVED has been delivered AND
// ImGui::UpdatePlatformWindows has run (next frame's
// platform_end_frame). During a continuous drag that lag means
// the Go-side previous-pos comparison sees the same value frame
// after frame and concludes "no drag". Querying NSWindow.frame
// directly here reads the live AppKit state and detects the drag
// immediately.
int platform_cocoa_any_window_moved(void);

// platform_cocoa_event_on_chrome returns 1 if the current cursor
// location falls outside the frontmost visible NSWindow contentView
// under the cursor (i.e. it's on title-bar / resize-edge chrome).
// Returns 0 if the cursor is in contentView or no app window contains
// it.
//
// Used by the macOS mouse-mirror to skip synthetic DOWN injection
// during a title-bar drag — without this guard, dragging a window
// over a peer manufactures phantom selections in whichever peer's
// content rect happens to be under the dragged title bar.
int platform_cocoa_event_on_chrome(void);

// platform_cocoa_app_is_active returns 1 while NSApp.isActive is YES
// (xerotty is the frontmost app), 0 otherwise. Used by the popup
// event loop to dismiss menus when the user clicks into another
// application — SDL doesn't deliver mouse events for clicks outside
// our windows, but [NSApp isActive] flips to NO on deactivation.
int platform_cocoa_app_is_active(void);

// See cocoa_focus_darwin.m — kills AppKit's implicit window
// animations, which leak permanently-blocked NSAnimation threads on
// macOS 26 with Metal-layer windows.
void platform_cocoa_disable_window_animations(unsigned long window_id);

// See cocoa_focus_darwin.m — caps the CAMetalLayer drawable pool at
// 2 (from MAILBOX's 3); each drawable is a full Retina window
// surface, so this is ~24MB per window.
void platform_cocoa_cap_drawables(unsigned long window_id);

// platform_cocoa_pressed_mouse_buttons returns AppKit's global
// pressedMouseButtons bitmask. Unlike SDL mouse events, this can be
// polled while the cursor is outside xerotty.
unsigned int platform_cocoa_pressed_mouse_buttons(void);

// platform_cocoa_mouse_in_window returns 1 if the current global mouse
// location is inside the backing NSWindow for the given SDL_WindowID.
int platform_cocoa_mouse_in_window(unsigned long window_id);

#ifdef __cplusplus
}
#endif

#endif
