//go:build darwin

package app

/*
#cgo pkg-config: sdl2
#cgo CFLAGS: -DGL_SILENCE_DEPRECATION
#cgo LDFLAGS: -framework OpenGL

#include <SDL2/SDL.h>
#include <OpenGL/gl.h>

// cimgui-go's vendored cimgui.a archive ships these as plain C symbols
// (verified with `nm`). They're the same functions cimgui-go's
// igSDLRunLoop calls every iteration; we replicate the loop body inside
// an SDL event watch so a full frame still renders even when AppKit's
// live-resize tracking mode is starving the main loop.
extern void ImGui_ImplSDL2_NewFrame(void);
extern void ImGui_ImplOpenGL3_NewFrame(void);
extern void ImGui_ImplOpenGL3_RenderDrawData(void* draw_data);
extern void igNewFrame(void);
extern void igRender(void);
extern void* igGetDrawData(void);
extern void igUpdatePlatformWindows(void);
extern void igRenderPlatformWindowsDefault(void* platform_render_arg, void* renderer_render_arg);

// //export'd from Go below.
extern void xerottyLiveResizeFrame(void);
extern void xerottyLiveResizeBeforeRender(void);

static SDL_Window *gWatchWindow = NULL;
static float gWatchBg[4] = {0, 0, 0, 1};
// Re-entrancy guard: a render we drive from the watch can itself queue
// SDL events (via SDL_GL_SwapWindow on some drivers, or via ImGui's
// platform IO updates) that fire the watch again. Bail out if we're
// already in the middle of one.
static int gWatchInRender = 0;
// True while cimgui-go's main loop is between NewFrame and Render —
// i.e. user code (our frame()) might be running and could call into
// SDL APIs (SetWindowSize) that synchronously fire the size-changed
// watch. If we drove a render from the watch in that window we'd call
// NewFrame twice without an intervening Render and trip ImGui's
// assertion. Bracket frame() in Go to set/clear this flag.
static int gWatchMainFrameActive = 0;

static int xerottySizeChangedWatch(void* ud, SDL_Event* event) {
	(void)ud;
	if (gWatchWindow == NULL) return 0;
	if (event->type != SDL_WINDOWEVENT) return 0;
	if (event->window.event != SDL_WINDOWEVENT_SIZE_CHANGED) return 0;
	if (event->window.windowID != SDL_GetWindowID(gWatchWindow)) return 0;
	if (gWatchInRender) return 0;
	if (gWatchMainFrameActive) return 0;
	gWatchInRender = 1;

	// Same body as cimgui-go's igSDLRunLoop iteration, minus
	// SDL_PollEvent (the main loop already drives that) and minus the
	// multi-viewport platform-windows pass (popped-out ImGui windows
	// catch up after the resize ends).
	xerottyLiveResizeBeforeRender();

	ImGui_ImplOpenGL3_NewFrame();
	ImGui_ImplSDL2_NewFrame();
	igNewFrame();

	int w, h;
	SDL_GL_GetDrawableSize(gWatchWindow, &w, &h);
	glViewport(0, 0, w, h);
	glClearColor(gWatchBg[0], gWatchBg[1], gWatchBg[2], gWatchBg[3]);
	glClear(GL_COLOR_BUFFER_BIT);

	xerottyLiveResizeFrame();

	igRender();
	ImGui_ImplOpenGL3_RenderDrawData(igGetDrawData());

	// Multi-viewport bookkeeping: when ConfigFlagsViewportsEnable is on
	// (which it is — popped-out prefs), ImGui's NewFrame on the NEXT
	// frame asserts that UpdatePlatformWindows was called between the
	// previous Render and now. Skipping it here would assert when
	// either the main loop or another watch invocation hits NewFrame.
	// Save/restore the GL context around the call because rendering
	// platform windows can switch contexts.
	SDL_Window *backup_window = SDL_GL_GetCurrentWindow();
	SDL_GLContext backup_context = SDL_GL_GetCurrentContext();
	igUpdatePlatformWindows();
	igRenderPlatformWindowsDefault(NULL, NULL);
	SDL_GL_MakeCurrent(backup_window, backup_context);

	SDL_GL_SwapWindow(gWatchWindow);

	gWatchInRender = 0;
	return 0;
}

static void xerottyInstallLiveResizeWatch(float r, float g, float b) {
	gWatchWindow = SDL_GL_GetCurrentWindow();
	gWatchBg[0] = r; gWatchBg[1] = g; gWatchBg[2] = b; gWatchBg[3] = 1.0f;
	SDL_AddEventWatch(xerottySizeChangedWatch, NULL);
}

static void xerottyUpdateLiveResizeBg(float r, float g, float b) {
	gWatchBg[0] = r; gWatchBg[1] = g; gWatchBg[2] = b;
}

static void xerottyLiveResizeSetMainFrame(int active) {
	gWatchMainFrameActive = active;
}

static int xerottyLiveResizeInWatch(void) {
	return gWatchInRender;
}
*/
import "C"

var (
	liveResizeFrameFn        func()
	liveResizeBeforeRenderFn func()
)

//export xerottyLiveResizeFrame
func xerottyLiveResizeFrame() {
	if liveResizeFrameFn != nil {
		liveResizeFrameFn()
	}
}

//export xerottyLiveResizeBeforeRender
func xerottyLiveResizeBeforeRender() {
	if liveResizeBeforeRenderFn != nil {
		liveResizeBeforeRenderFn()
	}
}

// installLiveResizeWatch hooks an SDL event watch on the current GL
// window so SDL_WINDOWEVENT_SIZE_CHANGED triggers a full frame even
// while AppKit's live-resize tracking mode is holding the main loop.
// Must be called after the window+GL context exist (i.e. after
// CreateWindow) but before Run() takes over.
func installLiveResizeWatch(bgR, bgG, bgB float32, frame, beforeRender func()) {
	liveResizeFrameFn = frame
	liveResizeBeforeRenderFn = beforeRender
	C.xerottyInstallLiveResizeWatch(C.float(bgR), C.float(bgG), C.float(bgB))
}

// updateLiveResizeBg updates the clear color the watch uses. Called
// when the user changes themes so mid-resize clears match.
func updateLiveResizeBg(bgR, bgG, bgB float32) {
	C.xerottyUpdateLiveResizeBg(C.float(bgR), C.float(bgG), C.float(bgB))
}

// liveResizeMainFrameBegin tells the watch the main loop is inside a
// frame body now (between NewFrame and Render). The watch must not
// drive its own NewFrame during this window or ImGui asserts.
func liveResizeMainFrameBegin() { C.xerottyLiveResizeSetMainFrame(1) }

// liveResizeMainFrameEnd marks the end of the main-loop frame — watch
// is free to render again.
func liveResizeMainFrameEnd() { C.xerottyLiveResizeSetMainFrame(0) }

// inLiveResizeWatch reports whether the current frame is being driven
// from the live-resize event watch (i.e. AppKit is in tracking mode
// holding the main loop). Used to suppress mouse-event synthesis during
// resize so we don't turn the resize-handle drag into a fake terminal
// click+selection.
func inLiveResizeWatch() bool { return C.xerottyLiveResizeInWatch() != 0 }
