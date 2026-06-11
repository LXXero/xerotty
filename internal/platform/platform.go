// Package platform is the SDL3+ImGui platform layer that will replace
// cimgui-go/backend/sdlbackend. Phase 0: opens a window and renders the
// ImGui demo. See docs/SDL3_PLAN.md for the phased roadmap.
package platform

// cimgui-go/imgui is imported (below) for both its Go API (calls into
// cimgui.a's igNewFrame, igRender, igShowDemoWindow, …) AND its cgo
// LDFLAGS, which pull the prebuilt cimgui.a into the link line.
// cimgui.a supplies the C++ ImGui::* symbols our vendored impl_sdl3
// references. Version match is guaranteed because we vendor headers
// from the same cimgui-go module copy that built cimgui.a (ImGui
// 1.92.4 WIP / 19231). cimgui-go's Go calls and our C++ shim both
// operate on the same global GImGui pointer in cimgui.a.

/*
#cgo pkg-config: sdl3
// IMGUI_USE_WCHAR32 / IMGUI_DISABLE_OBSOLETE_FUNCTIONS / IMGUI_IMPL_API
// must match the flags cimgui-go's lib/CMakeLists.txt passes when it
// builds cimgui.a, or ImGuiIO's layout differs between our TU and
// cimgui.a and IMGUI_CHECKVERSION asserts at runtime. cgo's flag
// validator rejects values that contain spaces (`extern "C"`), so we
// set all three in imconfig.h instead and don't pass anything via -D.
#cgo CPPFLAGS: -I${SRCDIR}/imgui -I${SRCDIR}/imgui_backends
#cgo CXXFLAGS: -std=c++17 -fno-exceptions -fno-rtti -Wno-unused-function
// Per-OS link line. On Linux we pull in libGL + libwayland-client + libX11
// for the real wlgrab/xgrab implementations. On macOS those translate to
// the OpenGL framework, and the wayland/X11 libs are absent (replaced by
// the _darwin.c stub TUs); Cocoa is already linked by cellsnap_darwin.go.
#cgo linux  LDFLAGS: -lGL -lstdc++ -lm -lwayland-client -lX11
// libxtmetal.a carries the ObjC++ Metal backend (xt_metal_darwin.mm
// + vendored imgui_impl_metal): Go's build system ignores .mm files,
// so the Makefile pre-compiles them on darwin (make build/app).
// Plain `go build` on a mac without running make first will fail to
// link — build via ./build.sh or make, per CLAUDE.md.
#cgo darwin LDFLAGS: ${SRCDIR}/libxtmetal.a -lstdc++ -lm -framework OpenGL -framework Metal -framework MetalKit -framework QuartzCore

#include <stdlib.h>
#include <SDL3/SDL.h>
#include "sdl3.h"
#include "wlgrab.h"
*/
import "C"

import (
	"errors"
	"image"
	"unsafe"

	"github.com/AllenDang/cimgui-go/imgui"
)

// beforeRender, if non-nil, is invoked before each ImGui::NewFrame.
// Used by font reloads — those need to happen before NewFrame snapshots
// the current font, otherwise freeing the old atlas mid-frame leaves a
// dangling pointer that crashes during Render.
var beforeRender func()

// SetBeforeRenderHook registers a callback that fires once per frame,
// immediately before imgui.NewFrame. Pass nil to clear. Replaces
// cimgui-go's Backend.SetBeforeRenderHook.
func SetBeforeRenderHook(fn func()) { beforeRender = fn }

// Init creates the OS window, GL context, and ImGui context. Returns
// an error if any step fails; on success Shutdown must be called.
func Init(title string, width, height int) error {
	ctitle := C.CString(title)
	defer C.free(unsafe.Pointer(ctitle))
	if C.platform_init(ctitle, C.int(width), C.int(height)) == 0 {
		return errors.New(C.GoString(C.platform_last_error()))
	}
	return nil
}

// Frame pumps events, runs one ImGui frame (calling draw between
// NewFrame and Render — draw is where the caller does its imgui.Begin
// / imgui.Text / etc.), and swaps the buffer. Returns false when the
// user has asked to close the window.
//
// The split between C-side (begin/end) and Go-side (NewFrame/Render +
// user draw) is what proves cimgui-go's Go API sees the same ImGui
// context our C shim created — they share the global GImGui pointer
// in cimgui.a.
func Frame(draw func()) bool {
	if C.platform_begin_frame() == 0 {
		return false
	}
	if beforeRender != nil {
		beforeRender()
	}
	imgui.NewFrame()
	if draw != nil {
		draw()
	}
	imgui.Render()
	C.platform_end_frame()
	return true
}

// VideoDriver reports the active SDL video driver — "wayland", "x11",
// "cocoa", etc. Useful for the spike's Phase 0 acceptance check.
func VideoDriver() string {
	return C.GoString(C.platform_video_driver())
}

// Shutdown tears down ImGui, the GL context, the window, and SDL.
func Shutdown() {
	C.platform_shutdown()
}

// PostWake pushes a custom SDL event into the queue, breaking the main
// loop out of SDL_WaitEventTimeout immediately. Safe to call from any
// goroutine. Intended for the PTY reader to signal "new data — render
// the next frame now, don't wait out the frame cap window".
func PostWake() {
	C.platform_post_wake()
}

// SetIdleTimeout bounds how long the render loop parks when idle (in
// milliseconds) before forcing a frame. The app calls this each frame:
// the time until the next cursor-blink toggle when a focused cursor is
// blinking (so the blink keeps ticking), otherwise a longer safety-net.
// Pass a finite value — never 0 — so a missed wake can't freeze the UI.
func SetIdleTimeout(ms int) {
	C.platform_set_idle_timeout_ms(C.int(ms))
}

// Quit signals the main loop to exit on its next iteration. Thread-safe.
// Replaces sdlQuit() in internal/app/sdl_helpers.go.
func Quit() { C.platform_request_quit() }

// HideMainWindow hides the SDL_Window created by Init without destroying
// it. The GL context still lives on it; ImGui multi-viewport pop-outs
// become the user-visible OS windows.
func HideMainWindow() { C.platform_hide_main_window() }

// ResyncModifiers reads the OS-level modifier state and re-asserts
// it into ImGui's IO as AddKeyEvent for ImGuiMod_*. Used after a
// window-focus transition (spawn or close) where macOS drops
// modifier state mid-flight — e.g. holding Cmd through Cmd+N → Cmd+T
// leaves ImGui thinking Cmd was released, so the Cmd+T keybind
// fails to match. Idempotent; safe to call every transition.
func ResyncModifiers() { C.platform_resync_modifiers() }

// SetWindowIcon attaches an RGBA8 pixel buffer to the SDL_Window
// with the given ID. The pixels are row-major, top-to-bottom, with
// no padding (pitch = width * 4). On Linux the WM uses this for
// taskbar / Alt-Tab / window-list display. On macOS the bundle's
// .icns wins for the Dock, but the per-window NSWindow icon can
// still be set this way for completeness. No-op if windowID is
// unknown or pixels is empty.
func SetWindowIcon(windowID uintptr, pixels []byte, width, height int) {
	if len(pixels) == 0 || width <= 0 || height <= 0 {
		return
	}
	C.platform_set_window_icon(C.ulong(windowID),
		(*C.uchar)(unsafe.Pointer(&pixels[0])), C.int(width), C.int(height))
}

// RaiseWindow raises + key-focuses the SDL_Window with the given ID.
// Called after spawnWindow so the new viewport NSWindow grabs OS
// keyboard focus immediately — otherwise the first frame's keybinds
// (e.g. Cmd+T right after Cmd+N) route to the spawning window because
// macOS hasn't transitioned focus yet.
func RaiseWindow(windowID uintptr) {
	C.platform_raise_window(C.ulong(windowID))
}

// WindowHasInputFocus reports whether the SDL_Window with the given
// ID (a viewport PlatformHandle) currently holds OS-level keyboard
// focus (SDL_WINDOW_INPUT_FOCUS). This is the OS truth — unlike
// ImGui's IsWindowFocused, it stays correct when focus crosses to a
// DIFFERENT application's window, which ImGui never sees. Used to gate
// click handling so a click that actually went to another window
// doesn't leak into this one's selection / tab bar (a mac
// multi-viewport hazard: ImGui can keep reporting the wrapper hovered
// because it never receives a mouse-leave when focus jumps away).
func WindowHasInputFocus(windowID uintptr) bool {
	return C.platform_window_input_focus(C.ulong(windowID)) != 0
}

// WindowOccluded reports whether the OS says this window's pixels
// are currently unreachable (fully covered / hidden — macOS
// occlusionState via SDL_EVENT_WINDOW_OCCLUDED). Draw paths skip
// occluded windows; the EXPOSED event wakes the loop and the next
// frame repaints with current content, so nothing is ever stale
// on screen.
func WindowOccluded(windowID uintptr) bool {
	return C.platform_window_occluded(C.ulong(windowID)) != 0
}

// MarkViewportDirty tells the render loop this window's pixels must
// be re-rendered this frame. Cleared every frame; the app re-marks
// while a window has changing content, focus, glow, or open
// overlays. Unmarked windows (idle, no events) skip their GL render
// + vsync swap entirely — the difference between one streaming tab
// costing one window and costing every window.
func MarkViewportDirty(windowID uintptr) {
	C.platform_mark_viewport_dirty(C.ulong(windowID))
}

// SetDamageEnabled toggles per-window damage tracking. Disabled =
// every visible viewport renders every frame (the old behavior);
// the XEROTTY_NO_DAMAGE escape hatch and --screenshot mode use it.
func SetDamageEnabled(on bool) {
	v := C.int(0)
	if on {
		v = 1
	}
	C.platform_set_damage_enabled(v)
}

// SetWindowOpacity sets whole-window opacity (0..1) on the SDL_Window
// with the given ID (a viewport PlatformHandle). Restores the documented
// `opacity` config the SDL2→SDL3 migration dropped.
func SetWindowOpacity(windowID uintptr, opacity float32) {
	C.platform_set_window_opacity(C.ulong(windowID), C.float(opacity))
}

// WindowUsableBounds returns the usable screen rect (excludes macOS
// Dock / Linux panels) for the display containing the given
// SDL_Window. Coords are absolute desktop screen pixels. Returns
// (0,0,0,0,false) if the window doesn't exist or SDL can't query
// bounds. Callers use it to flip-up popups that would overflow the
// screen bottom when anchored below a button.
func WindowUsableBounds(windowID uintptr) (x, y, w, h int, ok bool) {
	var cx, cy, cw, ch C.int
	got := C.platform_get_window_usable_bounds(C.ulong(windowID),
		&cx, &cy, &cw, &ch)
	if got == 0 {
		return 0, 0, 0, 0, false
	}
	return int(cx), int(cy), int(cw), int(ch), true
}

// EnsureTextInput re-asserts SDL text input on the window so the
// terminal keeps receiving typed characters (SDL_EVENT_TEXT_INPUT →
// io.InputQueueCharacters). The ImGui SDL3 backend calls
// SDL_StopTextInput on a window when an InputText there deactivates,
// so any dialog pinned to the terminal's viewport (rename, connect,
// search) silently kills terminal typing on close. Called every frame
// the terminal owns keyboard input; a no-op (guarded by
// SDL_TextInputActive) when already active. windowID 0 is ignored.
func EnsureTextInput(windowID uintptr) {
	if windowID == 0 {
		return
	}
	C.platform_ensure_text_input(C.ulong(windowID))
}

// MouseFocusWindowID returns the SDL_WindowID of the window the OS
// cursor is currently over, or 0 if it's not over any of our windows.
// Reliable on every backend (X11, XWayland, native Wayland, Cocoa)
// because it goes through the OS-level pointer focus tracking.
func MouseFocusWindowID() uintptr {
	return uintptr(C.platform_mouse_focus_window_id())
}

// StartWaylandTabDrag initiates a Wayland data_device drag rooted at
// the given Window. Returns true if the drag was actually started
// (Wayland driver + data_device available + a recent input serial
// tracked). The compositor takes over: cursor + enter/leave routing
// to other surfaces during the drag, then a drop or cancel event.
// Poll WaylandDragTarget / WaylandDropFired to know when a drop's
// landed and on which Window.
func StartWaylandTabDrag(originWindowID uintptr) bool {
	w := C.SDL_GetWindowFromID(C.SDL_WindowID(originWindowID))
	if w == nil {
		return false
	}
	props := C.SDL_GetWindowProperties(w)
	key := C.CString("SDL.window.wayland.surface")
	defer C.free(unsafe.Pointer(key))
	surf := C.SDL_GetPointerProperty(props, key, nil)
	if surf == nil {
		return false
	}
	return C.wldrag_start(surf) != 0
}

// WaylandDragTarget returns the wl_surface pointer the cross-surface
// drag is currently hovering, or 0. Compare to each Window's
// surface (via the same SDL_PROP_WINDOW_WAYLAND_SURFACE_POINTER) to
// figure out which Window is the current drop target.
func WaylandDragTarget() uintptr {
	return uintptr(C.wldrag_target_surface())
}

// WaylandDropFired returns true ONCE after the compositor delivered
// a drop event, then resets. Used by the app-level tab-drop resolver
// to know "the drop is now finalizable".
func WaylandDropFired() bool {
	return C.wldrag_drop_fired() != 0
}

// WaylandDropTarget returns the wl_surface that was current when the
// most recent Wayland DND drop fired, or 0 if the drop was outside a
// surface owned by this client.
func WaylandDropTarget() uintptr {
	return uintptr(C.wldrag_drop_target_surface())
}

// WaylandDragActive reports whether a Wayland data_device drag
// session is in flight (between StartWaylandTabDrag and the
// cancel/drop completion).
func WaylandDragActive() bool {
	return C.wldrag_active() != 0
}

// WindowWLSurfacePtr returns the wl_surface for the given Window ID,
// or 0 on non-Wayland or if the window doesn't exist.
func WindowWLSurfacePtr(windowID uintptr) uintptr {
	w := C.SDL_GetWindowFromID(C.SDL_WindowID(windowID))
	if w == nil {
		return 0
	}
	props := C.SDL_GetWindowProperties(w)
	key := C.CString("SDL.window.wayland.surface")
	defer C.free(unsafe.Pointer(key))
	return uintptr(C.SDL_GetPointerProperty(props, key, nil))
}

// SetFullscreen toggles fullscreen on the SDL_Window with the given
// ID. windowID is what ImGui's SDL3 backend stores in
// ImGuiViewport.PlatformHandle (a WindowID, not a pointer — same
// convention as impl_sdl2 since 2024).
func SetFullscreen(windowID uintptr, enable bool) {
	flag := C.int(0)
	if enable {
		flag = 1
	}
	C.platform_set_fullscreen(C.ulong(windowID), flag)
}

// SetBgColor updates the framebuffer clear color used by end_frame.
// Replaces backend.Backend.SetBgColor + updateEventLoopBg.
func SetBgColor(c imgui.Vec4) {
	C.platform_set_bg_color(C.float(c.X), C.float(c.Y), C.float(c.Z), C.float(c.W))
}

// TextureManager mirrors cimgui-go/backend.TextureManager so existing
// callers (internal/glyphcache) can swap implementations with a one-
// line change. The underlying impl is GL through our cgo shim — no
// SDL2 dependency.
type TextureManager struct{}

// Textures returns a package-level TextureManager. Caller can stash it
// at startup; there's no per-instance state.
func Textures() TextureManager { return TextureManager{} }

func (TextureManager) CreateTexture(pixels unsafe.Pointer, width, height int) imgui.TextureRef {
	glID := C.platform_create_texture((*C.uchar)(pixels), C.int(width), C.int(height))
	return *imgui.NewTextureRefTextureID(imgui.TextureID(glID))
}

func (t TextureManager) CreateTextureRgba(img *image.RGBA, width, height int) imgui.TextureRef {
	if len(img.Pix) == 0 {
		return imgui.TextureRef{}
	}
	return t.CreateTexture(unsafe.Pointer(&img.Pix[0]), width, height)
}

// captureBuf pins the capture target between RequestCapture and
// CollectCapture (the C side holds a pointer into it across frames).
var captureBuf []byte

// RequestCapture arms a framebuffer capture: the NEXT rendered frame
// is read back post-render/pre-swap (post-swap contents are
// undefined, so this can't be done from outside the render path).
// Collect with CollectCapture after the next Frame() returns. Powers
// `xerotty --screenshot` for automated visual regression tests.
// windowID selects WHICH window to capture (a viewport
// PlatformHandle); 0 captures the main scaffolding window.
func RequestCapture(windowID uintptr, maxW, maxH int) {
	captureBuf = make([]byte, maxW*maxH*4)
	C.platform_request_capture(C.ulong(windowID), (*C.uchar)(unsafe.Pointer(&captureBuf[0])), C.int(len(captureBuf)))
}

// CollectCapture returns the captured RGBA image (top-down rows), or
// ok=false if no capture completed yet.
func CollectCapture() (pix []byte, w, h int, ok bool) {
	var cw, ch C.int
	if C.platform_capture_result(&cw, &ch) == 0 {
		return nil, 0, 0, false
	}
	w, h = int(cw), int(ch)
	// GL rows are bottom-up; flip to image convention.
	flipped := make([]byte, w*h*4)
	stride := w * 4
	for y := 0; y < h; y++ {
		copy(flipped[y*stride:(y+1)*stride], captureBuf[(h-1-y)*stride:(h-y)*stride])
	}
	captureBuf = nil
	return flipped, w, h, true
}

// GlyphQuad is one textured quad for DrawListAddQuads. Field order
// and sizes mirror xt_glyph_quad in glyphbatch.cpp — keep in sync.
type GlyphQuad struct {
	X0, Y0, X1, Y1 float32
	U0, V0, U1, V1 float32
	Col            uint32
	_              uint32 // padding to align Tex, matching the C struct
	Tex            uint64
}

// DrawListAddQuads appends every quad to the draw list in ONE cgo
// crossing, grouping consecutive same-texture quads into native
// PrimReserve/PrimRectUV runs. With the glyph atlas, a whole text
// frame is typically one group — the per-glyph AddImage path cost
// ~20% of process CPU in crossing overhead alone on dense grids.
func DrawListAddQuads(dl *imgui.DrawList, quads []GlyphQuad) {
	if dl == nil || len(quads) == 0 {
		return
	}
	// DrawList's first (only) field is its *C.ImDrawList — cimgui-go
	// exports it but as a package-local C type, so reinterpret.
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(dl))
	C.platform_drawlist_add_quads(ptr, unsafe.Pointer(&quads[0]), C.int(len(quads)))
}

// RenderQuadsToTexture renders a quad stream into *tex (created on
// first use; reallocated when realloc is set) through ImGui's own GL
// backend, making the result pixel-identical to drawing the quads
// directly. disp* give the logical-coordinate rect the quads occupy;
// pxW/pxH the texture size in framebuffer pixels. Returns false when
// offscreen rendering isn't available (no FBO support, no context) —
// callers must fall back to direct drawing.
func RenderQuadsToTexture(tex *uint64, realloc bool, pxW, pxH int, dispX, dispY, dispW, dispH float32, quads []GlyphQuad) bool {
	var qp unsafe.Pointer
	if len(quads) > 0 {
		qp = unsafe.Pointer(&quads[0])
	}
	ct := C.ulonglong(*tex)
	r := C.int(0)
	if realloc {
		r = 1
	}
	ok := C.platform_render_quads_to_texture(&ct, r,
		C.int(pxW), C.int(pxH),
		C.float(dispX), C.float(dispY), C.float(dispW), C.float(dispH),
		qp, C.int(len(quads)))
	*tex = uint64(ct)
	return ok == 1
}

// DrawListBlitPremul draws a RenderQuadsToTexture result. The
// offscreen pass leaves PREMULTIPLIED color in the texture, so the
// blit runs under (ONE, ONE_MINUS_SRC_ALPHA) via draw callbacks —
// blitting through the normal SRC_ALPHA pipeline would double-darken
// every anti-aliased glyph edge.
func DrawListBlitPremul(dl *imgui.DrawList, tex uint64, x0, y0, x1, y1 float32) {
	if dl == nil || tex == 0 {
		return
	}
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(dl))
	C.platform_drawlist_blit_premul(ptr, C.ulonglong(tex), C.float(x0), C.float(y0), C.float(x1), C.float(y1))
}

// UpdateTexture overwrites a sub-rectangle of an existing texture
// (glTexSubImage2D). The glyph atlas uses it to pack freshly
// rasterized glyphs into shared pages instead of minting one GL
// texture per glyph — per-glyph textures forced a texture bind per
// glyph draw, which profiled as the dominant render cost (Mesa
// driver time) on large grids.
func (TextureManager) UpdateTexture(ref imgui.TextureRef, x, y, width, height int, pixels unsafe.Pointer) {
	C.platform_update_texture(C.ulonglong(ref.TexID()), C.int(x), C.int(y), C.int(width), C.int(height), (*C.uchar)(pixels))
}

func (TextureManager) DeleteTexture(ref imgui.TextureRef) {
	C.platform_delete_texture(C.ulonglong(ref.TexID()))
}
