package app

import (
	"sync"

	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/platform"
	"github.com/LXXero/xerotty/internal/renderer"
	"github.com/LXXero/xerotty/internal/scrollback"
	"github.com/LXXero/xerotty/internal/tabs"
	"github.com/LXXero/xerotty/internal/terminal"
)

// resizeRequest records one in-flight terminal resize request for
// the reconciliation loop (see frame()'s "Resize reconciliation").
type resizeRequest struct {
	cols, rows int
	at         float64 // imgui.Time() when sent
}

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

	// daemonWindowID is the server-side window ID this GUI window
	// corresponds to on the LOCAL/default daemon. Kept for
	// backwards compatibility with code that already knows it
	// only cares about the primary daemon (installSourceFactory,
	// adoption paths). For mixed local+remote tabs the per-hub
	// view lives in daemonWindowIDs below.
	daemonWindowID uint32

	// daemonWindowIDs maps each daemon hub this GUI window has
	// ever touched to the server-side window ID created on that
	// hub. Lets focus/reorder/move operations send the RIGHT
	// window ID to the RIGHT hub — without this map, a remote
	// tab's reorder used to send the local-daemon's window ID to
	// the remote daemon (mismatched ID spaces). Lazily populated
	// as needed by windowIDFor.
	daemonWindowIDsMu sync.Mutex
	daemonWindowIDs   map[*daemonsource.Hub]uint32

	// lastSentFocusTabID is the most recent tab ID we shipped to
	// the daemon via SendTabFocus / SendWindowFocusTab. Per-frame
	// focus check compares against w.tabs.Active().ID and fires
	// only on change so we don't spam the wire with redundant
	// focus messages.
	lastSentFocusTabID uint32

	// pendingDaemonMove holds a Source that landed via cross-
	// window drag (spawnWindowImpl with adopt!=nil) before this
	// Window had a daemonWindowID. Once the window registers with
	// its daemon (next frame after SendWindowCreate completes),
	// syncDaemonFocus / frame fire the deferred MoveTab so the
	// daemon's layout matches the GUI's. Cleared after fire.
	pendingDaemonMove terminal.Source

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
	// daemonSearch tracks the in-flight daemon-side search per tab
	// (windowed daemon tabs only) so the frame loop re-requests only
	// when the query/options change. Cleared when the overlay closes.
	daemonSearch map[int]*daemonSearchState
	renderer     *renderer.Renderer

	// Per-Window UI state. Each Window tracks its own selection,
	// hovered link, search overlay, prefs dialog, etc. — moving any
	// of this to App would couple two open Windows together (e.g.,
	// dragging a selection in Window A would extend a selection in
	// Window B).
	fullscreen    bool
	tabBarHovered bool
	tabSwitchReq  int
	ready         bool
	sel           selection
	// resizeReq tracks the last terminal-resize request per tab ID
	// for the per-frame resize reconciliation: dedupes re-requests
	// while a daemon round-trip is in flight (the shadow grid keeps
	// reporting the old size until the daemon's frames land).
	resizeReq map[int]resizeRequest

	// damageLastGen / damageLastTitle remember each tab's last seen
	// content generation and display title so windowVisuallyDirty can
	// detect "something the user can see changed" cheaply per frame.
	damageLastGen   map[int]uint64
	damageLastTitle map[int]string

	// lastBgMark is when this window last marked itself dirty while
	// in the background — the background_fps throttle clock.
	lastBgMark float64

	// dragScrollAccum carries fractional rows of edge auto-scroll
	// across frames (drag-selection past the top/bottom edge is
	// time-based: rows/sec × frame dt rarely lands on whole rows).
	dragScrollAccum    float64
	pendingPaste       string
	resizeTime         float64
	resizeOverlay      bool
	resizeOverlayText  string
	lastCols           int
	lastRows           int
	hoveredLink        *linkHit
	renamingTab        bool
	renameBuffer       string
	connectingHost     bool   // "Connect to host…" dialog open
	connectFocus       bool   // request keyboard focus into the dest field
	connectBuffer      string // typed SSH destination (user@host / alias)
	connectArgsBuffer  string // optional extra ssh args (e.g. "-p 2222 -i key")
	connectError       string // last connect failure, shown in the dialog
	sbDragging         bool
	searchFocusInput   bool
	// searchJumpOnResult: scroll to the nearest match when the
	// in-flight async scan lands (set on query/option edits — the
	// results don't exist yet on the edit frame).
	searchJumpOnResult bool
	searchInputFocused bool
	searchOverlayW     float32
	lastDblClickTime   float64
	lastDblClickRow    int
	lastDblClickCol    int
	prefDialog         configDialog
	lastTabBarW        float32
	lastTabBarH        float32
	skipDisplaySync    int
	// appliedOpacity is the cfg.Appearance.Opacity value last pushed to
	// SDL_SetWindowOpacity for this Window. Tracked so the per-frame apply
	// only calls SDL on change (first valid frame + live prefs edits),
	// never every frame. -1 sentinel until the first apply.
	appliedOpacity float32

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

	// pendingRemeasure means this Window needs to re-run measureCell()
	// next frame — e.g. after the font atlas was rebuilt by
	// beforeRender. Per-Window because each Window can be at a
	// different font zoom, so the cellW/H produced by a measurement on
	// Window A doesn't apply to Window B. Whichever Window consumes
	// the flag first also updates the app-wide baseCellW/H, scaled
	// back from the Window's local fontSize via its scale ratio.
	pendingRemeasure bool

	// lastGeom* dedupe the per-frame geometry push to the owning
	// daemon(s) (pushGeometry): only changes go on the wire.
	// geomPushed false means "never pushed yet".
	geomPushed           bool
	lastGeomX, lastGeomY int32
	lastGeomW, lastGeomH int32

	// restoredGeom means width/height were restored from a reattach
	// snapshot's recorded geometry (real pixels from the previous
	// session, tab bar included). The first-frame cols×rows re-fit
	// must skip such windows — "correcting" them to a bare grid is
	// exactly the reattach lost-row bug.
	restoredGeom bool

	// restoredWithBar means the restored geometry came from a
	// multi-tab window, i.e. the height ALREADY includes the tab
	// bar. The bar's first appearance after adoption must then skip
	// the grow-by-bar-height compensation — otherwise the bar is
	// double-counted and the window gains one bar height per
	// reattach, compounding forever. Consumed (cleared) at that
	// first transition; later 1↔2 transitions compensate as usual.
	restoredWithBar bool

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
	imguiName    string // stable ImGui ID suffix (e.g. "win0")
	imViewport   *imgui.Viewport
	pendingClose bool
	// daemonReseatPending is set when this Window emptied because its
	// daemon-backed tabs all VANISHED (the daemon process restarted and
	// took the shells with it) rather than the user closing them. Closing
	// the last window calls platform.Quit(), so a local daemon restart
	// would silently exit the whole GUI — instead we keep the Window alive
	// and reseat a fresh tab on the reconnected daemon. The flag persists
	// across frames so that if the hub is still mid-redial (NewTab returns
	// a transient error) we retry next frame instead of falling through to
	// pendingClose. Cleared once a tab is successfully created.
	daemonReseatPending bool
	// daemonReseatMinted guards the daemon-window CreateWindow so it
	// happens AT MOST ONCE per reseat episode. CreateWindow is a
	// synchronous hub RPC; the original reseat re-minted it on every
	// retry frame, so a CreateWindow-succeeds-but-NewTab-fails frame
	// leaked a fresh daemon window each tick. Set when the window is
	// minted, cleared (with daemonReseatPending) once NewTab finally
	// succeeds — retries reuse the minted window and only redo NewTab.
	daemonReseatMinted bool
	// daemonReseatInstance is the daemon InstanceID the reseat window was
	// minted against. It scopes the mint-once to a SINGLE restart episode:
	// if a SECOND daemon restart happens while a reseat is still pending
	// (CreateWindow done, NewTab not yet succeeded), the minted window ID
	// belongs to the now-dead intermediate daemon. Comparing this against
	// the hub's current InstanceID detects that and forces a re-mint on
	// the new daemon, so focus/move don't keep targeting a stale window.
	daemonReseatInstance string
	lastOSTitle          string // last OS-window title we set; avoids redundant syscalls
	pendingResize        bool   // next frame, force SetNextWindowSize with CondAlways
	// pendingFocus asks the main loop to raise + key-focus this Window's
	// SDL viewport after the frame. Used when a new Window is spawned
	// and when an auxiliary viewport (preferences) closes and focus
	// should return to its owning terminal Window.
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

	// iconApplied flips true after the embedded app icon has been
	// pushed to this Window's SDL_Window via SDL_SetWindowIcon.
	// One-shot — same retry-until-handle-valid pattern as the resize
	// increments above. No-op on macOS (bundle's .icns wins for the
	// Dock; per-window SDL icon isn't useful there).
	iconApplied bool

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
		app:            app,
		scroll:         make(map[int]*scrollback.State),
		daemonSearch:   make(map[int]*daemonSearchState),
		tabBarH:        0, // updated each frame from imgui.FrameHeight() when >1 tab
		tabSwitchReq:   -1,
		tabDragIdx:     -1,
		appliedOpacity: -1, // sentinel: forces opacity apply on first valid frame
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

// hasOSFocus reports whether this Window's OS window currently holds
// input focus, per SDL (the OS truth). Used to gate click-DOWN
// handling so a click that actually landed on a DIFFERENT window —
// another xerotty window or another application entirely — doesn't
// leak into this window's selection or tab bar. ImGui's own hover/
// focus state can't be trusted here: on mac multi-viewport it never
// receives a mouse-leave when focus jumps to another OS window, so
// it keeps reporting the wrapper hovered and the leaked click slips
// through. Returns true when the handle is unknown (0) so behavior
// degrades to the pre-gate state rather than swallowing every click.
func (w *Window) hasOSFocus() bool {
	h := w.sdlWindowHandle()
	if h == 0 {
		return true
	}
	return platform.WindowHasInputFocus(h)
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

// pushGeometry sends this Window's current position/size to every
// daemon that has a server-side window for it, deduped to changes.
// Called once per frame from the main loop. The daemon's geometry
// hints are what a future reattach restores, so they must track live
// resizes/moves: a spawn-time-only push bakes in the pre-tab-bar
// height, and a reattached multi-tab window then comes back a row
// short of its configured grid once the bar appears.
func (w *Window) pushGeometry() {
	if w.imViewport == nil {
		return
	}
	pos := w.imViewport.Pos()
	x, y := int32(pos.X), int32(pos.Y)
	width, height := int32(w.width), int32(w.height)
	if w.geomPushed && x == w.lastGeomX && y == w.lastGeomY &&
		width == w.lastGeomW && height == w.lastGeomH {
		return
	}
	pushed := false
	if w.app.daemonHub != nil && w.daemonWindowID != 0 {
		_ = w.app.daemonHub.Client().SendWindowGeometry(w.daemonWindowID, x, y, width, height)
		pushed = true
	}
	w.daemonWindowIDsMu.Lock()
	for hub, id := range w.daemonWindowIDs {
		_ = hub.Client().SendWindowGeometry(id, x, y, width, height)
		pushed = true
	}
	w.daemonWindowIDsMu.Unlock()
	if pushed {
		w.geomPushed = true
		w.lastGeomX, w.lastGeomY, w.lastGeomW, w.lastGeomH = x, y, width, height
	}
}

// windowVisuallyDirty reports whether anything in this Window could
// have changed on screen this frame — content generations, tab
// titles, bells, focus/hover (blink, hover effects), or any open
// overlay/interaction. Windows that aren't dirty (and got no SDL
// events) skip their GL render + vsync swap for the frame: one tab
// streaming output stops costing every other window its paint. Errs
// dirty on anything uncertain — a wasted paint is invisible, a
// missed one is a stale-pixel bug.
func (w *Window) windowVisuallyDirty() bool {
	dirty := false
	if w.damageLastGen == nil {
		w.damageLastGen = make(map[int]uint64)
		w.damageLastTitle = make(map[int]string)
	}
	for _, tab := range w.tabs.Tabs {
		if tab.Terminal == nil {
			continue
		}
		if g := tab.Terminal.RenderGeneration(); g != w.damageLastGen[tab.ID] {
			w.damageLastGen[tab.ID] = g
			dirty = true
		}
		if t := tab.DisplayTitle(); t != w.damageLastTitle[tab.ID] {
			w.damageLastTitle[tab.ID] = t
			dirty = true
		}
		if tab.BellPending() {
			dirty = true
		}
	}
	if dirty || !w.ready || w.pendingResize || w.pendingRemeasure ||
		w.renamingTab || w.connectingHost || w.prefDialog.open ||
		w.sel.dragging || w.sel.active || w.tabDragIdx >= 0 ||
		w.contextMenuOpen || w.resizeOverlay {
		return true
	}
	// Focused window repaints every frame: cursor blink arrives as a
	// timer wake with NO events, and typing echo must never lag.
	// Hovered window repaints for ImGui hover transitions.
	if w.hasOSFocus() {
		return true
	}
	if h := w.sdlWindowHandle(); h != 0 && platform.MouseFocusWindowID() == h {
		return true
	}
	if tab := w.tabs.Active(); tab != nil {
		if s, ok := w.scroll[tab.ID]; ok && s.Searching {
			return true
		}
	}
	return false
}
