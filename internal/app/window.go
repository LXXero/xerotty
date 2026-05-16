package app

import (
	"github.com/AllenDang/cimgui-go/backend"
	"github.com/AllenDang/cimgui-go/backend/sdlbackend"
	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/renderer"
	"github.com/LXXero/xerotty/internal/scrollback"
	"github.com/LXXero/xerotty/internal/tabs"
)

// Window owns the per-OS-window state. One Window = one SDL_Window =
// one terminal grid + tab bar + per-window UI overlays.
//
// Phase 1 of the multi-window refactor (see docs/MULTI_WINDOW_REFACTOR.md):
// App embeds *Window so every existing a.<field> access keeps resolving
// via Go's struct field promotion. Method receivers stay on *App for now
// — they'll migrate to *Window in phase 2. The embed will become a slice
// in phase 3 to land actual multi-window support.
//
// Cross-window shared state (theme, config, font cache, base cell metrics)
// stays on the owning App so font reloads and theme switches apply
// everywhere at once.
type Window struct {
	// Back-reference to the owning App for process-wide state (config,
	// theme, base font metrics, font-reload flags). Set in newWindow.
	app *App

	// cimgui-go backend handle. Phase 1: one backend, one Window. Phase
	// 3: each Window will reference the shared App-level backend but
	// render through its own ImGui viewport.
	backend backend.Backend[sdlbackend.SDLWindowFlags]

	// Window dimensions in logical pixels, synced each frame from
	// imgui.IO.DisplaySize.
	width  int
	height int

	// Per-window font size. Initialized from cfg.Font.Size on spawn,
	// then diverges on Cmd+= / Cmd+- so each Window can be zoomed
	// independently (iTerm2-style). The "scale" for this Window is
	// fontSize / app.baseFontSize — app.baseFontSize stays as the
	// process-wide reference at which the font advance was measured.
	fontSize float32

	// Cell geometry — width/height = ceil(app.baseCellW/H * fontScale).
	// Stored per-window so resize math doesn't have to re-derive on
	// every cell lookup. fontScale = w.fontSize / app.baseFontSize.
	cellW   float32
	cellH   float32
	tabBarH float32

	// Terminal grid content for this Window.
	tabs     *tabs.Manager
	scroll   map[int]*scrollback.State // per-tab scrollback offset
	renderer *renderer.Renderer

	// Per-Window UI state. Each Window tracks its own selection,
	// hovered link, search overlay, prefs dialog, etc. — moving any
	// of this to App would couple two open Windows together (e.g.,
	// dragging a selection in Window A would extend a selection in
	// Window B).
	fullscreen         bool
	tabBarHovered      bool
	tabSwitchReq       int
	ready              bool
	sel                selection
	pendingPaste       string
	resizeTime         float64
	resizeOverlay      bool
	resizeOverlayText  string
	lastCols           int
	lastRows           int
	hoveredLink        *linkHit
	renamingTab        bool
	renameBuffer       string
	sbDragging         bool
	searchFocusInput   bool
	searchInputFocused bool
	searchOverlayW     float32
	lastDblClickTime   float64
	lastDblClickRow    int
	lastDblClickCol    int
	prefDialog         configDialog
	lastTabBarW        float32
	lastTabBarH        float32
	skipDisplaySync    int

	// Multi-window plumbing (phase 4+). isMain is true for windows[0]
	// which uses cimgui-go's main SDL_Window directly; secondary
	// Windows render inside an ImGui top-level window that multi-
	// viewport auto-promotes to its own OS window. imguiName is the
	// unique ImGui window identifier for secondary Windows — empty for
	// the main Window. imViewport caches the *imgui.Viewport for this
	// Window each frame, set by the render loop before calling frame()
	// so w.viewport() returns the right one for coord translation and
	// draw-list selection.
	//
	// pendingClose is set by the render loop when the user hits the
	// OS-window close button on a secondary Window. The reap pass
	// after iteration removes the Window from a.windows and closes
	// its tabs.
	isMain        bool
	imguiName     string // stable ImGui ID suffix for secondaries (e.g. "win1")
	imViewport    *imgui.Viewport
	pendingClose  bool
	lastOSTitle   string // last OS-window title we set; avoids redundant syscalls
	pendingResize bool   // next frame, force SetNextWindowSize with CondAlways
}

// titleForWindow returns the human-readable title for this Window's
// OS window — delegates to the active tab's DisplayTitle() which
// prefers OSC-set titles (most shells set them from prompt), falls
// back to the PTY's foreground process name (vim, top, ssh), then
// "shell". The macOS Dock right-click menu uses this to show each
// window distinctly instead of N copies of "xerotty".
func (w *Window) titleForWindow() string {
	if w.tabs != nil {
		if tab := w.tabs.Active(); tab != nil {
			return tab.DisplayTitle()
		}
	}
	return "xerotty"
}

// newWindow returns a freshly-initialized Window with the minimum
// defaults needed before App.Run populates the rest (backend, tabs,
// renderer, cell metrics — all set during the boot sequence). The
// first Window created (via App.New) is marked isMain so it owns
// cimgui-go's primary SDL_Window; subsequent Windows opened via
// new_window get a unique imguiName so multi-viewport pop-out
// creates their OS window.
func newWindow(app *App) *Window {
	w := &Window{
		app:          app,
		scroll:       make(map[int]*scrollback.State),
		tabBarH:      0, // updated each frame from imgui.FrameHeight() when >1 tab
		tabSwitchReq: -1,
	}
	if len(app.windows) == 0 {
		w.isMain = true
	} else {
		// Phase 5 will set imguiName + initial pos/size for secondary
		// Windows when new_window appends them.
	}
	return w
}

// imguiSuffix returns an ImGui-window-ID suffix that makes any
// imgui.Begin / imgui.BeginTabBar etc. inside this Window's frame()
// unique per Window. Without this, the tab bar and search overlay
// share the same `##tabbar` / `##search` IDs across all Windows and
// ImGui treats them as the same window — secondary Windows would
// see no chrome at all. Main Window uses no suffix to keep the ID
// stable for layout persistence in imgui.ini.
func (w *Window) imguiSuffix() string {
	if w.isMain {
		return ""
	}
	return w.imguiName
}

// viewport returns the ImGui viewport this Window renders into. The
// render loop caches the correct viewport on the Window each frame
// before calling frame(); we fall back to the main viewport for
// safety when nothing has set imViewport yet (e.g. first frame
// before the render loop has run).
//
// Callers use it for (a) coordinate translation between desktop-
// absolute space (where MousePos and ImGui draw lists live under
// multi-viewport) and window-local space, and (b) picking the right
// foreground / background draw list to render into.
func (w *Window) viewport() *imgui.Viewport {
	if w.imViewport != nil {
		return w.imViewport
	}
	return imgui.MainViewport()
}

// bgDrawList returns the background draw list for this Window's
// viewport — what terminal cells, cursor, scrollbar, and link
// decorations render into. Z-order: behind all ImGui windows.
func (w *Window) bgDrawList() *imgui.DrawList {
	return imgui.BackgroundDrawListV(w.viewport())
}

// fgDrawList returns the foreground draw list for this Window's
// viewport — what the resize overlay renders into. Z-order: above
// all ImGui windows, so the overlay sits on top of any popped-out
// prefs / search overlays.
func (w *Window) fgDrawList() *imgui.DrawList {
	return imgui.ForegroundDrawListViewportPtrV(w.viewport())
}
