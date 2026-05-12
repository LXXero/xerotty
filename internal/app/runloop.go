package app

/*
#cgo pkg-config: sdl2
#cgo CFLAGS: -DGL_SILENCE_DEPRECATION
#cgo darwin LDFLAGS: -framework OpenGL
#cgo linux pkg-config: gl

#include <SDL2/SDL.h>
#ifdef __APPLE__
#include <OpenGL/gl.h>
#else
#include <GL/gl.h>
#endif
#include <stdint.h>

// cimgui.a (vendored by cimgui-go) ships these as plain C symbols — same
// approach as liveresize_darwin.go. We need them to drive a full frame
// from inside our own runloop, replacing cimgui-go's igSDLRunLoop.
extern void ImGui_ImplSDL2_NewFrame(void);
extern void ImGui_ImplOpenGL3_NewFrame(void);
extern void ImGui_ImplOpenGL3_RenderDrawData(void* draw_data);
extern int  ImGui_ImplSDL2_ProcessEvent(const SDL_Event* event);
extern void igNewFrame(void);
extern void igRender(void);
extern void* igGetDrawData(void);
extern void igUpdatePlatformWindows(void);
extern void igRenderPlatformWindowsDefault(void* a, void* b);

extern void xerottyEventLoopFrame(void);
extern void xerottyEventLoopBefore(void);
extern void xerottyEventLoopAfter(void);

static SDL_Window *gEvtWindow = NULL;
static float gEvtBg[4] = {0, 0, 0, 1};
static int gEvtDone = 0;
static int gEvtIdleTimeoutMs = 100;
static Uint32 gWakeEventType = (Uint32)-1;

// Mirrors cimgui-go's igSDLRunLoop body but replaces SDL_PollEvent (which
// returns immediately whether or not anything's pending and lets the loop
// burn CPU at SetTargetFPS speed) with SDL_WaitEventTimeout — the OS
// parks the thread in kernel sleep until an event arrives or the timeout
// fires. When nothing's happening, CPU usage is literally zero.
//
// Cursor blink and the resize-overlay fade need periodic wakeups, so
// xerottyEventLoopFrame() updates gEvtIdleTimeoutMs each frame to the
// shortest pending animation interval (or a long fallback when nothing's
// animating). PTY data arrival pushes the registered wake event from the
// reader goroutine to break the WaitEventTimeout sleep early.
static void xerottyEventLoopRun(SDL_Window *win, float r, float g, float b) {
	gEvtWindow = win;
	gEvtBg[0] = r; gEvtBg[1] = g; gEvtBg[2] = b; gEvtBg[3] = 1.0f;
	gWakeEventType = SDL_RegisterEvents(1);

	while (!gEvtDone) {
		xerottyEventLoopBefore();

		SDL_Event event;
		// Block until an event arrives or the per-frame computed
		// timeout expires. Either way, drain everything queued before
		// rendering (matches cimgui-go's drain-loop-then-render order).
		if (SDL_WaitEventTimeout(&event, gEvtIdleTimeoutMs) == 1) {
			do {
				ImGui_ImplSDL2_ProcessEvent(&event);
				if (event.type == SDL_QUIT) gEvtDone = 1;
				if (event.type == SDL_WINDOWEVENT &&
				    event.window.event == SDL_WINDOWEVENT_CLOSE &&
				    event.window.windowID == SDL_GetWindowID(win)) {
					gEvtDone = 1;
				}
			} while (SDL_PollEvent(&event) == 1);
		}

		ImGui_ImplOpenGL3_NewFrame();
		ImGui_ImplSDL2_NewFrame();
		igNewFrame();

		int w, h;
		SDL_GL_GetDrawableSize(win, &w, &h);
		glViewport(0, 0, w, h);
		glClearColor(gEvtBg[0], gEvtBg[1], gEvtBg[2], gEvtBg[3]);
		glClear(GL_COLOR_BUFFER_BIT);

		xerottyEventLoopFrame();

		igRender();
		ImGui_ImplOpenGL3_RenderDrawData(igGetDrawData());

		// Multi-viewport bookkeeping — popped-out prefs etc. need
		// UpdatePlatformWindows between Render and the next NewFrame
		// when ConfigFlagsViewportsEnable is on (which it is).
		SDL_Window *backup_window = SDL_GL_GetCurrentWindow();
		SDL_GLContext backup_context = SDL_GL_GetCurrentContext();
		igUpdatePlatformWindows();
		igRenderPlatformWindowsDefault(NULL, NULL);
		SDL_GL_MakeCurrent(backup_window, backup_context);

		SDL_GL_SwapWindow(win);

		xerottyEventLoopAfter();
	}
}

static void xerottyEventLoopSetTimeout(int ms) {
	if (ms < 1) ms = 1;
	gEvtIdleTimeoutMs = ms;
}

static void xerottyEventLoopUpdateBg(float r, float g, float b) {
	gEvtBg[0] = r; gEvtBg[1] = g; gEvtBg[2] = b;
}

static void xerottyEventLoopQuit(void) {
	gEvtDone = 1;
	// Push a wake event so the WaitEventTimeout returns immediately
	// instead of waiting for the current timeout to expire.
	if (gWakeEventType != (Uint32)-1) {
		SDL_Event ev;
		SDL_zero(ev);
		ev.type = gWakeEventType;
		SDL_PushEvent(&ev);
	}
}

// Wake breaks the WaitEventTimeout sleep by pushing a registered user
// event. Called from the PTY reader goroutine when new data arrives so
// the main loop can render immediately. SDL_PushEvent is documented as
// thread-safe.
static void xerottyEventLoopWake(void) {
	if (gWakeEventType == (Uint32)-1) return;
	SDL_Event ev;
	SDL_zero(ev);
	ev.type = gWakeEventType;
	SDL_PushEvent(&ev);
}
*/
import "C"

var (
	eventLoopFrameFn  func()
	eventLoopBeforeFn func()
	eventLoopAfterFn  func()
)

//export xerottyEventLoopFrame
func xerottyEventLoopFrame() {
	if eventLoopFrameFn != nil {
		eventLoopFrameFn()
	}
}

//export xerottyEventLoopBefore
func xerottyEventLoopBefore() {
	if eventLoopBeforeFn != nil {
		eventLoopBeforeFn()
	}
}

//export xerottyEventLoopAfter
func xerottyEventLoopAfter() {
	if eventLoopAfterFn != nil {
		eventLoopAfterFn()
	}
}

// runEventLoop drives the main render loop using SDL_WaitEventTimeout
// instead of cimgui-go's SDL_PollEvent. CPU usage drops to ~0% when
// idle. The frame callback receives the same call cadence cimgui-go
// would have used, but only when there's actually something to do
// (event arrival, PTY data via Wake, timeout for cursor blink /
// overlay fade).
func runEventLoop(bgR, bgG, bgB float32, frame, beforeRender, afterRender func()) {
	eventLoopFrameFn = frame
	eventLoopBeforeFn = beforeRender
	eventLoopAfterFn = afterRender
	C.xerottyEventLoopRun(C.SDL_GL_GetCurrentWindow(), C.float(bgR), C.float(bgG), C.float(bgB))
}

// setEventLoopIdleTimeout sets the maximum sleep duration for the next
// WaitEventTimeout call. The frame body computes this per-frame based
// on what's animating (cursor blink, fade overlay, mouse drag etc.) so
// truly-idle frames can sleep for a long time and animations stay
// smooth.
func setEventLoopIdleTimeout(ms int) {
	C.xerottyEventLoopSetTimeout(C.int(ms))
}

// updateEventLoopBg updates the GL clear color used by the runloop —
// called when the theme changes so mid-frame clears match the new bg.
func updateEventLoopBg(bgR, bgG, bgB float32) {
	C.xerottyEventLoopUpdateBg(C.float(bgR), C.float(bgG), C.float(bgB))
}

// quitEventLoop signals the runloop to exit after the current frame.
func quitEventLoop() {
	C.xerottyEventLoopQuit()
}

// wakeEventLoop pushes a wake event onto the SDL queue, breaking the
// WaitEventTimeout sleep early. Used by the PTY reader goroutine when
// new data arrives. Thread-safe.
func wakeEventLoop() {
	C.xerottyEventLoopWake()
}
