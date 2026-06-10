// SDL3 + Dear ImGui platform shim — C API consumed by cgo.
//
// Frame loop split: C owns event pump + impl-backend NewFrame, Go owns
// ImGui::NewFrame and the user draw, C owns Render+swap. This lets the
// Go side drive widgets through cimgui-go's API while we keep SDL +
// the GL context entirely on the C side.
//
// See docs/SDL3_PLAN.md.

#ifndef XEROTTY_PLATFORM_SDL3_H
#define XEROTTY_PLATFORM_SDL3_H

#ifdef __cplusplus
extern "C" {
#endif

// platform_init creates an SDL3 window + GL context and initializes the
// Dear ImGui context with the SDL3 and OpenGL3 backends.
// Returns 1 on success, 0 on failure (see platform_last_error).
int platform_init(const char* title, int width, int height);

// platform_last_error returns the last init failure message. Never NULL.
const char* platform_last_error(void);

// platform_video_driver returns the active SDL video driver name
// ("wayland", "x11", "cocoa", ...). For Phase 0 acceptance / debugging.
const char* platform_video_driver(void);

// platform_begin_frame pumps SDL events, calls the impl-backend
// NewFrame functions, and returns 1 to continue or 0 if quit was
// requested (close button or QUIT event). The caller MUST follow a
// successful begin_frame with ImGui::NewFrame (via cimgui-go), the
// per-frame draws, ImGui::Render, then platform_end_frame.
int platform_begin_frame(void);

// platform_end_frame clears the framebuffer, renders the ImGui draw
// data from the current context, and swaps the buffer.
void platform_end_frame(void);

void platform_shutdown(void);

// platform_request_quit signals the main loop to exit on its next
// iteration. Equivalent to the OS sending us a quit event; thread-safe.
void platform_request_quit(void);

// platform_set_bg_color updates the clear color used by platform_end_frame.
void platform_set_bg_color(float r, float g, float b, float a);

// platform_set_fullscreen toggles SDL_SetWindowFullscreen on the
// SDL_Window whose ID is window_id (cast from
// ImGuiViewport::PlatformHandle for popped-out viewports, or the
// main window's ID for the primary). uintptr_t for cgo friendliness.
void platform_set_fullscreen(unsigned long window_id, int enable);

// platform_hide_main_window hides the SDL_Window created by
// platform_init without destroying it. Used in the "hidden carrier"
// model: the main SDL_Window holds the GL context but every
// user-visible Window is a multi-viewport pop-out.
void platform_hide_main_window(void);

// platform_mouse_focus_window_id returns the SDL_WindowID of the
// window the OS cursor is currently over, or 0 if it isn't over
// any of our windows. Used by cross-window drag-drop to decide
// which Window receives a drop on mouse release — ImGui's per-
// window hover detection isn't usable mid-drag because the source
// window's captured active-item keeps cursor coords routed to it.
unsigned long platform_mouse_focus_window_id(void);

// Texture management — wraps glGenTextures/glTexImage2D, returns the
// GL texture id reinterpret-cast to an ImTextureID (which our vendored
// ImGui defines as ImU64). Caller passes the resulting handle to
// ImGui::ImageRef / DrawList::AddImage etc.
unsigned long long platform_create_texture(const unsigned char* pixels, int width, int height);
void               platform_delete_texture(unsigned long long tex_id);
void               platform_update_texture(unsigned long long tex_id, int x, int y, int width, int height, const unsigned char* pixels);
void               platform_drawlist_add_quads(void* dl_ptr, const void* quads_ptr, int n);
int                platform_render_quads_to_texture(unsigned long long* tex_inout, int realloc_tex, int px_w, int px_h, float disp_x, float disp_y, float disp_w, float disp_h, const void* quads_ptr, int n);
void               platform_drawlist_blit_premul(void* dl_ptr, unsigned long long tex, float x0, float y0, float x1, float y1);
void               platform_request_capture(unsigned long window_id, unsigned char* out, int max_bytes);
int                platform_capture_result(int* w, int* h);

// platform_resync_modifiers reads the OS-level modifier state (Cmd /
// Shift / Ctrl / Alt) and feeds it back into ImGui's IO as
// AddKeyEvent for ImGuiMod_*. Used after a window-focus transition
// where macOS drops modifier state mid-flight — e.g. holding Cmd
// while a new window pops via Cmd+N leaves ImGui thinking Cmd is
// no longer down, so the immediately-following Cmd+T doesn't match
// its keybind. This re-asserts the truth so the next keypress
// dispatches correctly.
void platform_resync_modifiers(void);

// platform_raise_window raises + activates + key-focuses the SDL_Window
// with the given ID. Used after spawnWindow so the freshly-created
// viewport NSWindow immediately becomes the OS-focused window —
// without this, macOS leaves keyboard focus on the spawning window
// until the user clicks the new one, so keybinds like Cmd+T received
// in the first frame after Cmd+N route to the wrong window.
void platform_raise_window(unsigned long window_id);

// platform_get_window_usable_bounds returns the usable screen rect
// (excludes the macOS Dock / Linux panel) for the display containing
// the SDL_Window with the given ID. Writes x/y/w/h via out-pointers
// and returns 1 on success, 0 if the window doesn't exist or SDL
// can't query bounds. Coords are absolute desktop screen pixels.
//
// Callers use it for popup-positioning fallback: when a dropdown
// anchored below a button would overflow the dock, flip it up.
int platform_get_window_usable_bounds(unsigned long window_id,
                                      int* out_x, int* out_y,
                                      int* out_w, int* out_h);

// platform_set_window_opacity sets whole-window opacity (0..1) on the
// SDL_Window with the given ID via SDL_SetWindowOpacity — the compositor
// blends the whole window. Restores the documented `opacity` config that
// the SDL2→SDL3 migration dropped. No-op if the compositor doesn't
// support per-window opacity.
void platform_set_window_opacity(unsigned long window_id, float opacity);
int platform_window_input_focus(unsigned long window_id);
int platform_any_window_input_focus(void);

// platform_ensure_text_input re-asserts SDL text input on the given
// window if not already active, so the terminal keeps receiving
// SDL_EVENT_TEXT_INPUT (typed characters) after an ImGui InputText
// dialog on the same viewport closes and the backend stopped it.
void platform_ensure_text_input(unsigned long window_id);

// platform_set_window_icon attaches an RGBA8 pixel buffer to the
// SDL_Window with the given ID via SDL_SetWindowIcon. The pixels are
// row-major, top-to-bottom, with no padding (pitch = width * 4). On
// Linux the WM uses this for taskbar / Alt-Tab / window-list display;
// on macOS the bundle's .icns wins for the Dock but per-window
// rendering may still pick it up. No-op if window_id is unknown.
void platform_set_window_icon(unsigned long window_id,
                              const unsigned char* rgba, int width, int height);

// platform_post_wake pushes a user-defined SDL event into the queue.
// Safe to call from any Go goroutine (SDL_PushEvent is thread-safe).
// Used by the PTY reader to break the main loop out of
// SDL_WaitEventTimeout immediately when new terminal output arrives,
// so the next frame renders without waiting out the frame-cap window.
void platform_post_wake(void);

// platform_set_idle_timeout_ms bounds the idle render wait (ms). Called
// each frame by Go with the time until the next cursor-blink toggle (or
// a safety-net) so an idle UI parks at ~0% CPU yet a blinking cursor
// keeps ticking and a missed wake can't freeze the UI.
void platform_set_idle_timeout_ms(int ms);

#ifdef __cplusplus
}
#endif

#endif
