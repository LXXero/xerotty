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
extern void igNewFrame(void);
extern void igRender(void);

// Backend-agnostic frame plumbing exported by sdl3.cpp: the watch
// must not hardcode a renderer — under the SDL_GPU backend there is
// NO GL context, which silently disabled the old GL-only watch and
// made live resize stretch the last frame until mouse release.
extern int platform_use_gpu(void);
extern SDL_Window* platform_main_window(void);
extern void platform_backend_new_frame(void);
extern void platform_backend_present_all(void);

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
  if (gWatchWindow == NULL) return false;
  if (!platform_use_gpu() && gWatchContext == NULL) return false;
  if (!xerottyLiveResizeEvent(event)) return false;
  if (event->window.windowID == 0) return false;
  if (!platform_cocoa_window_in_live_resize((unsigned long)event->window.windowID)) return false;
  if (gWatchInRender) return false;
  if (gWatchMainFrameActive) return false;

  gWatchInRender = 1;

  SDL_Window *backup_window = NULL;
  SDL_GLContext backup_context = NULL;
  if (!platform_use_gpu()) {
    backup_window = SDL_GL_GetCurrentWindow();
    backup_context = SDL_GL_GetCurrentContext();
    SDL_GL_MakeCurrent(gWatchWindow, gWatchContext);
  }

  xerottyLiveResizeBeforeRender();

  platform_backend_new_frame();
  if (event->type == SDL_EVENT_WINDOW_RESIZED) {
    ImGui_ImplSDL3_ProcessEvent(event);
  }
  ImGui_ImplSDL3_NewFrame();
  igNewFrame();

  xerottyLiveResizeFrame();

  igRender();
  // Render + present every visible window through whichever backend
  // is live (damage tracking bypassed: a resize IS a visual change).
  platform_backend_present_all();

  if (!platform_use_gpu() && backup_window != NULL && backup_context != NULL) {
    SDL_GL_MakeCurrent(backup_window, backup_context);
  }
  gWatchInRender = 0;
  return false;
}

static void xerottyInstallLiveResizeWatch(float r, float g, float b) {
  // platform_main_window, NOT SDL_GL_GetCurrentWindow: under the GPU
  // backend there is no current GL window and the watch never
  // installed at all (the live-resize stretch bug).
  gWatchWindow = platform_main_window();
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
