// Metal render backend (darwin-only, opt-in via XEROTTY_METAL=1).
//
// macOS deprecated OpenGL and emulates it over Metal: every GL swap
// pays a framebuffer copy through AppleMetalOpenGLRenderer plus a
// SYNCHRONOUS SkyLight (WindowServer) commit — profiled on a real
// 14-window session as the dominant main-thread cost. Native Metal
// renders straight into CAMetalLayer drawables and presents
// asynchronously; none of that tax exists.
//
// Slice 1 scope: init, frame loop, glyph textures, multi-viewport
// rendering (imgui_impl_metal's own viewport support — which also
// natively skips occluded windows). The offscreen cell-layer
// compositor and screenshot capture intentionally report
// unavailable under Metal for now; the renderer falls back to
// direct quad stamping, which Metal presents cheaply anyway.
//
// Implementations live in xt_metal_darwin.mm (MRC, matching the
// vendored imgui_impl_metal_darwin.mm). Every call site in shared
// C++ is wrapped in #ifdef __APPLE__, so non-mac builds never
// reference these symbols.
#pragma once

#ifdef __cplusplus
extern "C" {
#endif

int   xt_metal_init(void* sdl_window);      // device+queue+ImGui_ImplMetal_Init; 1=ok
void  xt_metal_new_frame(void);             // ImGui_ImplMetal_NewFrame with a format-carrying RPD
void  xt_metal_render_main(void* sdl_window, float r, float g, float b, float a);
void  xt_metal_shutdown(void);
void* xt_metal_pool_push(void);             // NSAutoreleasePool for the render section
void  xt_metal_pool_pop(void* pool);

unsigned long long xt_metal_create_texture(const unsigned char* pixels, int w, int h);
void               xt_metal_update_texture(unsigned long long tex, int x, int y, int w, int h, const unsigned char* pixels);
void               xt_metal_delete_texture(unsigned long long tex);

#ifdef __cplusplus
}
#endif
