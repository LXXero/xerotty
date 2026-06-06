// Batched glyph submission. The renderer's text pass used to make
// one cgo call per glyph (ImDrawList::AddImage via cimgui-go) —
// ~15k Go→C crossings per frame on a dense grid, which profiled at
// ~20% of process CPU in pure crossing overhead (reentersyscall /
// cgoIsGoPointer). This helper takes the WHOLE frame's glyph quads
// in one call and feeds ImGui's prim API natively, grouping
// consecutive quads that share a texture (with the atlas, that's
// nearly all of them) into single reserve+fill runs.

#include "imgui.h"

extern "C" {

// Mirrors platform.GlyphQuad — keep field order/size in sync.
typedef struct {
    float x0, y0, x1, y1;
    float u0, v0, u1, v1;
    unsigned int col;
    unsigned int _pad;
    unsigned long long tex;
} xt_glyph_quad;

void platform_drawlist_add_quads(void* dl_ptr, const void* quads_ptr, int n) {
    ImDrawList* dl = (ImDrawList*)dl_ptr;
    const xt_glyph_quad* quads = (const xt_glyph_quad*)quads_ptr;
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

} // extern "C"
