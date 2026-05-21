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

// platform_popup_run opens an SDL_WINDOW_POPUP_MENU child of the main
// window at (offset_x, offset_y) relative to the parent's top-left.
// Blocks until the popup dismisses, returning:
//   >= 0     index of the strip the user clicked on (0..num_items-1)
//   -1       dismissed without selection (compositor popup_done,
//            click-outside grab broken, Escape, or window close)
// On Wayland the popup is a real xdg_popup (compositor handles
// click-anywhere-outside dismiss including over native apps). On X11
// it's an override-redirect window with _NET_WM_WINDOW_TYPE_POPUP_MENU
// and an active XGrabPointer for the same behavior.
//
// For the spike, items are rendered as solid-color horizontal strips
// (raw GL, no ImGui inside the popup) — enough to prove the popup
// primitive itself works. Item text rendering can be layered on once
// the dismiss/grab paths are validated.
int platform_popup_run(int offset_x, int offset_y, int w, int h, int num_items);

#ifdef __cplusplus
}
#endif

#endif
