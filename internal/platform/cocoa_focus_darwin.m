// Direct Cocoa focus transfer. See cocoa_focus.h for rationale.

#include "cocoa_focus.h"

#include <SDL3/SDL.h>
#import <Cocoa/Cocoa.h>
#import <objc/runtime.h>

// AppKit's responder chain catches Cmd+W via `performKeyEquivalent:`
// and dispatches to the focused NSWindow's `performClose:` BEFORE
// SDL even sees the key event. With a single visible NSWindow that
// also triggers `applicationShouldTerminateAfterLastWindowClosed:`
// (defaults to YES) → NSApp.terminate → SDL_EVENT_QUIT → app exits.
// In multi-window mode the auto-terminate doesn't fire (other
// windows remain visible), so the close path looks like it works.
//
// Suppress performClose: when triggered by a Cmd+W keystroke. The
// red close button calls performClose: too but without a current
// key event, so the original (closing) behavior still runs for it.
//
// Implemented as a category + method swizzle at +load so we don't
// have to subclass NSWindow ourselves — SDL3 creates the windows
// and we just need to intercept the action method.
@interface NSWindow (XerottyCmdWSuppress)
- (void)xerottyOrigPerformClose:(id)sender;
@end

@implementation NSWindow (XerottyCmdWSuppress)

+ (void)load {
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        Method orig = class_getInstanceMethod(self, @selector(performClose:));
        Method repl = class_getInstanceMethod(self, @selector(xerottyOrigPerformClose:));
        method_exchangeImplementations(orig, repl);
    });
}

// After method_exchangeImplementations, `xerottyOrigPerformClose:`
// holds the IMP that was originally `performClose:`, and
// `performClose:` now dispatches to this body. Confusing naming, but
// inside this body we call back to `xerottyOrigPerformClose:` to
// invoke the original implementation.
- (void)xerottyOrigPerformClose:(id)sender {
    NSEvent *e = [NSApp currentEvent];
    if (e && [e type] == NSEventTypeKeyDown) {
        NSEventModifierFlags mods = [e modifierFlags];
        NSString *chars = [e charactersIgnoringModifiers];
        if ((mods & NSEventModifierFlagCommand) && [chars isEqualToString:@"w"]) {
            // Cmd+W: let xerotty's in-app keybind handle it. Don't
            // close the NSWindow (which would auto-terminate the
            // app when this is the only visible window).
            return;
        }
    }
    // Other trigger (red-X click, programmatic close, menu) — run
    // the real performClose:.
    [self xerottyOrigPerformClose:sender];
}

@end

void platform_cocoa_focus_window(unsigned long window_id) {
    SDL_Window* w = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!w) return;
    SDL_PropertiesID props = SDL_GetWindowProperties(w);
    NSWindow* nswin = (__bridge NSWindow*)SDL_GetPointerProperty(
        props, SDL_PROP_WINDOW_COCOA_WINDOW_POINTER, NULL);
    if (!nswin) return;

    // Pulled from AppKit lifecycle docs:
    //   1. Activate the app — ensures NSApp.isActive=YES so that
    //      makeKey: actually transfers OS keyboard focus to us
    //      rather than queueing it for "when app becomes active".
    //   2. makeKeyAndOrderFront: makes the NSWindow the key window
    //      AND raises it. This is the call SDL_RaiseWindow is
    //      *supposed* to do — but in practice it lands at a moment
    //      where AppKit defers the makeKey until the next event
    //      tick, producing the "I have to move my mouse first" bug.
    //      Calling it directly here, after the SDL_Window exists,
    //      synchronously transitions firstResponder so the next
    //      SDL_EVENT_KEY_DOWN routes to this window.
    [NSApp activateIgnoringOtherApps:YES];
    [nswin makeKeyAndOrderFront:NSApp];
}

unsigned int platform_cocoa_modifier_flags(void) {
    // NSEvent.modifierFlags is a CLASS method that returns the
    // current PHYSICAL state of modifier keys system-wide. Unlike
    // SDL_GetModState (which mirrors the per-window NSEvent stream),
    // this reads the IOHID-level hardware state directly and is
    // correct across window-focus transitions where AppKit drops a
    // phantom KEY_UP to the focus-losing window without ever
    // sending the corresponding KEY_DOWN to the focus-gaining one.
    NSEventModifierFlags raw = [NSEvent modifierFlags];
    unsigned int out = 0;
    if (raw & NSEventModifierFlagShift)   out |= 0x01; // SDL_KMOD_SHIFT
    if (raw & NSEventModifierFlagControl) out |= 0x02; // SDL_KMOD_CTRL
    if (raw & NSEventModifierFlagOption)  out |= 0x04; // SDL_KMOD_ALT
    if (raw & NSEventModifierFlagCommand) out |= 0x08; // SDL_KMOD_GUI
    return out;
}
