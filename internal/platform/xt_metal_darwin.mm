// See xt_metal.h. MRC (no ARC) to match the vendored
// imgui_impl_metal_darwin.mm and the rest of this package's ObjC.
#import <Metal/Metal.h>
#import <QuartzCore/CAMetalLayer.h>
#import <Cocoa/Cocoa.h>

#include <SDL3/SDL.h>
#include "imgui.h"
#include "imgui_impl_metal.h"
#include "xt_metal.h"

static id<MTLDevice>             g_mtl_dev;
static id<MTLCommandQueue>       g_mtl_queue;
static CAMetalLayer*             g_mtl_main_layer;
static MTLRenderPassDescriptor*  g_mtl_frame_rpd;
static id<MTLTexture>            g_mtl_fmt_tex;

extern "C" int xt_metal_init(void* sdl_window) {
    g_mtl_dev = MTLCreateSystemDefaultDevice(); // Create rule: +1
    if (!g_mtl_dev) return 0;
    g_mtl_queue = [g_mtl_dev newCommandQueue];  // new*: +1
    if (!g_mtl_queue) return 0;
    if (!ImGui_ImplMetal_Init(g_mtl_dev)) return 0;

    // Attach a layer to the main window's view so the (briefly
    // visible) startup frames can render before HideMainWindow.
    SDL_Window* w = (SDL_Window*)sdl_window;
    NSWindow* nsw = (NSWindow*)SDL_GetPointerProperty(
        SDL_GetWindowProperties(w), SDL_PROP_WINDOW_COCOA_WINDOW_POINTER, NULL);
    if (nsw) {
        g_mtl_main_layer = [[CAMetalLayer layer] retain];
        g_mtl_main_layer.device = g_mtl_dev;
        g_mtl_main_layer.pixelFormat = MTLPixelFormatBGRA8Unorm;
        nsw.contentView.wantsLayer = YES;
        nsw.contentView.layer = g_mtl_main_layer;
    }

    // ImGui_ImplMetal_NewFrame derives its pipeline's pixel format /
    // sample count from the render pass descriptor's color texture —
    // a tiny persistent BGRA8 render target carries the format even
    // on frames where the (hidden) main window never gets a drawable.
    MTLTextureDescriptor* td = [MTLTextureDescriptor
        texture2DDescriptorWithPixelFormat:MTLPixelFormatBGRA8Unorm
                                     width:4 height:4 mipmapped:NO];
    td.usage = MTLTextureUsageRenderTarget;
    g_mtl_fmt_tex = [g_mtl_dev newTextureWithDescriptor:td];
    g_mtl_frame_rpd = [MTLRenderPassDescriptor new];
    g_mtl_frame_rpd.colorAttachments[0].texture = g_mtl_fmt_tex;
    g_mtl_frame_rpd.colorAttachments[0].loadAction = MTLLoadActionClear;
    g_mtl_frame_rpd.colorAttachments[0].storeAction = MTLStoreActionStore;
    return 1;
}

extern "C" void xt_metal_new_frame(void) {
    ImGui_ImplMetal_NewFrame(g_mtl_frame_rpd);
}

extern "C" void* xt_metal_pool_push(void) {
    return [[NSAutoreleasePool alloc] init];
}

extern "C" void xt_metal_pool_pop(void* pool) {
    [(NSAutoreleasePool*)pool drain];
}

extern "C" void xt_metal_render_main(void* sdl_window, float r, float g, float b, float a) {
    if (!g_mtl_main_layer) return;
    SDL_Window* w = (SDL_Window*)sdl_window;
    int pw = 0, ph = 0;
    SDL_GetWindowSizeInPixels(w, &pw, &ph);
    if (pw <= 0 || ph <= 0) return;
    g_mtl_main_layer.drawableSize = CGSizeMake(pw, ph);
    id<CAMetalDrawable> drawable = [g_mtl_main_layer nextDrawable];
    if (!drawable) return;
    MTLRenderPassDescriptor* rpd = [MTLRenderPassDescriptor renderPassDescriptor];
    rpd.colorAttachments[0].texture = drawable.texture;
    rpd.colorAttachments[0].loadAction = MTLLoadActionClear;
    rpd.colorAttachments[0].storeAction = MTLStoreActionStore;
    rpd.colorAttachments[0].clearColor = MTLClearColorMake(r, g, b, a);
    id<MTLCommandBuffer> cb = [g_mtl_queue commandBuffer];
    id<MTLRenderCommandEncoder> enc = [cb renderCommandEncoderWithDescriptor:rpd];
    ImGui_ImplMetal_RenderDrawData(ImGui::GetDrawData(), cb, enc);
    [enc endEncoding];
    [cb presentDrawable:drawable];
    [cb commit];
}

extern "C" unsigned long long xt_metal_create_texture(const unsigned char* pixels, int w, int h) {
    if (w <= 0 || h <= 0) return 0;
    MTLTextureDescriptor* td = [MTLTextureDescriptor
        texture2DDescriptorWithPixelFormat:MTLPixelFormatRGBA8Unorm
                                     width:(NSUInteger)w height:(NSUInteger)h mipmapped:NO];
    td.usage = MTLTextureUsageShaderRead;
    td.storageMode = MTLStorageModeShared;
    id<MTLTexture> t = [g_mtl_dev newTextureWithDescriptor:td]; // +1, carried in the handle
    if (!t) return 0;
    if (pixels)
        [t replaceRegion:MTLRegionMake2D(0, 0, w, h) mipmapLevel:0
               withBytes:pixels bytesPerRow:(NSUInteger)w * 4];
    return (unsigned long long)(uintptr_t)t;
}

extern "C" void xt_metal_update_texture(unsigned long long tex, int x, int y, int w, int h, const unsigned char* pixels) {
    if (!tex || !pixels || w <= 0 || h <= 0) return;
    id<MTLTexture> t = (id<MTLTexture>)(uintptr_t)tex;
    [t replaceRegion:MTLRegionMake2D(x, y, w, h) mipmapLevel:0
           withBytes:pixels bytesPerRow:(NSUInteger)w * 4];
}

extern "C" void xt_metal_delete_texture(unsigned long long tex) {
    if (!tex) return;
    id<MTLTexture> t = (id<MTLTexture>)(uintptr_t)tex;
    [t release];
}

extern "C" void xt_metal_shutdown(void) {
    ImGui_ImplMetal_Shutdown();
}
