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
#include "imgui_impl_sdlgpu3.h"

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
// Set once HideMainWindow runs: the main scaffolding window is
// invisible, so platform_end_frame skips its render + swap (a swap
// of a hidden window blocks in Cocoa's GL sync — 51% of the main
// thread on a mac sample).
static int    g_main_hidden = 0;
// Render backend selector: 0 = OpenGL (default), 1 = SDL_GPU
// (XEROTTY_GPU=1 — Metal on macOS, Vulkan on Linux). SDL_GPU is the
// migration path off macOS's deprecated GL-on-Metal emulation, whose
// every swap pays a framebuffer copy plus a synchronous WindowServer
// commit (profiled as the dominant main-thread cost on real
// sessions). One backend, all platforms, pure C — and testable on
// Linux via Vulkan before the mac ever sees it.
static int            g_use_gpu = 0;
static SDL_GPUDevice* g_gpu_dev = nullptr;

extern "C" int platform_use_gpu(void) { return g_use_gpu; }

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

    // SDL disables the screensaver by default, which on Wayland plants a
    // zwp_idle_inhibit_manager_v1 inhibitor on every window. A terminal
    // that's always open thus silently breaks compositor idle session-wide:
    // swayidle never fires, so the screen never auto-locks/blanks and a
    // blanked output can't be woken by input (the resume hook needs an
    // idle->active edge that can never happen). A terminal has no business
    // keeping the screen awake — re-enable the screensaver.
    SDL_EnableScreenSaver();

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

#ifdef __APPLE__
    // SDL_GPU (Metal underneath) is the DEFAULT on macOS — the GL
    // path there is Apple's deprecated GL-on-Metal emulation: every
    // swap pays a framebuffer copy + a synchronous WindowServer
    // commit, plus the share-group/occlusion pathologies we kept
    // patching. User-validated on real hardware. renderer="gl" or
    // XEROTTY_GPU=0 falls back. Linux keeps GL as default until the
    // GPU backend ports the offscreen cell compositor (slice 2) —
    // flipping earlier would LOSE the compositor's perf win there.
    g_use_gpu = 1;
#endif
    // Value-aware: XEROTTY_GPU=0/false/off disables. (The first cut
    // tested PRESENCE, so =0 silently ENABLED the GPU backend — the
    // user's A/B was GPU vs GPU and nobody could tell what was what.)
    if (const char* gp = SDL_getenv("XEROTTY_GPU")) {
        g_use_gpu = !(gp[0] == '\0' || gp[0] == '0' ||
                      SDL_strcasecmp(gp, "false") == 0 || SDL_strcasecmp(gp, "off") == 0);
    }
    SDL_WindowFlags flags = SDL_WINDOW_RESIZABLE
                          | SDL_WINDOW_HIGH_PIXEL_DENSITY;
    if (!g_use_gpu) flags |= SDL_WINDOW_OPENGL;
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

    if (!g_use_gpu) {
        g_gl_ctx = SDL_GL_CreateContext(g_window);
        if (!g_gl_ctx) {
            set_err("SDL_GL_CreateContext");
            SDL_DestroyWindow(g_window);
            g_window = nullptr;
            SDL_Quit();
            return 0;
        }
        SDL_GL_MakeCurrent(g_window, g_gl_ctx);
    }

    // Enable vsync. Try adaptive (-1) first, fall back to standard (1).
    // Driver-side enforcement is best-effort: on AMD/Mesa we observed
    // SwapInterval accepted but silently no-op'd; the manual frame
    // cap in begin_frame covers that case.
    if (g_use_gpu) {
        // SDL_GPU swapchains carry their own present mode (set at
        // backend init below); GL swap-interval doesn't apply.
    } else if (SDL_getenv("XEROTTY_NO_VSYNC")) {
        // Experiment knob: macOS routes GL presents through the
        // Metal shim + a synchronous SkyLight commit; vsync stacks a
        // blocking wait per swap ON TOP of that, serialized across
        // every window on the main thread. Our loop already paces
        // frames (frame cap + render credits), so tearing isn't a
        // practical risk for terminal content.
        SDL_GL_SetSwapInterval(0);
    } else if (!SDL_GL_SetSwapInterval(-1)) {
        SDL_GL_SetSwapInterval(1);
    }

    IMGUI_CHECKVERSION();
    ImGui::CreateContext();
    ImGuiIO& io = ImGui::GetIO();
    io.ConfigFlags |= ImGuiConfigFlags_NavEnableKeyboard;
    ImGui::StyleColorsDark();

    if (g_use_gpu) {
        g_gpu_dev = SDL_CreateGPUDevice(
            SDL_GPU_SHADERFORMAT_SPIRV | SDL_GPU_SHADERFORMAT_DXIL |
            SDL_GPU_SHADERFORMAT_MSL | SDL_GPU_SHADERFORMAT_METALLIB,
            false, nullptr);
        if (!g_gpu_dev) {
            set_err("SDL_CreateGPUDevice");
            return 0;
        }
        if (!SDL_ClaimWindowForGPUDevice(g_gpu_dev, g_window)) {
            set_err("SDL_ClaimWindowForGPUDevice");
            return 0;
        }
        if (!ImGui_ImplSDL3_InitForSDLGPU(g_window)) {
            std::snprintf(g_err, sizeof(g_err), "ImGui_ImplSDL3_InitForSDLGPU failed");
            return 0;
        }
        ImGui_ImplSDLGPU3_InitInfo gi;
        gi.Device = g_gpu_dev;
        gi.ColorTargetFormat = SDL_GetGPUSwapchainTextureFormat(g_gpu_dev, g_window);
        gi.MSAASamples = SDL_GPU_SAMPLECOUNT_1;
        gi.SwapchainComposition = SDL_GPU_SWAPCHAINCOMPOSITION_SDR;
        gi.PresentMode = SDL_getenv("XEROTTY_NO_VSYNC")
            ? SDL_GPU_PRESENTMODE_IMMEDIATE : SDL_GPU_PRESENTMODE_VSYNC;
        if (!ImGui_ImplSDLGPU3_Init(&gi)) {
            std::snprintf(g_err, sizeof(g_err), "ImGui_ImplSDLGPU3_Init failed");
            return 0;
        }
    } else {
        if (!ImGui_ImplSDL3_InitForOpenGL(g_window, g_gl_ctx)) {
            std::snprintf(g_err, sizeof(g_err), "ImGui_ImplSDL3_InitForOpenGL failed");
            return 0;
        }
        if (!ImGui_ImplOpenGL3_Init(glsl_version)) {
            std::snprintf(g_err, sizeof(g_err), "ImGui_ImplOpenGL3_Init failed");
            return 0;
        }
    }

    fprintf(stderr, "xerotty: render backend: %s\n",
            g_use_gpu ? "sdl_gpu (metal/vulkan)" : "opengl");

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

// Occluded-window tracking. SDL3 reports SDL_EVENT_WINDOW_OCCLUDED /
// EXPOSED (macOS: NSWindow.occlusionState; some Wayland compositors:
// frame-callback suspension). A fully covered or hidden window's
// pixels are unreachable, so rendering + vsync-swapping it every
// lava tick is pure waste — with a dozen stacked terminal windows
// that waste DOMINATES (the user-visible report: 14 windows, 75%
// CPU). drain_events maintains the set; the per-viewport render
// loop and the Go draw path skip members. Absence of these events
// (older SDL, X11) leaves every window "visible" = old behavior.
static Uint32 g_occluded_ids[256];
static int    g_occluded_n = 0;

static void occluded_set(Uint32 id, int on) {
    int i = 0;
    for (; i < g_occluded_n; i++) if (g_occluded_ids[i] == id) break;
    if (on) {
        if (i == g_occluded_n && g_occluded_n < 256) g_occluded_ids[g_occluded_n++] = id;
    } else if (i < g_occluded_n) {
        g_occluded_ids[i] = g_occluded_ids[--g_occluded_n];
    }
}

// Per-window damage tracking. The render loop historically rendered
// and vsync-swapped EVERY visible viewport on EVERY frame — so one
// tab streaming output re-painted a dozen idle windows at streaming
// rate. Each frame, a viewport renders only when (a) an SDL event
// addressed its window this frame (input, expose, move, resize), or
// (b) the Go side marked it dirty (content change, focus, glow,
// overlays). g_damage_enabled=0 reverts to render-everything.
static Uint32 g_evented_ids[256];
static int    g_evented_n = 0;
static Uint32 g_dirty_ids[256];
static int    g_dirty_n = 0;
static int    g_damage_enabled = 1;

static void evented_add(Uint32 id) {
    if (id == 0) return;
    for (int i = 0; i < g_evented_n; i++) if (g_evented_ids[i] == id) return;
    if (g_evented_n < 256) g_evented_ids[g_evented_n++] = id;
}

extern "C" void platform_set_damage_enabled(int on) { g_damage_enabled = on; }

extern "C" void platform_mark_viewport_dirty(unsigned long window_id) {
    Uint32 id = (Uint32)window_id;
    if (id == 0) return;
    for (int i = 0; i < g_dirty_n; i++) if (g_dirty_ids[i] == id) return;
    if (g_dirty_n < 256) g_dirty_ids[g_dirty_n++] = id;
}

// Newborn warm-up: macOS DROPS presents for a window that isn't
// fully ordered-in yet. Pre-damage-tracking, every window rendered
// every frame so the dropped early presents were invisibly retried;
// with damage tracking the creation-event dirt washed out before the
// window became presentable and it sat BLANK until a click marked it
// focus-dirty (mac GL regression). Render every frame until the OS
// confirms visibility (EXPOSED / FOCUS_GAINED) AND a settle period
// passes — then damage tracking takes over. Steady-state cost: one
// timestamp compare.
#define XT_WARMUP_MS 2000
static Uint32 g_warm_ids[64];
static Uint64 g_warm_until[64];
static int    g_warm_n = 0;

static void warmup_note(Uint32 id) {
    if (id == 0) return;
    Uint64 until = SDL_GetTicks() + XT_WARMUP_MS;
    for (int i = 0; i < g_warm_n; i++)
        if (g_warm_ids[i] == id) { return; } // already warming
    if (g_warm_n < 64) { g_warm_ids[g_warm_n] = id; g_warm_until[g_warm_n] = until; g_warm_n++; }
}

// warmup_seen warms a window id the first time it is ever observed
// in the viewport loop — belt-and-suspenders for SHOWN events that
// raced or predated our event watch.
static Uint32 g_seen_ids[256];
static int    g_seen_n = 0;
static void warmup_seen(Uint32 id) {
    if (id == 0) return;
    for (int i = 0; i < g_seen_n; i++) if (g_seen_ids[i] == id) return;
    if (g_seen_n < 256) g_seen_ids[g_seen_n++] = id;
    warmup_note(id);
}

static int warmup_active(Uint32 id) {
    Uint64 now = SDL_GetTicks();
    for (int i = 0; i < g_warm_n; i++) {
        if (g_warm_ids[i] != id) continue;
        if (now < g_warm_until[i]) return 1;
        g_warm_ids[i] = g_warm_ids[--g_warm_n];
        g_warm_until[i] = g_warm_until[g_warm_n];
        return 0;
    }
    return 0;
}

static int window_wants_render(Uint32 id) {
    if (!g_damage_enabled) return 1;
    if (warmup_active(id)) return 1;
    for (int i = 0; i < g_evented_n; i++) if (g_evented_ids[i] == id) return 1;
    for (int i = 0; i < g_dirty_n; i++) if (g_dirty_ids[i] == id) return 1;
    return 0;
}

extern "C" int platform_window_occluded(unsigned long window_id) {
    for (int i = 0; i < g_occluded_n; i++)
        if (g_occluded_ids[i] == (Uint32)window_id) return 1;
    return 0;
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
        // Damage: remember which windows events addressed this frame.
        // The windowID field lives at the same struct offset for the
        // event families we care about; read it per family.
        if (e.type >= SDL_EVENT_WINDOW_FIRST && e.type <= SDL_EVENT_WINDOW_LAST) {
            evented_add(e.window.windowID);
        } else switch (e.type) {
            case SDL_EVENT_KEY_DOWN: case SDL_EVENT_KEY_UP:
                evented_add(e.key.windowID); break;
            case SDL_EVENT_TEXT_INPUT:
                evented_add(e.text.windowID); break;
            case SDL_EVENT_MOUSE_MOTION:
                evented_add(e.motion.windowID); break;
            case SDL_EVENT_MOUSE_BUTTON_DOWN: case SDL_EVENT_MOUSE_BUTTON_UP:
                evented_add(e.button.windowID); break;
            case SDL_EVENT_MOUSE_WHEEL:
                evented_add(e.wheel.windowID); break;
            default: break;
        }
        switch (e.type) {
            case SDL_EVENT_QUIT:
                g_quit = true;
                break;
            case SDL_EVENT_WINDOW_CLOSE_REQUESTED:
                if (SDL_GetWindowFromID(e.window.windowID) == g_window)
                    g_quit = true;
                break;
            case SDL_EVENT_WINDOW_OCCLUDED:
                if (SDL_getenv("XEROTTY_DEBUG_OCCLUSION"))
                    fprintf(stderr, "[occl] window %u OCCLUDED\n", e.window.windowID);
                occluded_set(e.window.windowID, 1);
                break;
            case SDL_EVENT_WINDOW_SHOWN:
                // Fresh window: keep rendering through Cocoa's
                // order-in latency (see warmup_note).
                warmup_note(e.window.windowID);
                occluded_set(e.window.windowID, 0);
                break;
            case SDL_EVENT_WINDOW_EXPOSED:
            case SDL_EVENT_WINDOW_RESTORED:
            case SDL_EVENT_WINDOW_FOCUS_GAINED:
                if (SDL_getenv("XEROTTY_DEBUG_OCCLUSION"))
                    fprintf(stderr, "[occl] window %u visible (ev %u)\n", e.window.windowID, e.type);
                occluded_set(e.window.windowID, 0);
                break;
            case SDL_EVENT_WINDOW_DESTROYED:
                occluded_set(e.window.windowID, 0);
                break;
            default:
                break;
        }
    }
    return n;
}

extern "C" int platform_begin_frame(void) {
    // New frame: previous frame's damage/evented sets expire.
    g_evented_n = 0;
    g_dirty_n = 0;

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
    if (g_use_gpu) ImGui_ImplSDLGPU3_NewFrame();
    else           ImGui_ImplOpenGL3_NewFrame();
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

// Framebuffer capture must happen INSIDE the render path, after
// RenderDrawData and before SwapWindow (post-swap back-buffer
// contents are undefined). Go requests a capture; the next frame's
// pre-swap point fills the buffer.
static unsigned char* g_cap_buf = nullptr;
static int g_cap_max = 0;
static int g_cap_w = 0, g_cap_h = 0;
static int g_cap_pending = 0, g_cap_ok = 0;
static unsigned int g_cap_window_id = 0; // 0 = main window

extern "C" void platform_request_capture(unsigned long window_id, unsigned char* out, int max_bytes) {
    g_cap_buf = out;
    g_cap_max = max_bytes;
    g_cap_w = g_cap_h = 0;
    g_cap_ok = 0;
    g_cap_window_id = (unsigned int)window_id;
    g_cap_pending = 1;
}

// Called from the backend's per-viewport pre-swap point with that
// viewport's GL context current.
extern "C" void platform_capture_maybe(SDL_Window* win) {
    if (!g_cap_pending || !win) return;
    if (SDL_GetWindowID(win) != g_cap_window_id) return;
    g_cap_pending = 0;
    int fw = 0, fh = 0;
    SDL_GetWindowSizeInPixels(win, &fw, &fh);
    if (fw <= 0 || fh <= 0 || fw * fh * 4 > g_cap_max || !g_cap_buf) return;
    glPixelStorei(GL_PACK_ALIGNMENT, 1);
    glReadBuffer(GL_BACK);
    glReadPixels(0, 0, fw, fh, GL_RGBA, GL_UNSIGNED_BYTE, g_cap_buf);
    g_cap_w = fw;
    g_cap_h = fh;
    g_cap_ok = 1;
}

extern "C" int platform_capture_result(int* w, int* h) {
    if (!g_cap_ok) return 0;
    *w = g_cap_w;
    *h = g_cap_h;
    return 1;
}

static void platform_do_capture_locked(void) {
    // Called with g_window's context current, post-render pre-swap.
    g_cap_pending = 0;
    int fw = 0, fh = 0;
    SDL_GetWindowSizeInPixels(g_window, &fw, &fh);
    if (fw <= 0 || fh <= 0 || fw * fh * 4 > g_cap_max || !g_cap_buf) return;
    glPixelStorei(GL_PACK_ALIGNMENT, 1);
    glReadBuffer(GL_BACK);
    glReadPixels(0, 0, fw, fh, GL_RGBA, GL_UNSIGNED_BYTE, g_cap_buf);
    g_cap_w = fw;
    g_cap_h = fh;
    g_cap_ok = 1;
}


// backend_present renders + presents everything for the current
// frame: main window (when visible) and all secondary viewports.
// respect_damage=0 forces every visible viewport to render — the
// macOS live-resize watch uses that (a resize IS a visual change,
// and the damage/evented sets belong to the normal frame loop).
static void backend_present(int respect_damage);

extern "C" void platform_end_frame(void) {
    backend_present(1);
}

static void backend_present(int respect_damage) {
    // Caller (Go) has already called ImGui::Render via cimgui-go.
    //
    // The main scaffolding window is HIDDEN once multi-viewport takes
    // over (every user-visible window is a secondary viewport), yet
    // this function used to clear + render + SwapWindow it EVERY
    // frame. On macOS that swap was catastrophic: Cocoa can't present
    // an invisible window, so SDL's GL swap parked in a condition-
    // variable timeout — a mac `sample` showed it as 51% of the main
    // thread's wallclock, serializing ahead of every real window's
    // present. Skip the main window's render + swap entirely while
    // hidden (its draw data is empty anyway); a capture targeting
    // window id 0 still takes the full path below.
    int main_visible = !g_main_hidden || (g_cap_pending && g_cap_window_id == 0);

    if (g_use_gpu) {
        // SDL_GPU path: explicit command buffer + render pass for the
        // main window (only while visible — it hides once viewports
        // take over); secondary viewports render through
        // imgui_impl_sdlgpu3's Renderer hooks, which acquire their
        // own swapchains and submit per window. Presents are
        // swapchain operations (Metal/Vulkan underneath) — none of
        // the GL-on-Metal copy + synchronous WindowServer commit.
        if (main_visible) {
            SDL_GPUCommandBuffer* cmd = SDL_AcquireGPUCommandBuffer(g_gpu_dev);
            if (cmd) {
                SDL_GPUTexture* swtex = nullptr;
                SDL_WaitAndAcquireGPUSwapchainTexture(cmd, g_window, &swtex, nullptr, nullptr);
                if (swtex) {
                    ImDrawData* dd = ImGui::GetDrawData();
                    ImGui_ImplSDLGPU3_PrepareDrawData(dd, cmd);
                    SDL_GPUColorTargetInfo ct = {};
                    ct.texture = swtex;
                    ct.load_op = SDL_GPU_LOADOP_CLEAR;
                    ct.store_op = SDL_GPU_STOREOP_STORE;
                    ct.clear_color = { g_bg_r, g_bg_g, g_bg_b, g_bg_a };
                    SDL_GPURenderPass* pass = SDL_BeginGPURenderPass(cmd, &ct, 1, nullptr);
                    ImGui_ImplSDLGPU3_RenderDrawData(dd, cmd, pass);
                    SDL_EndGPURenderPass(pass);
                }
                SDL_SubmitGPUCommandBuffer(cmd);
            }
        }
        ImGuiIO& gio = ImGui::GetIO();
        if (gio.ConfigFlags & ImGuiConfigFlags_ViewportsEnable) {
            ImGui::UpdatePlatformWindows();
            ImGuiPlatformIO& pio = ImGui::GetPlatformIO();
            for (int i = 1; i < pio.Viewports.Size; i++) {
                ImGuiViewport* vp = pio.Viewports[i];
                if (vp->Flags & ImGuiViewportFlags_IsMinimized) continue;
                Uint32 vpid = (Uint32)(intptr_t)vp->PlatformHandle;
                // First sighting of a viewport warms it up even if its
                // SHOWN event was missed or preceded our tracking.
                warmup_seen(vpid);
                // Warm-up overrides BOTH skip gates: on macOS a fresh
                // window is born with occlusionState "not visible" —
                // SDL marks it OCCLUDED at creation, and if EXPOSED
                // never follows (or beat our tracking), the occlusion
                // gate alone left it skipped FOREVER: blank until a
                // click's FOCUS_GAINED cleared the flag.
                if (!warmup_active(vpid)) {
                    if (platform_window_occluded((unsigned long)vpid)) continue;
                    if (respect_damage && !window_wants_render(vpid)) continue;
                }
                if (pio.Platform_RenderWindow) pio.Platform_RenderWindow(vp, nullptr);
                if (pio.Renderer_RenderWindow) pio.Renderer_RenderWindow(vp, nullptr);
                if (pio.Platform_SwapBuffers) pio.Platform_SwapBuffers(vp, nullptr);
                if (pio.Renderer_SwapBuffers) pio.Renderer_SwapBuffers(vp, nullptr);
            }
        }
        return;
    }

    if (main_visible) {
        int w = 0, h = 0;
        SDL_GetWindowSizeInPixels(g_window, &w, &h);
        glViewport(0, 0, w, h);
        glClearColor(g_bg_r, g_bg_g, g_bg_b, g_bg_a);
        glClear(GL_COLOR_BUFFER_BIT);
        ImGui_ImplOpenGL3_RenderDrawData(ImGui::GetDrawData());
    }

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
        // Hand-rolled RenderPlatformWindowsDefault: identical loop,
        // plus an occlusion skip — a fully covered window's render +
        // vsync swap is pure waste (see g_occluded_ids above). The
        // viewport keeps existing; on EXPOSED the event wakes the
        // loop and the next frame repaints it with current content.
        {
            ImGuiPlatformIO& pio = ImGui::GetPlatformIO();
            for (int i = 1; i < pio.Viewports.Size; i++) {
                ImGuiViewport* vp = pio.Viewports[i];
                if (vp->Flags & ImGuiViewportFlags_IsMinimized) continue;
                Uint32 vpid = (Uint32)(intptr_t)vp->PlatformHandle;
                // First sighting of a viewport warms it up even if its
                // SHOWN event was missed or preceded our tracking.
                warmup_seen(vpid);
                // Warm-up overrides BOTH skip gates: on macOS a fresh
                // window is born with occlusionState "not visible" —
                // SDL marks it OCCLUDED at creation, and if EXPOSED
                // never follows (or beat our tracking), the occlusion
                // gate alone left it skipped FOREVER: blank until a
                // click's FOCUS_GAINED cleared the flag.
                if (!warmup_active(vpid)) {
                    if (platform_window_occluded((unsigned long)vpid)) continue;
                    if (respect_damage && !window_wants_render(vpid)) continue;
                }
                if (pio.Platform_RenderWindow) pio.Platform_RenderWindow(vp, nullptr);
                if (pio.Renderer_RenderWindow) pio.Renderer_RenderWindow(vp, nullptr);
                if (pio.Platform_SwapBuffers) pio.Platform_SwapBuffers(vp, nullptr);
                if (pio.Renderer_SwapBuffers) pio.Renderer_SwapBuffers(vp, nullptr);
            }
        }
        SDL_GL_MakeCurrent(backup_win, backup_ctx);
    }

    // Capture targeting the main window (or unspecified) happens at
    // the main pre-swap; child viewports capture in the backend's
    // per-viewport swap hook (platform_capture_maybe).
    if (g_cap_pending && (g_cap_window_id == 0 || (g_window && SDL_GetWindowID(g_window) == g_cap_window_id))) {
        SDL_GL_MakeCurrent(g_window, g_gl_ctx);
        platform_do_capture_locked();
    }
    if (main_visible) SDL_GL_SwapWindow(g_window);

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
    g_main_hidden = 1;
}

extern "C" void platform_raise_window(unsigned long window_id) {
    SDL_Window* w = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!w) return;
    SDL_RaiseWindow(w);
}

extern "C" int platform_window_input_focus(unsigned long window_id) {
    SDL_Window* w = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!w) return 0;
    return (SDL_GetWindowFlags(w) & SDL_WINDOW_INPUT_FOCUS) ? 1 : 0;
}

extern "C" int platform_any_window_input_focus(void) {
    int count = 0;
    SDL_Window** ws = SDL_GetWindows(&count);
    if (!ws) return 0;
    int any = 0;
    for (int i = 0; i < count; i++) {
        if (SDL_GetWindowFlags(ws[i]) & SDL_WINDOW_INPUT_FOCUS) { any = 1; break; }
    }
    SDL_free(ws);
    return any;
}

extern "C" void platform_set_window_opacity(unsigned long window_id, float opacity) {
    SDL_Window* w = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!w) return;
    SDL_SetWindowOpacity(w, opacity);
}

extern "C" int platform_get_window_usable_bounds(unsigned long window_id,
                                                 int* out_x, int* out_y,
                                                 int* out_w, int* out_h) {
    SDL_Window* w = SDL_GetWindowFromID((SDL_WindowID)window_id);
    if (!w) return 0;
    SDL_DisplayID did = SDL_GetDisplayForWindow(w);
    if (did == 0) return 0;
    SDL_Rect r;
    if (!SDL_GetDisplayUsableBounds(did, &r)) {
        // Fallback: full display bounds (includes Dock area). Better
        // than nothing — at least the popup gets clamped to screen.
        if (!SDL_GetDisplayBounds(did, &r)) return 0;
    }
    if (out_x) *out_x = r.x;
    if (out_y) *out_y = r.y;
    if (out_w) *out_w = r.w;
    if (out_h) *out_h = r.h;
    return 1;
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
// SDL_GPU texture upload: staging transfer buffer -> copy pass.
// Returns false on any failure.
static bool xt_gpu_upload(SDL_GPUTexture* tex, int x, int y, int w, int h,
                          const unsigned char* pixels) {
    if (!pixels || w <= 0 || h <= 0) return true; // nothing to do
    SDL_GPUTransferBufferCreateInfo tci = {};
    tci.usage = SDL_GPU_TRANSFERBUFFERUSAGE_UPLOAD;
    tci.size = (Uint32)(w * h * 4);
    SDL_GPUTransferBuffer* tb = SDL_CreateGPUTransferBuffer(g_gpu_dev, &tci);
    if (!tb) return false;
    void* map = SDL_MapGPUTransferBuffer(g_gpu_dev, tb, false);
    if (!map) { SDL_ReleaseGPUTransferBuffer(g_gpu_dev, tb); return false; }
    SDL_memcpy(map, pixels, (size_t)w * h * 4);
    SDL_UnmapGPUTransferBuffer(g_gpu_dev, tb);
    SDL_GPUCommandBuffer* cmd = SDL_AcquireGPUCommandBuffer(g_gpu_dev);
    if (!cmd) { SDL_ReleaseGPUTransferBuffer(g_gpu_dev, tb); return false; }
    SDL_GPUCopyPass* cp = SDL_BeginGPUCopyPass(cmd);
    SDL_GPUTextureTransferInfo src = {};
    src.transfer_buffer = tb;
    src.pixels_per_row = (Uint32)w;
    src.rows_per_layer = (Uint32)h;
    SDL_GPUTextureRegion dst = {};
    dst.texture = tex;
    dst.x = (Uint32)x; dst.y = (Uint32)y;
    dst.w = (Uint32)w; dst.h = (Uint32)h; dst.d = 1;
    SDL_UploadToGPUTexture(cp, &src, &dst, false);
    SDL_EndGPUCopyPass(cp);
    SDL_SubmitGPUCommandBuffer(cmd);
    SDL_ReleaseGPUTransferBuffer(g_gpu_dev, tb);
    return true;
}

extern "C" unsigned long long platform_create_texture(const unsigned char* pixels,
                                                       int width, int height) {
    if (g_use_gpu) {
        SDL_GPUTextureCreateInfo ci = {};
        ci.type = SDL_GPU_TEXTURETYPE_2D;
        ci.format = SDL_GPU_TEXTUREFORMAT_R8G8B8A8_UNORM;
        ci.usage = SDL_GPU_TEXTUREUSAGE_SAMPLER;
        ci.width = (Uint32)width;
        ci.height = (Uint32)height;
        ci.layer_count_or_depth = 1;
        ci.num_levels = 1;
        SDL_GPUTexture* t = SDL_CreateGPUTexture(g_gpu_dev, &ci);
        if (!t) return 0;
        if (!xt_gpu_upload(t, 0, 0, width, height, pixels)) {
            SDL_ReleaseGPUTexture(g_gpu_dev, t);
            return 0;
        }
        return (unsigned long long)(uintptr_t)t;
    }
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

extern "C" void platform_update_texture(unsigned long long tex_id, int x, int y,
                                        int width, int height,
                                        const unsigned char* pixels) {
    if (g_use_gpu) {
        xt_gpu_upload((SDL_GPUTexture*)(uintptr_t)tex_id, x, y, width, height, pixels);
        return;
    }
    GLint last_texture;
    glGetIntegerv(GL_TEXTURE_BINDING_2D, &last_texture);
    glBindTexture(GL_TEXTURE_2D, (GLuint)tex_id);
    glTexSubImage2D(GL_TEXTURE_2D, 0, x, y, width, height,
                    GL_RGBA, GL_UNSIGNED_BYTE, pixels);
    glBindTexture(GL_TEXTURE_2D, last_texture);
}

extern "C" void platform_delete_texture(unsigned long long tex_id) {
    if (g_use_gpu) {
        SDL_ReleaseGPUTexture(g_gpu_dev, (SDL_GPUTexture*)(uintptr_t)tex_id);
        return;
    }
    GLuint id = (GLuint)tex_id;
    glBindTexture(GL_TEXTURE_2D, 0);
    glDeleteTextures(1, &id);
}

// --- macOS live-resize support (called from liveresize_darwin.go's
// event watch, which must drive frames while AppKit's tracking loop
// starves the normal pump) ---
extern "C" SDL_Window* platform_main_window(void) { return g_window; }

extern "C" void platform_backend_new_frame(void) {
    if (g_use_gpu) ImGui_ImplSDLGPU3_NewFrame();
    else           ImGui_ImplOpenGL3_NewFrame();
}

extern "C" void platform_backend_present_all(void) {
    backend_present(0);
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
        if (g_use_gpu) ImGui_ImplSDLGPU3_Shutdown();
        else           ImGui_ImplOpenGL3_Shutdown();
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
