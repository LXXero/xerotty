// SDL3 xdg_popup with a real ImGui rendering context inside it.
//
// Wayland doesn't let apps position toplevel windows, so the existing
// menu (which relied on ImGui multi-viewport to pop a NoAutoMerge
// window into its own toplevel) ends up wherever the compositor
// decides — usually centered, not at the click position. The only
// fix is to use xdg_popup, which Wayland DOES let us position
// relative to a parent surface. SDL3 exposes this as
// SDL_WINDOW_POPUP_MENU.
//
// This file wraps that primitive with a second ImGui context (sharing
// the main one's font atlas) so existing menu-rendering code that
// uses cimgui-go's `imgui.Begin/MenuItem/...` API can run unchanged
// inside the popup window. The C side handles SDL + GL + ImGui ctx
// lifecycle; Go calls back into the draw callback each frame inside
// the popup loop.

#ifndef XEROTTY_PLATFORM_POPUP_IMGUI_H
#define XEROTTY_PLATFORM_POPUP_IMGUI_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// platform_run_imgui_popup opens an SDL_WINDOW_POPUP_MENU child of
// the main window at (offset_x, offset_y) relative to the parent's
// top-left, with initial size (w, h). Inside, it creates a separate
// ImGui context (sharing the main context's font atlas), runs an
// event loop, and calls back into Go via goPopupImguiDraw each
// frame so the caller can render menu items using cimgui-go API.
//
// The Go callback returns 0 to keep the popup open or 1 to close
// it (e.g. after the user clicked an item; the selected action is
// tracked Go-side and returned separately via the cbID lookup).
// The popup also closes on Escape, on the compositor's popup_done
// (Wayland xdg_popup grab dismiss via wlgrab), or on the OS window
// close button.
//
// Returns 0 on a normal close (caller should consult Go-side state
// for the selected action), -1 if popup creation failed.
int platform_run_imgui_popup(unsigned long parent_window_id,
                             int offset_x, int offset_y, int w, int h,
                             uint64_t cb_id);

#ifdef __cplusplus
}
#endif

#endif
