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
