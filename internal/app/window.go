package app

import (
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

	// (Old: per-Window cimgui-go backend handle. Removed during the
	// SDL3 platform migration — the platform layer owns lifecycle
	// process-wide; per-Window state is just OS-window metadata
	// retrieved via imgui.Viewport when needed.)

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

	// Context menu state. ImGui's BeginPopup auto-closes the popup
	// whenever the OS window loses focus / mouse crosses the parent
	// viewport edge — fine for typical single-window apps, but we want
	// the menu to stay put once the user has right-clicked. So we
	// manage open/close manually here and render via BeginV (not
	// BeginPopup), which gives us total control over the lifecycle.
	// tabDragIdx is the slice index of the tab currently being
	// drag-reordered by the user, or -1 if no drag is in progress.
	// Updated as the user drags past tab boundaries; the tab bar
	// renderer swaps slots live so the user sees the reorder happen
	// as they drag. Reset to -1 on mouse release.
	tabDragIdx    int
	tabDragStartX float32 // mouse X at the moment tabDragIdx was set
	tabDragStartY float32 // mouse Y at the moment tabDragIdx was set

	contextMenuOpen        bool
	contextMenuX           float32
	contextMenuY           float32
	contextMenuOpenedFrame int  // frame the menu was opened on — close-on-click-outside skips this frame so the opening right-click doesn't immediately close it
	contextMenuCaptured    bool // SDL_CaptureMouse succeeded — best-effort global mouse capture so clicks outside our windows reach us; partial on Wayland and sdl2-compat → SDL3 X11
	contextMenuOutCount    int  // (legacy) running counter once used by an experimental cursor-out timer; kept as a field to avoid churning the struct layout during the menu-detection saga, currently unused

	// pendingRemeasure means this Window needs to re-run measureCell()
	// next frame — e.g. after the font atlas was rebuilt by
	// beforeRender. Per-Window because each Window can be at a
	// different font zoom, so the cellW/H produced by a measurement on
	// Window A doesn't apply to Window B. Whichever Window consumes
	// the flag first also updates the app-wide baseCellW/H, scaled
	// back from the Window's local fontSize via its scale ratio.
	pendingRemeasure bool

	// Multi-window plumbing. Every Window is equal: the cimgui-go
	// primary SDL_Window stays hidden as an invisible "carrier" for
	// the ImGui context, and every user-visible Window renders inside
	// an ImGui top-level window that multi-viewport auto-promotes to
	// its own OS window. imguiName is the stable ImGui identifier;
	// the display half of the Begin name (text before "###") comes
	// from titleForWindow() each frame so the OS title tracks the
	// active tab. imViewport caches the *imgui.Viewport for this
	// Window each frame so w.viewport() returns the right one for
	// coord translation and draw-list selection.
	//
	// pendingClose is set when the user hits the OS-window close
	// button (read via viewport.PlatformRequestClose) or when the
	// last tab in this Window closes. reapClosedWindows after the
	// iteration removes the Window from a.windows and tears down
	// its tabs/renderer. When a.windows becomes empty, the app
	// quits.
	imguiName     string // stable ImGui ID suffix (e.g. "win0")
	imViewport    *imgui.Viewport
	pendingClose  bool
	lastOSTitle   string // last OS-window title we set; avoids redundant syscalls
	pendingResize bool   // next frame, force SetNextWindowSize with CondAlways
	// pendingFocus is set by spawnWindow on freshly-created Windows.
	// After the new viewport's SDL_Window exists (first frame), the
	// main loop calls platform.RaiseWindow on it so macOS transitions
	// keyboard focus to the new NSWindow immediately — without this
	// the first keybind after Cmd+N (e.g. Cmd+T) routes to the
	// spawning Window because the OS hasn't moved focus yet.
	pendingFocus bool

	// resizeIncrementSet* tracks which native window + cell dimensions
	// we've already pushed to NSWindow.setContentResizeIncrements via
	// platform.SetResizeIncrements. Early frames can see either no
	// handle or the hidden carrier/main viewport before ImGui creates
	// this Window's popped-out SDL_Window, so the handle is part of the
	// cache key.
	resizeIncrementSetWindow uintptr
	resizeIncrementSetCellW  int
	resizeIncrementSetCellH  int

	// swallowOSCloseFrames suppresses PlatformRequestClose events for
	// this many subsequent frames. macOS's NSWindow performClose:
	// default-binds to Cmd+W, so when our close_tab keybind handles
	// Cmd+W the OS ALSO fires a window-close event. Without swallowing,
	// "close a tab" turns into "close the whole window." Our close_tab
	// dispatch arms this; reapClosedWindows / PlatformRequestClose
	// reads + decrements it.
	swallowOSCloseFrames int

	// Initial position for the first BeginV call. spawnWindow sets this
	// to the parent's current OS-window position plus a cascade offset
	// so new windows don't stack exactly on top of their parent.
	initialPosX   float32
	initialPosY   float32
	hasInitialPos bool

	// Actual top-left of the wrapper's content region in absolute
	// desktop coords, captured each frame via CursorScreenPos right
	// after BeginV returns. Used as the reference origin for cell
	// offsets and the tab-bar SetNextWindowPos. Necessary because
	// viewport.Pos() and ImGui's internal content origin can differ
	// in subtle ways (window border allowance, content-region inset
	// applied even with WindowPadding=0 pushed) — using the same
	// reference for both keeps them pixel-aligned.
	contentOriginX float32
	contentOriginY float32
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
// defaults needed before App.Run / spawnWindow populates the rest
// (backend, tabs, renderer, cell metrics). imguiName is assigned by
// the caller — every Window is multi-viewport-popped-out, none
// "owns" the cimgui-go primary SDL_Window (that's a hidden carrier
// kept alive only for the ImGui context).
func newWindow(app *App) *Window {
	return &Window{
		app:          app,
		scroll:       make(map[int]*scrollback.State),
		tabBarH:      0, // updated each frame from imgui.FrameHeight() when >1 tab
		tabSwitchReq: -1,
		tabDragIdx:   -1,
	}
}

// imguiSuffix returns an ImGui-window-ID suffix that makes any
// imgui.Begin / imgui.BeginTabBar etc. inside this Window's frame()
// unique per Window. Without this, the tab bar and search overlay
// share the same `##tabbar` / `##search` IDs across all Windows and
// ImGui treats them as the same window.
func (w *Window) imguiSuffix() string {
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

// sdlWindowHandle returns the SDL window ID (uint32, widened to
// uintptr for cgo) for this Window's OS window. ImGui's SDL2 backend
// stores the WindowID — not the SDL_Window* pointer — in
// ImGuiViewport::PlatformHandle (changed upstream 2024-08-19 to avoid
// dangling pointer races on window destroy). The C helpers that
// receive this value call SDL_GetWindowFromID to recover the actual
// SDL_Window*.
//
// Used by the SDL helpers that need to target the visible OS window
// (fullscreen toggle, macOS cell-snap resize increments) instead of
// the hidden cimgui-go carrier window which is what
// SDL_GL_GetCurrentWindow() returns.
//
// Returns 0 if the viewport hasn't been created yet (very early in
// startup, before the first BeginV call). Callers should treat 0 as
// "skip the operation" rather than crash.
func (w *Window) sdlWindowHandle() uintptr {
	vp := w.viewport()
	if vp == nil {
		return 0
	}
	return vp.PlatformHandle()
}

// bgDrawList returns the draw list terminal cells / cursor /
// scrollbar / link decorations render into.
//
// Every Window now renders inside an ImGui top-level Begin/End
// (wrappedFrame's wrapper for multi-viewport pop-out). We draw the
// terminal into the WRAPPER's window draw list rather than the
// viewport's background draw list — the latter renders BELOW the
// wrapper's content, which means subsequent peer windows (tab bar,
// search overlay) created with separate Begin/End would still
// layer on top of terminal cells, but the wrapper's own
// NoBringToFrontOnFocus interaction with multi-viewport popped-out
// windows had bg-drawlist landing in front of the tab bar.
// Putting cells on the wrapper's window draw list orders them
// correctly: wrapper's drawlist first, then any peer windows
// (tab bar, etc.) on top, then viewport foreground drawlist last.
//
// Must be called while the wrapper's Begin/End is still on the
// ImGui window stack — wrappedFrame guarantees that since frame()
// runs inside the wrapper.
func (w *Window) bgDrawList() *imgui.DrawList {
	return imgui.WindowDrawList()
}

// fgDrawList returns the foreground draw list for this Window's
// viewport — what the resize overlay renders into. Z-order: above
// all ImGui windows, so the overlay sits on top of any popped-out
// prefs / search overlays.
func (w *Window) fgDrawList() *imgui.DrawList {
	return imgui.ForegroundDrawListViewportPtrV(w.viewport())
}
