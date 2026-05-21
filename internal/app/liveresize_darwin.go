//go:build darwin

package app

// macOS live-resize smooth-render watch.
//
// AppKit handles NSWindow live-resize in NSEventTrackingRunLoopMode.
// While that tracking loop is active, the normal SDL_WaitEventTimeout
// frame pump does not advance, so the OS stretches the last framebuffer
// until the resize gesture ends. The event watch below drives one normal
// ImGui frame for real AppKit live-resize size events only.
//
// The important guard is platform_cocoa_window_in_live_resize(). Ordinary
// SDL_EVENT_WINDOW_RESIZED events also fire while ImGui creates or updates
// multi-viewport windows; rendering from the watch for those events calls
// NewFrame outside the normal loop and can consume queued input.

/*
#cgo pkg-config: sdl3
#cgo CFLAGS: -DGL_SILENCE_DEPRECATION -I${SRCDIR}/../platform
#cgo LDFLAGS: -framework OpenGL

#include <stdbool.h>
#include <SDL3/SDL.h>
#include <OpenGL/gl.h>
#include "cocoa_focus.h"

extern void ImGui_ImplSDL3_NewFrame(void);
extern bool ImGui_ImplSDL3_ProcessEvent(const SDL_Event* event);
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
static SDL_GLContext gWatchContext = NULL;
static float gWatchBg[4] = {0, 0, 0, 1};
static int gWatchInRender = 0;
static int gWatchMainFrameActive = 0;
static int gWatchInstalled = 0;

static bool xerottyLiveResizeEvent(const SDL_Event* event) {
  return event->type == SDL_EVENT_WINDOW_RESIZED ||
         event->type == SDL_EVENT_WINDOW_PIXEL_SIZE_CHANGED;
}

static bool xerottySizeChangedWatch(void* ud, SDL_Event* event) {
  (void)ud;
  if (gWatchWindow == NULL || gWatchContext == NULL) return false;
  if (!xerottyLiveResizeEvent(event)) return false;
  if (event->window.windowID == 0) return false;
  if (!platform_cocoa_window_in_live_resize((unsigned long)event->window.windowID)) return false;
  if (gWatchInRender) return false;
  if (gWatchMainFrameActive) return false;

  gWatchInRender = 1;

  SDL_Window *backup_window = SDL_GL_GetCurrentWindow();
  SDL_GLContext backup_context = SDL_GL_GetCurrentContext();
  SDL_GL_MakeCurrent(gWatchWindow, gWatchContext);

  xerottyLiveResizeBeforeRender();

  ImGui_ImplOpenGL3_NewFrame();
  if (event->type == SDL_EVENT_WINDOW_RESIZED) {
    ImGui_ImplSDL3_ProcessEvent(event);
  }
  ImGui_ImplSDL3_NewFrame();
  igNewFrame();

  int w = 0, h = 0;
  SDL_GetWindowSizeInPixels(gWatchWindow, &w, &h);
  glViewport(0, 0, w, h);
  glClearColor(gWatchBg[0], gWatchBg[1], gWatchBg[2], gWatchBg[3]);
  glClear(GL_COLOR_BUFFER_BIT);

  xerottyLiveResizeFrame();

  igRender();
  ImGui_ImplOpenGL3_RenderDrawData(igGetDrawData());

  igUpdatePlatformWindows();
  igRenderPlatformWindowsDefault(NULL, NULL);

  SDL_GL_MakeCurrent(gWatchWindow, gWatchContext);
  SDL_GL_SwapWindow(gWatchWindow);

  if (backup_window != NULL && backup_context != NULL) {
    SDL_GL_MakeCurrent(backup_window, backup_context);
  }
  gWatchInRender = 0;
  return false;
}

static void xerottyInstallLiveResizeWatch(float r, float g, float b) {
  gWatchWindow = SDL_GL_GetCurrentWindow();
  gWatchContext = SDL_GL_GetCurrentContext();
  gWatchBg[0] = r; gWatchBg[1] = g; gWatchBg[2] = b; gWatchBg[3] = 1.0f;
  if (!gWatchInstalled) {
    SDL_AddEventWatch(xerottySizeChangedWatch, NULL);
    gWatchInstalled = 1;
  }
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

func installLiveResizeWatch(bgR, bgG, bgB float32, frame, beforeRender func()) {
	liveResizeFrameFn = frame
	liveResizeBeforeRenderFn = beforeRender
	C.xerottyInstallLiveResizeWatch(C.float(bgR), C.float(bgG), C.float(bgB))
}

func updateLiveResizeBg(bgR, bgG, bgB float32) {
	C.xerottyUpdateLiveResizeBg(C.float(bgR), C.float(bgG), C.float(bgB))
}

func liveResizeMainFrameBegin() { C.xerottyLiveResizeSetMainFrame(1) }
func liveResizeMainFrameEnd()   { C.xerottyLiveResizeSetMainFrame(0) }
func inLiveResizeWatch() bool   { return C.xerottyLiveResizeInWatch() != 0 }
