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

// Render-on-demand state. The loop only renders when there's a reason
// to (an SDL event, a PTY/daemon wake, or a requested settle frame);
// when idle it parks in SDL_WaitEventTimeout at ~0% CPU instead of
// re-rendering at the frame cap. g_render_credits is the number of
// frames still owed: any event refills it to RENDER_SETTLE so ImGui's
// hover/active state animates a beat after the last input before the
// loop idles. g_idle_timeout_ms is the longest the idle wait will
// block — set by Go each frame to the next cursor-blink toggle (so a
// blinking cursor still ticks) or a long safety-net otherwise, so a
// missed wake can't freeze the UI for more than that.
//
// RENDER_SETTLE: frames to draw after the LAST input so ImGui finishes
// its hover/active transitions before idling. INIT is larger so the
// first frames (font/layout settle) always render.
const int     RENDER_SETTLE      = 3;
const int     RENDER_SETTLE_INIT = 8;
int           g_render_credits = RENDER_SETTLE_INIT;
int           g_idle_timeout_ms = 250;

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
// Doesn't block. Returns the number of events processed so the caller
// can decide whether anything happened that warrants a render.
static int drain_events() {
    SDL_Event e;
    int n = 0;
    while (SDL_PollEvent(&e)) {
        n++;
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
    return n;
}

extern "C" int platform_begin_frame(void) {
    // Render-on-demand with a frame cap.
    //
    // ACTIVE (we owe render credits — recent input/wake): pace to the
    // 4x-refresh cap while draining events, so a mouse-motion flood
    // can't burst FPS above the cap (jittery on X11) but drag still
    // tracks at mouse-poll rate.
    //
    // IDLE (no credits): block in SDL_WaitEventTimeout until an event
    // arrives or the Go-set idle timeout (next cursor-blink toggle, or
    // a safety-net) elapses — so a static screen parks at ~0% CPU
    // instead of re-rendering the whole UI at the cap.
    static Uint64 next_render_ns = 0;
    if (next_render_ns == 0) next_render_ns = SDL_GetTicksNS();

    if (drain_events() > 0) g_render_credits = RENDER_SETTLE;
    if (g_quit) return 0;

    if (g_render_credits <= 0) {
        // Idle: park until something happens. Loop so a wake that
        // produced no events (e.g. an irrelevant queued event) doesn't
        // force a render — except a blink-timeout wake, which renders
        // exactly one frame to toggle the cursor.
        for (;;) {
            int to = g_idle_timeout_ms;
            if (to > 0) SDL_WaitEventTimeout(nullptr, to);
            else        SDL_WaitEvent(nullptr);
            int got = drain_events();
            if (g_quit) return 0;
            if (got > 0) { g_render_credits = RENDER_SETTLE; break; }
            if (to > 0)  { g_render_credits = 1; break; } // blink/safety tick
        }
        // Reset pacing so the first post-idle frame isn't throttled.
        next_render_ns = SDL_GetTicksNS();
    } else {
        // Active: wait out the frame-cap window, draining as we go.
        while (!g_quit) {
            Uint64 now = SDL_GetTicksNS();
            if (now >= next_render_ns) break;
            // Round up so we don't busy-spin on sub-ms residuals.
            Sint32 ms_left = (Sint32)((next_render_ns - now + 999999ull) / 1000000ull);
            if (ms_left <= 0) break;
            SDL_WaitEventTimeout(nullptr, ms_left);
            if (drain_events() > 0) g_render_credits = RENDER_SETTLE;
        }
        if (g_quit) return 0;
    }

    // Advance deadline. If a frame ran long, snap forward instead of
    // accumulating "owed" frames (would cause a catch-up burst once
    // load drops).
    next_render_ns += g_target_frame_ns;
    Uint64 now = SDL_GetTicksNS();
    if (next_render_ns < now) next_render_ns = now + g_target_frame_ns;

    if (g_render_credits > 0) g_render_credits--;
    ImGui_ImplOpenGL3_NewFrame();
    ImGui_ImplSDL3_NewFrame();
    // Caller (Go) calls ImGui::NewFrame via cimgui-go next.
    return 1;
}

// platform_set_idle_timeout_ms sets the longest the idle render wait
// will block before forcing a frame. Go calls this each frame: the ms
// until the next cursor-blink toggle when a focused cursor is blinking
// (so the blink keeps ticking), else a longer safety-net. <=0 would
// block indefinitely; callers pass a finite safety-net instead so a
// missed wake can't freeze the UI.
extern "C" void platform_set_idle_timeout_ms(int ms) {
    g_idle_timeout_ms = ms;
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

// platform_ensure_text_input re-asserts SDL text input on the given
// window if it isn't already active. SDL3 only delivers
// SDL_EVENT_TEXT_INPUT (the source of typed characters the terminal
// reads via io.InputQueueCharacters) while text input is ACTIVE on a
// window. The ImGui SDL3 backend's PlatformSetImeData calls
// SDL_StopTextInput on a window whenever an InputText there
// deactivates — so any dialog (rename, connect, search) pinned to the
// terminal's own viewport turns the terminal's character input OFF
// when it closes, leaving mapped keys working but typing dead. The
// app calls this every frame the terminal owns keyboard input,
// re-establishing the invariant "focused terminal window has text
// input active." Guarded by SDL_TextInputActive so it's a no-op when
// already on and never fights the backend while an InputText is up.
extern "C" void platform_ensure_text_input(unsigned long window_id) {
    SDL_Window* w = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!w) return;
    if (!SDL_TextInputActive(w)) SDL_StartTextInput(w);
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
