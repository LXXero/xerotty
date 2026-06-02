// See popup_imgui.h. Implements an xdg_popup-backed window with its
// own ImGui context, calling back into Go each frame so the caller
// can render menu items via cimgui-go without us needing to know
// what's in the menu.
//
// IMPORTANT design note: the popup uses SDL_Renderer +
// impl_sdlrenderer3 rather than OpenGL + impl_opengl3. Two reasons:
//
//   1. Dear ImGui's impl_opengl3 explicitly says "STRONGLY preferred
//      that you use docking-branch multi-viewports (single context +
//      multiple windows) instead of multiple Dear ImGui contexts"
//      (impl_opengl3.cpp:261). We need a separate context for the
//      popup because the main loop is mid-frame when the popup runs,
//      so we ARE in the "multiple contexts" boat. Empirically, the
//      first ImGui_ImplOpenGL3_RenderDrawData call on the main
//      context after the popup's impl_opengl3 lifecycle segfaults
//      regardless of cleanup order.
//
//   2. SDL_Renderer has fully isolated state from raw GL — no
//      cross-talk with main's impl_opengl3 even when both run in the
//      same process.
//
// Tradeoff: the popup font is whatever ImGui's default is, since we
// build a fresh atlas with AddFontDefault. Acceptable for menus.

#include "popup_imgui.h"
#include "sdl3.h"
#include "wlgrab.h"
#include "xgrab.h"
#ifdef __APPLE__
#include "cocoa_focus.h"
#endif

#include "imgui.h"
#include "imgui_impl_sdl3.h"
#include "imgui_impl_sdlrenderer3.h"

#include <SDL3/SDL.h>

#include <cstdio>
#include <cstring>

extern SDL_Window*   g_window;
extern SDL_GLContext g_gl_ctx;
extern Uint32        g_wake_event_type;
extern bool          g_quit;

extern "C" int goPopupImguiDraw(uint64_t cb_id, int* out_w, int* out_h);

extern "C" int platform_run_imgui_popup(unsigned long parent_window_id,
                                        int offset_x, int offset_y,
                                        int w, int h, uint64_t cb_id) {
    SDL_Window* parent = parent_window_id ?
        SDL_GetWindowFromID((SDL_WindowID)parent_window_id) : g_window;
    if (!parent) parent = g_window;
    if (!parent) return -1;

    // No SDL_WINDOW_OPENGL here — we want a SDL_Renderer-managed
    // window, not a GL context window. TRANSPARENT so the surface can
    // be larger than the visible menu: the caller sizes it to fit a
    // cascaded submenu, and the area not covered by a menu window
    // clears to transparent instead of an opaque box.
    SDL_Window* popup = SDL_CreatePopupWindow(
        parent, offset_x, offset_y, w, h,
        SDL_WINDOW_POPUP_MENU | SDL_WINDOW_TRANSPARENT);
    if (!popup) {
        std::fprintf(stderr, "popup_imgui: SDL_CreatePopupWindow: %s\n",
                     SDL_GetError());
        return -1;
    }

    SDL_Renderer* renderer = SDL_CreateRenderer(popup, nullptr);
    if (!renderer) {
        std::fprintf(stderr, "popup_imgui: SDL_CreateRenderer: %s\n",
                     SDL_GetError());
        SDL_DestroyWindow(popup);
        return -1;
    }
    SDL_StartTextInput(popup);
    // On Wayland the popup typically never gets wl_keyboard focus — key
    // and text events keep landing on the PARENT surface. Without text
    // input started on the parent too, parent-focused SDL emits key
    // events but no SDL_EVENT_TEXT_INPUT, which would break type-to-jump.
    // We rewrite parent-delivered key/text events onto the popup in the
    // event loop below; this makes sure the text events actually fire.
    SDL_StartTextInput(parent);

    const char* drv = SDL_GetCurrentVideoDriver();
    SDL_PropertiesID props = SDL_GetWindowProperties(popup);
    if (std::strcmp(drv, "wayland") == 0) {
        void* xdg_popup_ptr = SDL_GetPointerProperty(props,
            SDL_PROP_WINDOW_WAYLAND_XDG_POPUP_POINTER, nullptr);
        wlgrab_popup(xdg_popup_ptr);
    } else if (std::strcmp(drv, "x11") == 0) {
        void* x_display = SDL_GetPointerProperty(props,
            SDL_PROP_WINDOW_X11_DISPLAY_POINTER, nullptr);
        Sint64 x_window = SDL_GetNumberProperty(props,
            SDL_PROP_WINDOW_X11_WINDOW_NUMBER, 0);
        if (x_display && x_window != 0) {
            xgrab_popup(x_display, (unsigned long)x_window);
        }
    }

    // Per-context atlas (NOT shared with main). Tried sharing once
    // — empty popup, because impl_sdlrenderer3 and impl_opengl3
    // independently manage atlas texture upload via TexID, and the
    // shared atlas's TexID was an OpenGL texture from main's renderer
    // which SDL_Renderer can't read from. Each backend needs its own
    // atlas texture, so each context gets its own atlas. The popup
    // uses ImGui's default font (13px); the caller pre-computes
    // popup size matching default-font metrics.
    ImGuiContext* main_ctx  = ImGui::GetCurrentContext();
    ImGuiContext* popup_ctx = ImGui::CreateContext(nullptr);
    ImGui::SetCurrentContext(popup_ctx);
    {
        // Configure the popup context like the main one so keyboard nav
        // (Up/Down/Enter) works. AddFocusEvent(true) tells ImGui the
        // window is focused even though on Wayland the popup may not hold
        // real wl_keyboard focus — without it, nav input is ignored.
        ImGuiIO& io = ImGui::GetIO();
        io.ConfigFlags |= ImGuiConfigFlags_NavEnableKeyboard;
        io.AddFocusEvent(true);
    }
    ImGui::GetIO().Fonts->AddFontDefault();
    ImGui::StyleColorsDark();

    if (!ImGui_ImplSDL3_InitForOther(popup)) {
        std::fprintf(stderr, "popup_imgui: ImGui_ImplSDL3_InitForOther failed\n");
        goto fail_init;
    }
    if (!ImGui_ImplSDLRenderer3_Init(renderer)) {
        std::fprintf(stderr, "popup_imgui: ImGui_ImplSDLRenderer3_Init failed\n");
        ImGui_ImplSDL3_Shutdown();
        goto fail_init;
    }

    {
        SDL_WindowID popup_id  = SDL_GetWindowID(popup);
        SDL_WindowID parent_id = SDL_GetWindowID(parent);
        bool         done      = false;
        bool         dirty     = true;
#ifdef __APPLE__
        Uint64       popup_created_ns = SDL_GetTicksNS();
        // Darwin has two awkward popup-click cases:
        //
        //   1. SDL doesn't deliver mouse events for clicks in another
        //      app, so poll AppKit's global mouse button state and
        //      close on a NEW down-edge outside the popup frame.
        //   2. When right-clicking a background xerotty window, the
        //      activation/opening click can still arrive tagged to the
        //      parent after the popup exists. Don't arm outside-click
        //      dismissal until the buttons that existed at popup
        //      creation have been released.
        unsigned int cocoa_last_mouse_buttons = platform_cocoa_pressed_mouse_buttons();
        bool         cocoa_outside_click_armed = cocoa_last_mouse_buttons == 0;
        bool         app_was_active = platform_cocoa_app_is_active() != 0;
#endif

        while (!done && !g_quit) {
            if (dirty) {
                dirty = false;

                ImGui_ImplSDLRenderer3_NewFrame();
                ImGui_ImplSDL3_NewFrame();
                ImGui::NewFrame();

                int want_w = 0, want_h = 0;
                int draw_result = goPopupImguiDraw(cb_id, &want_w, &want_h);
                if (draw_result != 0) done = true;

                ImGui::Render();

                // Clear to fully transparent (not the old opaque
                // 38,38,46) so only the ImGui menu/submenu windows are
                // visible; the rest of the (deliberately oversized)
                // surface stays see-through. BLENDMODE_NONE makes
                // RenderClear write the 0-alpha straight to the buffer.
                SDL_SetRenderDrawBlendMode(renderer, SDL_BLENDMODE_NONE);
                SDL_SetRenderDrawColor(renderer, 0, 0, 0, 0);
                SDL_RenderClear(renderer);
                ImGui_ImplSDLRenderer3_RenderDrawData(ImGui::GetDrawData(), renderer);
                SDL_RenderPresent(renderer);
                if (done) break;
            }

            SDL_Event e;
            bool got_event = SDL_WaitEventTimeout(&e, 30);
            if (!got_event) {
                dirty = true;
            } else {
                do {
                    // On Wayland the popup rarely gets wl_keyboard focus, so
                    // key/text events arrive tagged with the PARENT's
                    // windowID and ImGui_ImplSDL3_ProcessEvent would drop
                    // them (wrong window). Rewrite parent-delivered keyboard
                    // and text events onto the popup so they reach this
                    // context's nav/typematch. Mouse events are left
                    // window-specific: a parent mouse-down is the
                    // click-outside dismiss path below, NOT popup input.
                    switch (e.type) {
                        case SDL_EVENT_KEY_DOWN:
                        case SDL_EVENT_KEY_UP:
                            if (e.key.windowID == parent_id) e.key.windowID = popup_id;
                            break;
                        case SDL_EVENT_TEXT_INPUT:
                            if (e.text.windowID == parent_id) e.text.windowID = popup_id;
                            break;
                        default:
                            break;
                    }
                    ImGui_ImplSDL3_ProcessEvent(&e);
                    switch (e.type) {
                        case SDL_EVENT_QUIT:
                            g_quit = true;
                            break;
                        case SDL_EVENT_WINDOW_CLOSE_REQUESTED:
                            if (e.window.windowID == popup_id) done = true;
                            break;
                        case SDL_EVENT_WINDOW_HIDDEN:
                            if (e.window.windowID == popup_id) done = true;
                            break;
                        // FOCUS_LOST isn't usable for click-outside-app on
                        // macOS: the popup window itself never takes key
                        // focus (NSPanel canBecomeKey=NO for borderless),
                        // and the parent's focus-lost-then-regained churn
                        // at popup creation can't be cleanly distinguished
                        // from real deactivation by event alone. We poll
                        // [NSApp isActive] each loop iteration instead —
                        // see below the event-drain block.
                        case SDL_EVENT_KEY_DOWN:
                            if (e.key.key == SDLK_ESCAPE) done = true;
                            // Redraw immediately so the nav highlight tracks
                            // arrow keys without waiting for the idle timeout.
                            dirty = true;
                            break;
                        case SDL_EVENT_KEY_UP:
                            dirty = true;
                            break;
                        case SDL_EVENT_MOUSE_BUTTON_DOWN:
                            // Click on a window OTHER than the popup
                            // dismisses. Wayland's xdg_popup grab only
                            // fires popup_done for clicks on different
                            // apps; clicks on the *parent* surface (the
                            // terminal) are considered "still inside the
                            // grab area" per spec and don't dismiss
                            // automatically. Without this, right- or
                            // left-clicking the terminal while a menu is
                            // open does nothing instead of closing the
                            // menu the way every native context menu does.
                            if (e.button.windowID != popup_id) {
#ifdef __APPLE__
                                // Ignore stale opener clicks. On macOS,
                                // clicking/right-clicking a background
                                // window can both activate the parent and
                                // open this popup, then deliver that same
                                // parent-window button-down to the popup
                                // loop. Treat outside clicks as real only
                                // after the creation-time buttons have
                                // come back up, and never close on an
                                // event timestamped before this loop
                                // existed.
                                if (!cocoa_outside_click_armed ||
                                    e.button.timestamp <= popup_created_ns) {
                                    dirty = true;
                                    break;
                                }
#endif
                                done = true;
                                break;
                            }
                            dirty = true;
                            break;
                        case SDL_EVENT_MOUSE_MOTION:
                        case SDL_EVENT_MOUSE_BUTTON_UP:
                        case SDL_EVENT_MOUSE_WHEEL:
                        case SDL_EVENT_TEXT_INPUT:
                            dirty = true;
                            break;
                        default:
                            break;
                    }
                } while (!done && SDL_PollEvent(&e));
            }

#ifdef __APPLE__
            // Cocoa click-away path. This catches clicks in other
            // apps, where SDL never sends us a mouse-down event. Use
            // button DOWN EDGES, not "mouse button is down", so the
            // right-click that opened the menu cannot dismiss it.
            unsigned int cocoa_mouse_buttons = platform_cocoa_pressed_mouse_buttons();
            unsigned int cocoa_new_mouse_buttons =
                cocoa_mouse_buttons & ~cocoa_last_mouse_buttons;
            if (cocoa_mouse_buttons == 0) {
                cocoa_outside_click_armed = true;
            }
            if (!done && cocoa_outside_click_armed &&
                cocoa_new_mouse_buttons != 0 &&
                !platform_cocoa_mouse_in_window((unsigned long)popup_id)) {
                done = true;
            }
            cocoa_last_mouse_buttons = cocoa_mouse_buttons;

            // Keyboard app switching/Dock activation has no mouse
            // down edge. Keep this edge-triggered and armed so a
            // background-window launch does not close before AppKit's
            // activation has settled.
            if (platform_cocoa_app_is_active()) {
                app_was_active = true;
            } else if (app_was_active && cocoa_outside_click_armed) {
                done = true;
            }
#endif
        }
    }

    ImGui_ImplSDLRenderer3_Shutdown();
    ImGui_ImplSDL3_Shutdown();

fail_init:
    ImGui::DestroyContext(popup_ctx);
    ImGui::SetCurrentContext(main_ctx);

    xgrab_release();
    SDL_DestroyRenderer(renderer);
    SDL_DestroyWindow(popup);

    // Restore main GL context so main loop's next end_frame runs
    // against the right context. Main's GL ctx was never made
    // non-current by the popup path (the popup used SDL_Renderer not
    // GL), but on Wayland/some drivers a stale "no current context"
    // state can linger after sub-window destruction.
    SDL_GL_MakeCurrent(g_window, g_gl_ctx);

    ImGui::GetIO().AddMouseButtonEvent(ImGuiMouseButton_Right, false);
    return 0;
}
