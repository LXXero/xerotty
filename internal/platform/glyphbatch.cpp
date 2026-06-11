// Batched glyph submission + cell-layer offscreen compositing.
//
// platform_drawlist_add_quads: the renderer's text pass used to make
// one cgo call per glyph (ImDrawList::AddImage via cimgui-go) — ~15k
// Go→C crossings per frame on a dense grid (~20% of process CPU in
// crossing overhead). This takes the WHOLE frame's quads in one call
// and feeds ImGui's prim API natively.
//
// platform_render_quads_to_texture: renders that same quad stream
// into an offscreen texture through ImGui's own OpenGL3 backend (so
// the result is pixel-identical to direct drawing by construction).
// The renderer uses it to cache the whole cell layer as ONE texture:
// animation ticks (lava lamp) then composite a single quad per
// window instead of re-stamping ~20k quads into vertex buffers — the
// difference between every glow tick re-painting every window's full
// text grid and it costing a blit.
//
// platform_drawlist_blit_premul: draws that cached texture. Content
// rendered onto a transparent-black FBO with ImGui's standard
// SRC_ALPHA blending comes out PREMULTIPLIED (color already ×alpha),
// so blitting it back through SRC_ALPHA blending would double-darken
// the soft edges of every glyph. The blit brackets its AddImage in
// draw-callbacks: switch to (ONE, ONE_MINUS_SRC_ALPHA), then
// ImDrawCallback_ResetRenderState to hand the backend its state back.

#include "imgui.h"
#include "imgui_internal.h"
#include "imgui_impl_opengl3.h"

#include <SDL3/SDL.h>
#include <SDL3/SDL_opengl.h>

extern "C" int platform_use_gpu(void);

extern "C" {

// Mirrors platform.GlyphQuad — keep field order/size in sync.
typedef struct {
    float x0, y0, x1, y1;
    float u0, v0, u1, v1;
    unsigned int col;
    unsigned int _pad;
    unsigned long long tex;
} xt_glyph_quad;

static void xt_append_quads(ImDrawList* dl, const xt_glyph_quad* quads, int n) {
    int i = 0;
    while (i < n) {
        unsigned long long tex = quads[i].tex;
        int j = i;
        while (j < n && quads[j].tex == tex) j++;
        dl->PushTexture(ImTextureRef((ImTextureID)tex));
        dl->PrimReserve((j - i) * 6, (j - i) * 4);
        for (int k = i; k < j; k++) {
            const xt_glyph_quad& q = quads[k];
            dl->PrimRectUV(ImVec2(q.x0, q.y0), ImVec2(q.x1, q.y1),
                           ImVec2(q.u0, q.v0), ImVec2(q.u1, q.v1), q.col);
        }
        dl->PopTexture();
        i = j;
    }
}

void platform_drawlist_add_quads(void* dl_ptr, const void* quads_ptr, int n) {
    xt_append_quads((ImDrawList*)dl_ptr, (const xt_glyph_quad*)quads_ptr, n);
}

// --- Offscreen FBO machinery -------------------------------------
// Framebuffer entry points are GL 3.0; mac ships them core but the
// portable way to reach them from SDL-managed contexts is
// SDL_GL_GetProcAddress. Loaded once, lazily.
typedef void   (APIENTRY *xt_GenFramebuffers)(GLsizei, GLuint*);
typedef void   (APIENTRY *xt_BindFramebuffer)(GLenum, GLuint);
typedef void   (APIENTRY *xt_FramebufferTexture2D)(GLenum, GLenum, GLenum, GLuint, GLint);
typedef GLenum (APIENTRY *xt_CheckFramebufferStatus)(GLenum);
typedef void   (APIENTRY *xt_BlendFuncSeparate)(GLenum, GLenum, GLenum, GLenum);

#ifndef GL_FRAMEBUFFER
#define GL_FRAMEBUFFER          0x8D40
#define GL_COLOR_ATTACHMENT0    0x8CE0
#define GL_FRAMEBUFFER_COMPLETE 0x8CD5
#define GL_FRAMEBUFFER_BINDING  0x8CA6
#endif

static xt_GenFramebuffers         p_GenFramebuffers = nullptr;
static xt_BindFramebuffer         p_BindFramebuffer = nullptr;
static xt_FramebufferTexture2D    p_FramebufferTexture2D = nullptr;
static xt_CheckFramebufferStatus  p_CheckFramebufferStatus = nullptr;
static xt_BlendFuncSeparate       p_BlendFuncSeparate = nullptr;
static int g_fbo_procs_state = 0; // 0=unloaded 1=ok -1=unavailable

static int xt_load_fbo_procs(void) {
    if (g_fbo_procs_state != 0) return g_fbo_procs_state;
    p_GenFramebuffers        = (xt_GenFramebuffers)SDL_GL_GetProcAddress("glGenFramebuffers");
    p_BindFramebuffer        = (xt_BindFramebuffer)SDL_GL_GetProcAddress("glBindFramebuffer");
    p_FramebufferTexture2D   = (xt_FramebufferTexture2D)SDL_GL_GetProcAddress("glFramebufferTexture2D");
    p_CheckFramebufferStatus = (xt_CheckFramebufferStatus)SDL_GL_GetProcAddress("glCheckFramebufferStatus");
    p_BlendFuncSeparate      = (xt_BlendFuncSeparate)SDL_GL_GetProcAddress("glBlendFuncSeparate");
    g_fbo_procs_state = (p_GenFramebuffers && p_BindFramebuffer &&
                         p_FramebufferTexture2D && p_CheckFramebufferStatus &&
                         p_BlendFuncSeparate) ? 1 : -1;
    return g_fbo_procs_state;
}

static GLuint g_offscreen_fbo = 0;

// Renders n quads into *tex_inout (created on first use, reallocated
// when realloc!=0). disp_* define the logical-coordinate rect the
// quads live in — the same mapping ImGui's projection would give them
// on screen, so positions/AA land on identical pixels. Returns 1 on
// success; 0 means the caller must fall back to direct drawing.
int platform_render_quads_to_texture(
    unsigned long long* tex_inout, int realloc_tex,
    int px_w, int px_h,
    float disp_x, float disp_y, float disp_w, float disp_h,
    const void* quads_ptr, int n
) {
    if (px_w <= 0 || px_h <= 0 || disp_w <= 0 || disp_h <= 0) return 0;
    // SDL_GPU slice 1: the offscreen compositor is still GL-only —
    // returning 0 falls back to direct quad stamping, which the GPU
    // backend presents cheaply. The SDL_GPU port (explicit render
    // pass + custom premul pipeline, both first-class in the API) is
    // the designated slice 2.
    if (platform_use_gpu()) return 0;
    if (xt_load_fbo_procs() != 1) return 0;
    if (!ImGui::GetCurrentContext()) return 0;

    GLint prev_fbo = 0, prev_tex = 0;
    GLint prev_viewport[4];
    glGetIntegerv(GL_FRAMEBUFFER_BINDING, &prev_fbo);
    glGetIntegerv(GL_TEXTURE_BINDING_2D, &prev_tex);
    glGetIntegerv(GL_VIEWPORT, prev_viewport);
    GLboolean prev_scissor = glIsEnabled(GL_SCISSOR_TEST);

    GLuint tex = (GLuint)*tex_inout;
    if (tex == 0) {
        glGenTextures(1, &tex);
        glBindTexture(GL_TEXTURE_2D, tex);
        // NEAREST: the blit is always 1:1 at framebuffer scale; exact
        // texel fetch keeps it bit-identical to direct rendering.
        glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_NEAREST);
        glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_NEAREST);
        glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_WRAP_S, GL_CLAMP_TO_EDGE);
        glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_WRAP_T, GL_CLAMP_TO_EDGE);
        realloc_tex = 1;
        *tex_inout = (unsigned long long)tex;
    } else {
        glBindTexture(GL_TEXTURE_2D, tex);
    }
    if (realloc_tex) {
        glTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA, px_w, px_h, 0,
                     GL_RGBA, GL_UNSIGNED_BYTE, nullptr);
    }

    if (g_offscreen_fbo == 0) p_GenFramebuffers(1, &g_offscreen_fbo);
    p_BindFramebuffer(GL_FRAMEBUFFER, g_offscreen_fbo);
    p_FramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, tex, 0);
    if (p_CheckFramebufferStatus(GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE) {
        p_BindFramebuffer(GL_FRAMEBUFFER, (GLuint)prev_fbo);
        glBindTexture(GL_TEXTURE_2D, (GLuint)prev_tex);
        return 0;
    }

    glViewport(0, 0, px_w, px_h);
    if (prev_scissor) glDisable(GL_SCISSOR_TEST);
    glClearColor(0.f, 0.f, 0.f, 0.f);
    glClear(GL_COLOR_BUFFER_BIT);

    // Stack ImDrawList + ImDrawData rendered through the real backend
    // — same shaders, same projection math, same AA as on-screen.
    ImDrawList dl(ImGui::GetDrawListSharedData());
    dl._ResetForNewFrame();
    dl.PushClipRect(ImVec2(disp_x, disp_y),
                    ImVec2(disp_x + disp_w, disp_y + disp_h), false);
    xt_append_quads(&dl, (const xt_glyph_quad*)quads_ptr, n);
    dl.PopClipRect();

    ImDrawData dd;
    ImDrawList* lists[1] = { &dl };
    dd.Valid = true;
    dd.CmdLists.Data = lists;
    dd.CmdLists.Size = 1;
    dd.CmdLists.Capacity = 1;
    dd.CmdListsCount = 1;
    dd.TotalVtxCount = dl.VtxBuffer.Size;
    dd.TotalIdxCount = dl.IdxBuffer.Size;
    dd.DisplayPos = ImVec2(disp_x, disp_y);
    dd.DisplaySize = ImVec2(disp_w, disp_h);
    dd.FramebufferScale = ImVec2((float)px_w / disp_w, (float)px_h / disp_h);
    ImGui_ImplOpenGL3_RenderDrawData(&dd);
    // dd.CmdLists wraps a stack array — detach before ImVector's
    // destructor tries to free it.
    dd.CmdLists.Data = nullptr;
    dd.CmdLists.Size = 0;
    dd.CmdLists.Capacity = 0;

    p_BindFramebuffer(GL_FRAMEBUFFER, (GLuint)prev_fbo);
    glBindTexture(GL_TEXTURE_2D, (GLuint)prev_tex);
    glViewport(prev_viewport[0], prev_viewport[1], prev_viewport[2], prev_viewport[3]);
    if (prev_scissor) glEnable(GL_SCISSOR_TEST);
    return 1;
}

static void xt_premul_blend_cb(const ImDrawList*, const ImDrawCmd*) {
    if (p_BlendFuncSeparate)
        p_BlendFuncSeparate(GL_ONE, GL_ONE_MINUS_SRC_ALPHA,
                            GL_ONE, GL_ONE_MINUS_SRC_ALPHA);
}

void platform_drawlist_blit_premul(void* dl_ptr, unsigned long long tex,
                                   float x0, float y0, float x1, float y1) {
    ImDrawList* dl = (ImDrawList*)dl_ptr;
    dl->AddCallback(xt_premul_blend_cb, nullptr);
    // GL renders the FBO bottom-up relative to screen coords: the
    // visual top of the content sits at v=1. Flip V at the blit.
    dl->AddImage(ImTextureRef((ImTextureID)tex),
                 ImVec2(x0, y0), ImVec2(x1, y1),
                 ImVec2(0, 1), ImVec2(1, 0), 0xFFFFFFFF);
    dl->AddCallback(ImDrawCallback_ResetRenderState, nullptr);
}

} // extern "C"
