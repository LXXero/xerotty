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

// platform_cocoa_modifier_flags returns the current modifier-key
// state from NSEvent.modifierFlags. Reports the PHYSICAL state of
// the modifier keys at the keyboard, not the per-window event
// stream — so a Cmd that's held through a window-focus transition
// still reads as Cmd-down here even after macOS sent a phantom
// KEY_UP to the focus-losing window. Bit values follow the
// SDL_Keymod schema (1=shift, 2=ctrl, 4=alt, 8=super/cmd) so
// callers can use the same masks as SDL_GetModState.
unsigned int platform_cocoa_modifier_flags(void);

#ifdef __cplusplus
}
#endif

#endif
