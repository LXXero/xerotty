// Direct Cocoa focus transfer. See cocoa_focus.h for rationale.

#include "cocoa_focus.h"

#include <SDL3/SDL.h>
#import <QuartzCore/CAMetalLayer.h>
#import <Cocoa/Cocoa.h>
#import <objc/runtime.h>
#include <stdlib.h>
#include <stdio.h>

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

int platform_cocoa_window_z_rank(unsigned long window_id) {
    SDL_Window* sdlwin = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!sdlwin) return -1;
    SDL_PropertiesID props = SDL_GetWindowProperties(sdlwin);
    NSWindow* target = (__bridge NSWindow*)SDL_GetPointerProperty(
        props, SDL_PROP_WINDOW_COCOA_WINDOW_POINTER, NULL);
    if (!target) return -1;

    int rank = 0;
    for (NSWindow* w in [NSApp orderedWindows]) {
        if (![w isVisible]) continue;
        if (w == target) return rank;
        rank++;
    }
    return -1;
}

int platform_cocoa_window_in_live_resize(unsigned long window_id) {
    SDL_Window* sdlwin = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!sdlwin) return 0;
    SDL_PropertiesID props = SDL_GetWindowProperties(sdlwin);
    NSWindow* nswin = (__bridge NSWindow*)SDL_GetPointerProperty(
        props, SDL_PROP_WINDOW_COCOA_WINDOW_POINTER, NULL);
    if (!nswin) return 0;
    return [nswin inLiveResize] ? 1 : 0;
}

int platform_cocoa_any_window_moved(void) {
    static NSMutableDictionary<NSNumber*, NSValue*>* lastOrigins = nil;
    if (!lastOrigins) lastOrigins = [[NSMutableDictionary alloc] init];

    static int dbg = -1;
    if (dbg < 0) { const char* v = getenv("XEROTTY_DEBUG_MIRROR"); dbg = (v && *v && *v != '0') ? 1 : 0; }
    static int call_count = 0;
    static int last_logged = 0;
    call_count++;

    int moved = 0;
    NSMutableSet<NSNumber*>* seen = [NSMutableSet set];
    for (NSWindow* w in [NSApp windows]) {
        if (![w isVisible]) continue;
        NSNumber* key = @([w windowNumber]);
        [seen addObject:key];
        NSPoint origin = [w frame].origin;
        NSValue* prev = lastOrigins[key];
        if (prev) {
            NSPoint p = [prev pointValue];
            if (p.x != origin.x || p.y != origin.y) {
                moved = 1;
                if (dbg) fprintf(stderr,
                    "[move] win#%ld moved (%.1f,%.1f)->(%.1f,%.1f)\n",
                    (long)[w windowNumber], p.x, p.y, origin.x, origin.y);
            }
        }
        lastOrigins[key] = [NSValue valueWithPoint:origin];
    }
    NSMutableArray<NSNumber*>* stale = [NSMutableArray array];
    for (NSNumber* k in lastOrigins) {
        if (![seen containsObject:k]) [stale addObject:k];
    }
    for (NSNumber* k in stale) [lastOrigins removeObjectForKey:k];
    // Periodically log call count even if no movement, so we can tell
    // if the function is being called during drags at all.
    if (dbg && (call_count - last_logged) >= 60) {
        fprintf(stderr, "[move] heartbeat: %d calls, %d visible\n",
                call_count, (int)[seen count]);
        last_logged = call_count;
    }
    return moved;
}

int platform_cocoa_event_on_chrome(void) {
    NSPoint screenLoc = [NSEvent mouseLocation];
    static int dbg = -1;
    if (dbg < 0) { const char* v = getenv("XEROTTY_DEBUG_MIRROR"); dbg = (v && *v && *v != '0') ? 1 : 0; }
    if (dbg) fprintf(stderr, "[chrome] cursor screen=(%.1f,%.1f) [bottom-left]\n", screenLoc.x, screenLoc.y);
    // Use orderedWindows, not windows. [NSApp windows] is not a
    // reliable front-to-back hit-test order, so when two terminal
    // windows overlap it can report the lower content window before
    // the dragged/front title bar. That misclassifies a title-bar
    // drag as a content click and lets the mouse mirror synthesize a
    // phantom terminal selection.
    for (NSWindow *w in [NSApp orderedWindows]) {
        if (![w isVisible]) continue;
        NSRect winFrame = [w frame];
        if (dbg) fprintf(stderr, "[chrome]   win '%s' frame=(%.0f,%.0f,%.0fx%.0f) visible\n",
                        [[w title] UTF8String] ?: "", winFrame.origin.x, winFrame.origin.y, winFrame.size.width, winFrame.size.height);
        if (!NSPointInRect(screenLoc, winFrame)) continue;
        NSView *cv = [w contentView];
        if (!cv) {
            if (dbg) fprintf(stderr, "[chrome]   -> no cv, CHROME\n");
            return 1;
        }
        NSRect cvWinRect = [cv frame];
        NSRect cvScreenRect = [w convertRectToScreen:cvWinRect];
        int in = NSPointInRect(screenLoc, cvScreenRect);
        if (dbg) fprintf(stderr, "[chrome]   cvScreen=(%.0f,%.0f,%.0fx%.0f) in=%d -> %s\n",
                        cvScreenRect.origin.x, cvScreenRect.origin.y, cvScreenRect.size.width, cvScreenRect.size.height,
                        in, in ? "content" : "CHROME");
        return in ? 0 : 1;
    }
    if (dbg) fprintf(stderr, "[chrome]   no window contains cursor\n");
    return 0;
}

int platform_cocoa_app_is_active(void) {
    return [NSApp isActive] ? 1 : 0;
}

unsigned int platform_cocoa_pressed_mouse_buttons(void) {
    return (unsigned int)[NSEvent pressedMouseButtons];
}

int platform_cocoa_mouse_in_window(unsigned long window_id) {
    SDL_Window* sdlwin = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!sdlwin) return 0;
    SDL_PropertiesID props = SDL_GetWindowProperties(sdlwin);
    NSWindow* nswin = (__bridge NSWindow*)SDL_GetPointerProperty(
        props, SDL_PROP_WINDOW_COCOA_WINDOW_POINTER, NULL);
    if (!nswin) return 0;
    return NSPointInRect([NSEvent mouseLocation], [nswin frame]) ? 1 : 0;
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

// platform_cocoa_disable_window_animations: opt the window out of
// AppKit's implicit window animations (appear-zoom, frame changes).
// Under the SDL_GPU/Metal path on macOS 26, those animations run as
// BLOCKING NSAnimations on dispatch worker threads and never
// complete for CAMetalLayer-backed windows — a `sample` showed one
// permanently parked -[NSAnimation _runBlocking] thread PER WINDOW,
// each sipping sub-millisecond run-loop timers forever (the
// powermetrics "1800 hair-trigger wakeups/sec" storm). The GL-era
// sample had zero such threads. Terminals don't need window
// theatrics; kill them at creation.
void platform_cocoa_disable_window_animations(unsigned long window_id) {
    SDL_Window* win = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!win) return;
    NSWindow* nswin = (__bridge NSWindow*)SDL_GetPointerProperty(
        SDL_GetWindowProperties(win), SDL_PROP_WINDOW_COCOA_WINDOW_POINTER, NULL);
    if (!nswin) return;
    nswin.animationBehavior = NSWindowAnimationBehaviorNone;
}

// platform_cocoa_cap_drawables: shrink the window's CAMetalLayer
// drawable pool from 3 (MAILBOX default) to 2. Each drawable is a
// full window surface — ~24MB at Retina — so a 15-window fleet was
// carrying ~1GB of swapchain pools (the GPU build's resident-memory
// delta vs GL, which double-buffered). At xerotty's self-paced
// frame rates with non-blocking presents, drawables recycle in
// milliseconds; a pool of 2 doesn't reintroduce the nextDrawable
// queue. Must run AFTER SDL_ClaimWindowForGPUDevice attaches the
// metal layer.
void platform_cocoa_cap_drawables(unsigned long window_id) {
    SDL_Window* win = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!win) return;
    NSWindow* nswin = (__bridge NSWindow*)SDL_GetPointerProperty(
        SDL_GetWindowProperties(win), SDL_PROP_WINDOW_COCOA_WINDOW_POINTER, NULL);
    if (!nswin) return;
    // SDL parents its metal view under the content view; find the
    // CAMetalLayer wherever it hangs.
    NSView* content = nswin.contentView;
    CALayer* layer = content.layer;
    if (![layer isKindOfClass:[CAMetalLayer class]]) {
        for (NSView* sub in content.subviews) {
            if ([sub.layer isKindOfClass:[CAMetalLayer class]]) { layer = sub.layer; break; }
        }
    }
    if ([layer isKindOfClass:[CAMetalLayer class]])
        ((CAMetalLayer*)layer).maximumDrawableCount = 2;
}
