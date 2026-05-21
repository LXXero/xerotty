// macOS cell-snap. AppKit's setContentResizeIncrements: is the OS-
// level "snap-to-grid" knob for drag-resize; same role as
// XSetWMNormalHints PResizeInc on X11. Combined with an initially
// cell-aligned window size, every resized state lands on a cell
// boundary, and the live-resize image-stretch artifact also drops
// way down because Cocoa only commits new sizes at cell boundaries.

#include "cellsnap.h"

#include <SDL3/SDL.h>
#import <Cocoa/Cocoa.h>

int platform_set_resize_increments(unsigned long window_id, int inc_w, int inc_h) {
    if (inc_w < 1) inc_w = 1;
    if (inc_h < 1) inc_h = 1;

    SDL_Window* w = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!w) return 0;
    SDL_PropertiesID props = SDL_GetWindowProperties(w);
    NSWindow* nswin = (__bridge NSWindow*)SDL_GetPointerProperty(
        props, SDL_PROP_WINDOW_COCOA_WINDOW_POINTER, NULL);
    if (!nswin) return 0;
    [nswin setContentResizeIncrements:NSMakeSize((CGFloat)inc_w, (CGFloat)inc_h)];
    return 1;
}
