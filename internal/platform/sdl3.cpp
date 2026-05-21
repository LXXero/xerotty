// SDL3 + Dear ImGui platform shim — Phase 0 implementation.
//
// Mirrors what cimgui-go/backend/sdlbackend does for SDL2, but talks to
// SDL3 directly so we get native xdg_shell / xdg_popup, proper Wayland
// multi-window, and SDL_CreatePopupWindow (used in Phase 2).
//
// Why we don't reuse cimgui-go's SDL backend: cimgui-go v1.4.0's
// backend/sdlbackend is hard-pinned to SDL2 (via sdl2-compat → SDL3
// shim). That shim drops native Wayland popup support and silently
// no-ops SDL_CaptureMouse on X11. The ImGui side of cimgui-go is fine
// — we keep using it for the Go API (cimgui.a already has SDL2 backend
// symbols compiled in but we just don't call them).
//
// Linking: cimgui.a (provided by github.com/AllenDang/cimgui-go/imgui
// via cgo LDFLAGS, transitively imported by the spike binary) supplies
// the ImGui::* implementation. Our impl_sdl3.cpp / impl_opengl3.cpp
// are compiled here and link against cimgui.a's ImGui core. ABI match
// is guaranteed because we vendor the headers and impl sources from
// the same cimgui-go module copy that built cimgui.a (ImGui 1.92.4
// WIP), and we pass -DIMGUI_USE_WCHAR32 to match cimgui.a's build.

#include "sdl3.h"
#include "wlgrab.h"
#include "xgrab.h"

#include "imgui.h"
#include "imgui_impl_sdl3.h"
#include "imgui_impl_opengl3.h"

#include <SDL3/SDL.h>
#include <SDL3/SDL_opengl.h>

#include <cstdio>
#include <cstring>

#ifdef __APPLE__
#include "cocoa_focus.h"
#endif

// Globals visible to popup_imgui.cpp — kept at file scope (no anonymous
// namespace) so the popup translation unit can `extern` them. Other
// platform files shouldn't touch these directly; popup_imgui needs the
// main window handle to parent its xdg_popup, the GL context to
// restore after switching, and the wake-event id + quit flag for the
// shared event-loop semantics.
SDL_Window*   g_window = nullptr;
SDL_GLContext g_gl_ctx = nullptr;
bool          g_quit   = false;
Uint32        g_wake_event_type = 0;

namespace {

char          g_err[512] = {0};
Uint64        g_target_frame_ns = 1000000000ull / 60;  // updated in init
float         g_bg_r = 0.10f, g_bg_g = 0.10f, g_bg_b = 0.12f, g_bg_a = 1.0f;

void set_err(const char* prefix) {
    const char* sdl = SDL_GetError();
    std::snprintf(g_err, sizeof(g_err), "%s: %s",
                  prefix, (sdl && *sdl) ? sdl : "(no SDL error)");
}

} // namespace

extern "C" int platform_init(const char* title, int width, int height) {
    // Prefer native Wayland over XWayland when both are available — the
    // X11 path on a Wayland session can't honor cross-app pointer
    // grabs (popup-menu click-outside dismiss is silently broken),
    // and our wlgrab shim only works against actual Wayland. Allow
    // SDL_VIDEODRIVER to override (e.g. user explicitly testing X11).
    // Linux only — macOS's only video driver is "cocoa", forcing
    // wayland/x11 there makes SDL_Init fail with no useful error.
#if !defined(__APPLE__)
    if (!SDL_GetHint(SDL_HINT_VIDEO_DRIVER)) {
        SDL_SetHint(SDL_HINT_VIDEO_DRIVER, "wayland,x11");
    }
#endif
    if (!SDL_Init(SDL_INIT_VIDEO)) {
        set_err("SDL_Init");
        return 0;
    }

    // Match Dear ImGui's own SDL example: GL 3.0 core + GLSL 130 on
    // Linux/Windows, 3.2 core + GLSL 150 + forward-compatible on macOS
    // (the only Mac-allowed combo). Asking for 3.2 core on X11 made
    // Mesa hand back a context whose GLSL compiler rejected
    // "#version 150" → ImGui_ImplOpenGL3_CreateDeviceObjects asserted
    // at first frame. 3.0/130 works on Mesa, NVIDIA, and Wayland.
#if defined(__APPLE__)
    const char* glsl_version = "#version 150";
    SDL_GL_SetAttribute(SDL_GL_CONTEXT_FLAGS, SDL_GL_CONTEXT_FORWARD_COMPATIBLE_FLAG);
    SDL_GL_SetAttribute(SDL_GL_CONTEXT_PROFILE_MASK, SDL_GL_CONTEXT_PROFILE_CORE);
    SDL_GL_SetAttribute(SDL_GL_CONTEXT_MAJOR_VERSION, 3);
    SDL_GL_SetAttribute(SDL_GL_CONTEXT_MINOR_VERSION, 2);
#else
    const char* glsl_version = "#version 130";
    SDL_GL_SetAttribute(SDL_GL_CONTEXT_FLAGS, 0);
    SDL_GL_SetAttribute(SDL_GL_CONTEXT_PROFILE_MASK, SDL_GL_CONTEXT_PROFILE_CORE);
    SDL_GL_SetAttribute(SDL_GL_CONTEXT_MAJOR_VERSION, 3);
    SDL_GL_SetAttribute(SDL_GL_CONTEXT_MINOR_VERSION, 0);
#endif
    SDL_GL_SetAttribute(SDL_GL_DOUBLEBUFFER, 1);
    SDL_GL_SetAttribute(SDL_GL_DEPTH_SIZE, 24);
    SDL_GL_SetAttribute(SDL_GL_STENCIL_SIZE, 8);

    SDL_WindowFlags flags = SDL_WINDOW_OPENGL
                          | SDL_WINDOW_RESIZABLE
                          | SDL_WINDOW_HIGH_PIXEL_DENSITY;
    g_window = SDL_CreateWindow(title, width, height, flags);
    if (!g_window) {
        set_err("SDL_CreateWindow");
        SDL_Quit();
        return 0;
    }

    // SDL3 turns text input OFF by default (SDL2 had it on). Without
    // this, SDL_EVENT_TEXT_INPUT never fires → impl_sdl3 never calls
    // io.AddInputCharactersUTF8 → io.InputQueueCharacters is always
    // empty → terminals can't be typed into. Has to be done per-window
    // and after window creation but before any events get pumped.
    SDL_StartTextInput(g_window);

    g_gl_ctx = SDL_GL_CreateContext(g_window);
    if (!g_gl_ctx) {
        set_err("SDL_GL_CreateContext");
        SDL_DestroyWindow(g_window);
        g_window = nullptr;
        SDL_Quit();
        return 0;
    }
    SDL_GL_MakeCurrent(g_window, g_gl_ctx);

    // Enable vsync. Try adaptive (-1) first, fall back to standard (1).
    // Driver-side enforcement is best-effort: on AMD/Mesa we observed
    // SwapInterval accepted but silently no-op'd; the manual frame
    // cap in begin_frame covers that case.
    if (!SDL_GL_SetSwapInterval(-1)) {
        SDL_GL_SetSwapInterval(1);
    }

    IMGUI_CHECKVERSION();
    ImGui::CreateContext();
    ImGuiIO& io = ImGui::GetIO();
    io.ConfigFlags |= ImGuiConfigFlags_NavEnableKeyboard;
    ImGui::StyleColorsDark();

    if (!ImGui_ImplSDL3_InitForOpenGL(g_window, g_gl_ctx)) {
        std::snprintf(g_err, sizeof(g_err), "ImGui_ImplSDL3_InitForOpenGL failed");
        return 0;
    }
    if (!ImGui_ImplOpenGL3_Init(glsl_version)) {
        std::snprintf(g_err, sizeof(g_err), "ImGui_ImplOpenGL3_Init failed");
        return 0;
    }

    // On Wayland: bind our own wl_seat off the SDL-owned wl_display so
    // we can issue xdg_popup.grab() ourselves later. SDL3's
    // SDL_WINDOW_POPUP_MENU skips that call (verified vs xfce4 via
    // WAYLAND_DEBUG) and without it the compositor doesn't track
    // click-outside dismiss. See wlgrab.h.
    if (std::strcmp(SDL_GetCurrentVideoDriver(), "wayland") == 0) {
        SDL_PropertiesID props = SDL_GetWindowProperties(g_window);
        void* wl_disp = SDL_GetPointerProperty(props,
            SDL_PROP_WINDOW_WAYLAND_DISPLAY_POINTER, nullptr);
        if (wl_disp) wlgrab_init(wl_disp);
    }

    // Reserve a custom event type for PTY-wake signals. SDL_PushEvent
    // is thread-safe, so Go's PTY reader goroutine can call
    // platform_post_wake to break the main loop out of WaitEventTimeout
    // immediately when new output arrives.
    g_wake_event_type = SDL_RegisterEvents(1);
    if (g_wake_event_type == (Uint32)-1) {
        std::snprintf(g_err, sizeof(g_err), "SDL_RegisterEvents failed");
        return 0;
    }

    // Cap frame loop to display refresh × 4 (so up to ~240 fps on a
    // 60 Hz panel). Locking to exactly display rate makes mouse drag
    // only update 60 times/sec even though mice poll at 125-1000 Hz,
    // which is visibly stuttery; 4× keeps CPU bounded but lets drag
    // track at mouse-poll rate. SDL_WaitEventTimeout handles idle.
    SDL_DisplayID did = SDL_GetDisplayForWindow(g_window);
    const SDL_DisplayMode* mode = did ? SDL_GetCurrentDisplayMode(did) : nullptr;
    float refresh = (mode && mode->refresh_rate > 0) ? mode->refresh_rate : 60.0f;
    g_target_frame_ns = (Uint64)(1e9 / (refresh * 4.0f));

    return 1;
}

extern "C" const char* platform_last_error(void) {
    return g_err[0] ? g_err : "(no error)";
}

extern "C" const char* platform_video_driver(void) {
    const char* d = SDL_GetCurrentVideoDriver();
    return d ? d : "(unknown)";
}

// Drain all currently queued SDL events into ImGui + our quit flag.
// Doesn't block. Returns nothing — quit state goes through g_quit.
static void drain_events() {
    SDL_Event e;
    while (SDL_PollEvent(&e)) {
        ImGui_ImplSDL3_ProcessEvent(&e);
        switch (e.type) {
            case SDL_EVENT_QUIT:
                g_quit = true;
                break;
            case SDL_EVENT_WINDOW_CLOSE_REQUESTED:
                if (SDL_GetWindowFromID(e.window.windowID) == g_window)
                    g_quit = true;
                break;
            default:
                break;
        }
    }
}

extern "C" int platform_begin_frame(void) {
    // Cadence: render exactly once per display frame. While waiting for
    // the next render deadline, keep draining events as they arrive so
    // ImGui always sees the latest mouse position when we do render.
    // Earlier version slept-or-rendered per event, which on X11 (no
    // compositor throttle) let drag-spawned motion events burst FPS
    // way above the cap and made movement visibly jittery.
    static Uint64 next_render_ns = 0;
    if (next_render_ns == 0) next_render_ns = SDL_GetTicksNS();

    drain_events();
    if (g_quit) return 0;

    while (!g_quit) {
        Uint64 now = SDL_GetTicksNS();
        if (now >= next_render_ns) break;
        // Round up so we don't busy-spin on sub-ms residuals — better
        // to sleep ~1ms long than to wake with timeout=0 and not wait.
        Sint32 ms_left = (Sint32)((next_render_ns - now + 999999ull) / 1000000ull);
        if (ms_left <= 0) break;
        SDL_WaitEventTimeout(nullptr, ms_left);
        drain_events();
    }
    if (g_quit) return 0;

    // Advance deadline. If a frame ran long, snap forward instead of
    // accumulating "owed" frames (would cause a catch-up burst once
    // load drops).
    next_render_ns += g_target_frame_ns;
    Uint64 now = SDL_GetTicksNS();
    if (next_render_ns < now) next_render_ns = now + g_target_frame_ns;

    ImGui_ImplOpenGL3_NewFrame();
    ImGui_ImplSDL3_NewFrame();
    // Caller (Go) calls ImGui::NewFrame via cimgui-go next.
    return 1;
}

extern "C" void platform_end_frame(void) {
    // Caller (Go) has already called ImGui::Render via cimgui-go.
    int w = 0, h = 0;
    SDL_GetWindowSizeInPixels(g_window, &w, &h);
    glViewport(0, 0, w, h);
    glClearColor(g_bg_r, g_bg_g, g_bg_b, g_bg_a);
    glClear(GL_COLOR_BUFFER_BIT);
    ImGui_ImplOpenGL3_RenderDrawData(ImGui::GetDrawData());

    // Multi-viewport pass — required when io.ConfigFlags has
    // ViewportsEnable set (the existing app turns it on for the prefs
    // pop-out and the secondary terminal windows). UpdatePlatformWindows
    // creates / destroys the OS windows for any viewports that appeared
    // or disappeared this frame; RenderPlatformWindowsDefault iterates
    // the rest and renders/swaps each. Order: AFTER main RenderDrawData,
    // BEFORE main SwapWindow. We save and restore the current GL
    // context because multi-viewport hops between platform contexts.
    ImGuiIO& vio = ImGui::GetIO();
    if (vio.ConfigFlags & ImGuiConfigFlags_ViewportsEnable) {
        SDL_Window*   backup_win = SDL_GL_GetCurrentWindow();
        SDL_GLContext backup_ctx = SDL_GL_GetCurrentContext();
        ImGui::UpdatePlatformWindows();
        ImGui::RenderPlatformWindowsDefault();
        SDL_GL_MakeCurrent(backup_win, backup_ctx);
    }

    SDL_GL_SwapWindow(g_window);

    // (Frame cap is enforced in platform_begin_frame via
    // SDL_WaitEventTimeout — that way input events wake the loop
    // immediately instead of waiting out a fixed sleep.)
}

extern "C" int platform_popup_run(int offset_x, int offset_y,
                                  int w, int h, int num_items) {
    if (!g_window || num_items <= 0) return -1;

    SDL_Window* popup = SDL_CreatePopupWindow(
        g_window, offset_x, offset_y, w, h,
        SDL_WINDOW_POPUP_MENU | SDL_WINDOW_OPENGL);
    if (!popup) {
        std::fprintf(stderr, "platform_popup_run: SDL_CreatePopupWindow: %s\n",
                     SDL_GetError());
        return -1;
    }

    SDL_GLContext popup_ctx = SDL_GL_CreateContext(popup);
    if (!popup_ctx) {
        std::fprintf(stderr, "platform_popup_run: SDL_GL_CreateContext: %s\n",
                     SDL_GetError());
        SDL_DestroyWindow(popup);
        return -1;
    }

    SDL_RaiseWindow(popup);

    // Issue the missing grab call SDL3 should have done for
    // SDL_WINDOW_POPUP_MENU — same gap on both Wayland (xdg_popup.grab)
    // and X11 (XGrabPointer). Without this, click-outside doesn't
    // dismiss because pointer events go to whatever app the cursor's
    // over instead of being routed to our popup.
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

    SDL_WindowID popup_id = SDL_GetWindowID(popup);
    int selected = -1;
    bool done = false;
    bool dirty = true;   // first frame needs render
    int  hover_item = -1;

    int popup_sx = 0, popup_sy = 0;
    SDL_GetWindowPosition(popup, &popup_sx, &popup_sy);
    std::fprintf(stderr,
                 "popup: opened id=%u parent=%u at screen=(%d,%d) size=%dx%d\n",
                 (unsigned)popup_id, (unsigned)SDL_GetWindowID(g_window),
                 popup_sx, popup_sy, w, h);

    // Snapshot button state at open time so the first poll doesn't
    // misread the right-button-still-held-from-opening as a fresh
    // outside-click press.
    Uint32 prev_buttons_mask = 0;
    {
        float gx_init, gy_init;
        prev_buttons_mask = SDL_GetGlobalMouseState(&gx_init, &gy_init);
        std::fprintf(stderr,
                     "popup: initial global mouse=(%.0f,%.0f) buttons=0x%x\n",
                     gx_init, gy_init, prev_buttons_mask);
    }

    while (!done) {
        // Render-when-dirty FIRST so the popup is painted before we
        // block on the next event. Earlier version waited on events
        // first and skipped the render on initial 100 ms timeout —
        // popup appeared blank until any input arrived, which felt
        // like "needed two clicks to open".
        if (dirty) {
            dirty = false;
            SDL_GL_MakeCurrent(popup, popup_ctx);
            glViewport(0, 0, w, h);
            glClearColor(0.15f, 0.15f, 0.18f, 1.0f);
            glClear(GL_COLOR_BUFFER_BIT);

            glEnable(GL_SCISSOR_TEST);
            int item_h = h / num_items;
            for (int i = 0; i < num_items; i++) {
                int y0 = i * item_h;
                // GL scissor uses bottom-left origin.
                glScissor(0, h - (y0 + item_h), w, item_h);
                if (i == hover_item)
                    glClearColor(0.30f, 0.45f, 0.65f, 1.0f);
                else
                    glClearColor(0.18f, 0.18f, 0.22f, 1.0f);
                glClear(GL_COLOR_BUFFER_BIT);
            }
            glDisable(GL_SCISSOR_TEST);
            SDL_GL_SwapWindow(popup);
        }

        SDL_Event e;
        bool got_event = SDL_WaitEventTimeout(&e, 30);

        // Polling fallback for click-outside dismiss. SDL3's
        // SDL_WINDOW_POPUP_MENU is supposed to establish an xdg_popup
        // grab so the compositor sends popup_done on click-outside,
        // but on this stack no dismiss event ever arrives — clicks
        // outside go to whichever app the cursor's over and we never
        // see them. So we poll SDL_GetGlobalMouseState every wake
        // (event or 30 ms timeout) and detect button-press-edge while
        // cursor is outside the popup rect ourselves.
        {
            float gx, gy;
            Uint32 buttons = SDL_GetGlobalMouseState(&gx, &gy);
            Uint32 new_press = buttons & ~prev_buttons_mask;
            prev_buttons_mask = buttons;
            if (new_press != 0) {
                bool outside = (gx < popup_sx) || (gx >= popup_sx + w) ||
                               (gy < popup_sy) || (gy >= popup_sy + h);
                std::fprintf(stderr,
                             "popup: poll detected press 0x%x at (%.0f,%.0f) outside=%d\n",
                             new_press, gx, gy, outside ? 1 : 0);
                if (outside) {
                    done = true;
                    continue;
                }
            }
        }

        if (!got_event) continue;
        do {
            // Trace EVERY event (with the popup's window id for ones
            // that carry one) so we can see what SDL actually delivers
            // when the user clicks outside the popup. The original
            // motivating bug — "click anywhere outside dismisses the
            // menu, including over native Wayland apps" — depends on
            // the compositor's popup_done coming through as *some*
            // SDL event. We need to know which one.
            std::fprintf(stderr, "popup: ev type=0x%x", e.type);
            if (e.type >= SDL_EVENT_WINDOW_FIRST &&
                e.type <= SDL_EVENT_WINDOW_LAST) {
                std::fprintf(stderr, " wid=%u", e.window.windowID);
            }
            std::fprintf(stderr, "\n");
            switch (e.type) {
                case SDL_EVENT_QUIT:
                    done = true;
                    break;
                case SDL_EVENT_WINDOW_CLOSE_REQUESTED:
                    if (e.window.windowID == popup_id) done = true;
                    break;
                case SDL_EVENT_WINDOW_HIDDEN:
                    // Compositor dismissed the xdg_popup (click outside
                    // on Wayland) or X11 grab broke — popup is gone.
                    if (e.window.windowID == popup_id) done = true;
                    break;
                case SDL_EVENT_KEY_DOWN:
                    if (e.key.key == SDLK_ESCAPE) done = true;
                    break;
                case SDL_EVENT_MOUSE_MOTION:
                    if (e.motion.windowID == popup_id) {
                        int item_h = h / num_items;
                        int new_hover = item_h > 0
                            ? (int)(e.motion.y) / item_h : -1;
                        if (new_hover < 0 || new_hover >= num_items)
                            new_hover = -1;
                        if (new_hover != hover_item) {
                            hover_item = new_hover;
                            dirty = true;
                        }
                    }
                    break;
                case SDL_EVENT_MOUSE_BUTTON_DOWN:
                    if (e.button.windowID == popup_id) {
                        // With XGrabPointer (X11) or xdg_popup.grab
                        // (Wayland), clicks outside the popup still
                        // arrive as a MOUSE_BUTTON_DOWN for the popup
                        // window — with coords outside [0, w)×[0, h).
                        // Inside → select item; outside → dismiss.
                        int bx = (int)e.button.x;
                        int by = (int)e.button.y;
                        if (bx < 0 || bx >= w || by < 0 || by >= h) {
                            done = true;  // click outside grabbed popup
                            break;
                        }
                        int item_h = h / num_items;
                        if (item_h > 0) {
                            int item = by / item_h;
                            if (item >= 0 && item < num_items) {
                                selected = item;
                                done = true;
                            }
                        }
                    }
                    break;
            }
        } while (!done && SDL_PollEvent(&e));
    }

    xgrab_release();  // no-op on Wayland/elsewhere
    SDL_GL_DestroyContext(popup_ctx);
    SDL_DestroyWindow(popup);
    SDL_GL_MakeCurrent(g_window, g_gl_ctx);  // restore main GL context

    // Synthesize a right-button-up event into the main ImGui context.
    // Reason: the user's right-button RELEASE event arrived while we
    // were inside the popup loop, which consumed it via PollEvent
    // without forwarding it to impl_sdl3. Main's ImGui state therefore
    // still thinks the right button is down, so the next right-click
    // is DOWN→DOWN with no edge → IsMouseClicked doesn't fire and the
    // user appears to need two clicks to reopen the popup.
    ImGui::GetIO().AddMouseButtonEvent(ImGuiMouseButton_Right, false);

    std::fprintf(stderr, "popup: closed, selected=%d\n", selected);
    return selected;
}

extern "C" void platform_request_quit(void) {
    g_quit = true;
    // Wake the loop so it notices the new state without waiting on a
    // timeout. Reuses the PTY wake event since the only consumer
    // (begin_frame's WaitEventTimeout) doesn't distinguish.
    if (g_wake_event_type != 0) {
        SDL_Event e;
        SDL_zero(e);
        e.type = g_wake_event_type;
        SDL_PushEvent(&e);
    }
}

extern "C" void platform_set_bg_color(float r, float g, float b, float a) {
    g_bg_r = r; g_bg_g = g; g_bg_b = b; g_bg_a = a;
}

extern "C" void platform_set_fullscreen(unsigned long window_id, int enable) {
    SDL_Window* w = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!w) return;
    SDL_SetWindowFullscreen(w, enable ? true : false);
}

extern "C" void platform_hide_main_window(void) {
    if (g_window) SDL_HideWindow(g_window);
}

extern "C" void platform_raise_window(unsigned long window_id) {
    SDL_Window* w = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!w) return;
    SDL_RaiseWindow(w);
}

extern "C" void platform_resync_modifiers(void) {
    // On macOS, SDL_GetModState mirrors the per-window NSEvent
    // modifier stream — which AppKit corrupts during window-focus
    // transitions by firing a phantom KEY_UP for held modifiers to
    // the focus-losing window without ever sending the
    // corresponding KEY_DOWN to the focus-gaining one. The result:
    // a user holding Cmd through Cmd+N sees `mod=0x0` on the very
    // next KEY_DOWN(T), and SDL_GetModState reports Cmd-up even
    // though the key is physically held. Read the truth from
    // NSEvent.modifierFlags (IOHID-level hardware state) instead.
    //
    // Other platforms don't have this quirk; SDL_GetModState is
    // accurate there.
#ifdef __APPLE__
    unsigned int mods = platform_cocoa_modifier_flags();
    bool shift = (mods & 0x01) != 0;
    bool ctrl  = (mods & 0x02) != 0;
    bool alt   = (mods & 0x04) != 0;
    bool super_= (mods & 0x08) != 0;
#else
    SDL_Keymod mods = SDL_GetModState();
    bool shift = (mods & SDL_KMOD_SHIFT) != 0;
    bool ctrl  = (mods & SDL_KMOD_CTRL)  != 0;
    bool alt   = (mods & SDL_KMOD_ALT)   != 0;
    bool super_= (mods & SDL_KMOD_GUI)   != 0;
#endif
    ImGuiIO& io = ImGui::GetIO();
    io.AddKeyEvent(ImGuiMod_Shift, shift);
    io.AddKeyEvent(ImGuiMod_Ctrl,  ctrl);
    io.AddKeyEvent(ImGuiMod_Alt,   alt);
    io.AddKeyEvent(ImGuiMod_Super, super_);
}

extern "C" void platform_set_window_icon(unsigned long window_id,
                                         const unsigned char* rgba,
                                         int width, int height) {
    if (!rgba || width <= 0 || height <= 0) return;
    SDL_Window* w = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!w) return;
    // SDL3: SDL_CreateSurfaceFrom takes a pixel pointer + format. RGBA32
    // is platform-endian-aware. Pitch = width * 4 (tight packing).
    SDL_Surface* surf = SDL_CreateSurfaceFrom(
        width, height, SDL_PIXELFORMAT_RGBA32,
        (void*)rgba, width * 4);
    if (!surf) return;
    SDL_SetWindowIcon(w, surf);
    SDL_DestroySurface(surf);
}

extern "C" unsigned long platform_mouse_focus_window_id(void) {
    SDL_Window* w = SDL_GetMouseFocus();
    if (!w) return 0;
    return (unsigned long)SDL_GetWindowID(w);
}

// Plain GL texture helpers — bit-for-bit copy of cimgui-go's sdlbackend
// equivalents (sdl_backend.cpp:274-294). RGBA8, linear filter, no mip.
// Returns the GL texture id reinterpret-cast to ImTextureID (ImU64 in
// our vendored ImGui), which is what ImGui's draw list expects.
extern "C" unsigned long long platform_create_texture(const unsigned char* pixels,
                                                       int width, int height) {
    GLint last_texture;
    GLuint tex_id;
    glGetIntegerv(GL_TEXTURE_BINDING_2D, &last_texture);
    glGenTextures(1, &tex_id);
    glBindTexture(GL_TEXTURE_2D, tex_id);
    glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_LINEAR);
    glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_LINEAR);
    glTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA, width, height, 0,
                 GL_RGBA, GL_UNSIGNED_BYTE, pixels);
    glBindTexture(GL_TEXTURE_2D, last_texture);
    return (unsigned long long)tex_id;
}

extern "C" void platform_delete_texture(unsigned long long tex_id) {
    GLuint id = (GLuint)tex_id;
    glBindTexture(GL_TEXTURE_2D, 0);
    glDeleteTextures(1, &id);
}

extern "C" void platform_post_wake(void) {
    if (g_wake_event_type == 0) return;  // init not yet run
    SDL_Event e;
    SDL_zero(e);
    e.type = g_wake_event_type;
    SDL_PushEvent(&e);
}

extern "C" void platform_shutdown(void) {
    wlgrab_shutdown();

    if (ImGui::GetCurrentContext()) {
        ImGui_ImplOpenGL3_Shutdown();
        ImGui_ImplSDL3_Shutdown();
        ImGui::DestroyContext();
    }
    if (g_gl_ctx) {
        SDL_GL_DestroyContext(g_gl_ctx);
        g_gl_ctx = nullptr;
    }
    if (g_window) {
        SDL_DestroyWindow(g_window);
        g_window = nullptr;
    }
    SDL_Quit();
}
