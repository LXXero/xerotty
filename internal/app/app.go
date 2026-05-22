// Package app handles the SDL2/ImGui lifecycle and main render loop.
package app

import (
	"fmt"
	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/fontsys"
	"github.com/LXXero/xerotty/internal/glyphcache"
	"github.com/LXXero/xerotty/internal/input"
	"github.com/LXXero/xerotty/internal/menu"
	"github.com/LXXero/xerotty/internal/platform"
	"github.com/LXXero/xerotty/internal/renderer"
	"github.com/LXXero/xerotty/internal/scrollback"
	"github.com/LXXero/xerotty/internal/sdlhack"
	"github.com/LXXero/xerotty/internal/tabs"
	"github.com/LXXero/xerotty/internal/terminal"
	"github.com/LXXero/xerotty/internal/themes"
	"math"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// disableMirror is a runtime kill switch for the macOS mouse-event
// mirror — set XEROTTY_DISABLE_MIRROR=1 to shut it off entirely. Kept
// because the mirror is the kind of fragile workaround we want a way
// to disable if a future SDL/cimgui-go upgrade fixes the underlying
// Cocoa event-drop bug and the mirror becomes net-negative.
var disableMirror = os.Getenv("XEROTTY_DISABLE_MIRROR") != ""

func init() {
	runtime.LockOSThread()
}

// App is the main application struct. It owns process-wide state
// (config, theme, base font metrics, font-reload flags) plus the
// slice of Windows in this process. One xerotty process can host
// multiple OS windows (Cmd+N / "new_window") — see
// docs/MULTI_WINDOW_REFACTOR.md for why this matters on macOS Dock
// coalescing.
type App struct {
	cfg             config.Config
	theme           renderer.Theme
	baseFontSize    float32 // font size the atlas was built at
	baseCellW       float32 // cell width at base font size
	baseCellH       float32 // cell height at base font size
	pendingFontFace bool    // rebuild font atlas at start of next frame

	// Daemon-source plumbing — only populated when cfg.Tabs.Source
	// == "daemon". hub owns the connection + frame router;
	// tabSourceFactory is what tabs.Manager.SourceFactory points at
	// for every Window so all new tabs route through the daemon.
	daemonHub        *daemonsource.Hub
	tabSourceFactory func(cols, rows int, cwd string) (terminal.Source, error)

	windows []*Window // every OS window currently open in this process
	active  *Window   // the window with input focus, or windows[0] if none

	// dragTab is the process-wide state for an in-progress drag of a
	// tab across windows. nil when no drag is happening. Set when the
	// user drags a tab down past the tab bar (lift-off threshold)
	// from any Window's tab bar; the source Window has already
	// RemoveTab'd the entry by then so the Terminal lives only in
	// dragTab.Term until release. The app resolves the final target
	// once per frame after all Windows have rendered.
	dragTab *tabDrag

	// prevOsLeftDown is the OS-level left mouse button state observed
	// at the end of the previous frame's mouse-mirror check. The mirror
	// injects events on TRANSITIONS of this value rather than diffing
	// against ImGui's IsMouseDown — IsMouseDown can lag behind the
	// real OS state during macOS focus shifts (the exact scenario the
	// mirror exists to compensate for), which causes the mirror to
	// skip injection when ImGui's view spuriously already matches a
	// half-applied OS state.
	prevOsLeftDown bool

	// anyWindowMovingThisFrame is set at the top of wrappedFrame
	// if any Window's NSWindow.frame.origin changed since last
	// frame (i.e. the OS is dragging the window). Read by the
	// mouse mirror's deferred-DOWN logic to abort an
	// about-to-be-injected synthetic DOWN whose source turns out
	// to be a window-drag gesture rather than a content click.
	anyWindowMovingThisFrame bool

	// mirrorPendingDown is set on the OS button-down edge — but
	// instead of injecting the synthetic DOWN immediately we wait
	// a few frames. If the window starts moving OR the cursor
	// moves substantially during the wait, the click was a drag
	// gesture (window drag or selection drag) and should be
	// aborted (the latter already had its DOWN delivered through
	// SDL; the former should never inject). Otherwise inject after
	// the timer expires.
	mirrorPendingDown       bool
	mirrorPendingPos        imgui.Vec2
	mirrorPendingGlobalX    int
	mirrorPendingGlobalY    int
	mirrorPendingFramesLeft int
}

// tabDrag is the in-flight state of a tab being dragged across
// Windows. The Terminal lives ONLY inside Term while drag is
// active — source Window's tabs.Manager has already RemoveTab'd
// it. On release, either the cursor's-over Window adopts the
// Terminal (intra-process Window-to-Window move) or a new Window
// gets spawned and adopts it (detach-to-new).
//
// LastFocus is only a fallback signal. On Wayland the compositor's
// implicit pointer grab can keep it stuck on the source Window, so a
// Wayland data-device drop target or geometry hit-test wins first.
type tabDrag struct {
	Term               terminal.Source
	Label              string  // for floating-preview rendering
	From               *Window // window the tab originated in; used to reject stale source focus
	LastFocus          uintptr // SDL_WindowID last seen under the cursor during the drag
	WaylandStarted     bool
	WaylandDropSeen    bool
	WaylandDropSurface uintptr
}

// New creates a new App with the given config. The initial Window
// struct is allocated here; its SDL_Window, GL context, tabs, and
// renderer are populated during Run() once the cimgui-go backend is
// initialized.
func New(cfg config.Config) *App {
	a := &App{cfg: cfg}
	if cfg.Tabs.Source == "daemon" {
		if err := a.initDaemonSource(); err != nil {
			// Don't fail GUI startup — fall back to in-process
			// PTY tabs and surface the error on stderr so the
			// user knows daemon mode is degraded.
			fmt.Fprintf(os.Stderr, "xerotty: daemon mode requested but unavailable, falling back to in-process PTY: %v\n", err)
		}
	}
	w := newWindow(a)
	a.windows = append(a.windows, w)
	a.active = w
	return a
}

// initDaemonSource ensures a local xerotty daemon is running,
// connects to it, attaches, and wires the resulting Hub into App so
// tabs.Manager.NewTab routes through it. Errors here are non-fatal
// — caller logs + falls back to PTY tabs.
func (a *App) initDaemonSource() error {
	cli, err := daemonsource.EnsureLocalDaemon(a.cfg.Tabs.DaemonSocket)
	if err != nil {
		return err
	}
	if _, err := cli.Hello("xerotty-gui"); err != nil {
		_ = cli.Close()
		return fmt.Errorf("hello: %w", err)
	}
	go cli.Run()
	// Attach with NewIfMissing=false: the GUI doesn't want an
	// auto-tab from the daemon's default-tab-on-empty path; we'll
	// create tabs ourselves via NewTab when the user asks.
	if err := cli.Attach("", false); err != nil {
		_ = cli.Close()
		return fmt.Errorf("attach: %w", err)
	}
	// Drain the Attached frame so it doesn't sit blocking the
	// client's channel forever.
	select {
	case <-cli.Attached():
	case <-time.After(2 * time.Second):
		_ = cli.Close()
		return fmt.Errorf("attach: no response from daemon")
	}
	hub := daemonsource.NewHub(cli)
	a.daemonHub = hub
	a.tabSourceFactory = func(cols, rows int, cwd string) (terminal.Source, error) {
		return hub.NewTab(cols, rows, cwd)
	}
	return nil
}

// reapClosedWindows removes Windows the user closed this frame. All
// Windows are equal — no "main" — so the cleanup is uniform. The
// loop in wrappedFrame quits the process if a.windows becomes empty
// after the reap (last visible Window gone = app exit).
func (a *App) reapClosedWindows() {
	survivors := a.windows[:0]
	for _, w := range a.windows {
		if !w.pendingClose {
			survivors = append(survivors, w)
			continue
		}
		// Tear down the Window's terminals and renderer.
		if w.tabs != nil {
			for _, tab := range w.tabs.Tabs {
				tab.Terminal.Close()
			}
		}
		if w.renderer != nil && w.renderer.Glyphs != nil {
			w.renderer.Glyphs.Close()
			w.renderer.Glyphs = nil
		}
	}
	a.windows = survivors
	// If the active Window was reaped, point at any surviving one.
	if a.active != nil {
		found := false
		for _, w := range a.windows {
			if w == a.active {
				found = true
				break
			}
		}
		if !found {
			a.active = a.bestFocusReplacement()
		}
	}
}

func (a *App) bestFocusReplacement() *Window {
	var fallback *Window
	for i := len(a.windows) - 1; i >= 0; i-- {
		if !a.windows[i].pendingClose {
			fallback = a.windows[i]
			break
		}
	}

	bestRank := int(^uint(0) >> 1)
	var best *Window
	for _, win := range a.windows {
		if win.pendingClose || win.imViewport == nil {
			continue
		}
		handle := win.imViewport.PlatformHandle()
		if handle == 0 {
			continue
		}
		rank := platform.CocoaWindowZRank(handle)
		if rank >= 0 && rank < bestRank {
			bestRank = rank
			best = win
		}
	}
	if best != nil {
		return best
	}
	return fallback
}

// mouseOverOwnedContent returns true iff the OS-level cursor is
// currently inside the content rect of any visible Window in this
// App. Used by the macOS mouse-mirror to gate synthetic DOWN
// injection: we only inject if the user clicked on something this
// app owns, otherwise a click on the desktop or another app would
// generate a phantom terminal selection here.
//
// Primary signal: ImGui's MouseHoveredViewport, which is the
// platform-reported viewport-under-cursor (set each frame by the
// SDL backend). If ImGui has a hovered viewport and it's one of
// ours, return true.
//
// Fallback: iterate our Window list and bounds-check the OS-level
// cursor against each viewport's Pos/Size. We never used to need
// the fallback before realizing MouseHoveredViewport can briefly
// be 0 right after a focus shift (the very moment the mirror is
// trying to compensate for).
// mouseOverOwnedContentPos: like mouseOverOwnedContent, but also
// returns the mouse pos we determined to be "inside one of our
// viewports" — used by the mirror to feed ImGui a valid AddMousePosEvent
// when ImGui's own MousePos is sentinel (-FLT_MAX) during a focus
// shift. Without that, a synthetic DOWN lands at -FLT_MAX, the hit
// test misses every widget, and the click does nothing.
func (a *App) mouseOverOwnedContentPos() (bool, imgui.Vec2) {
	ok := a.mouseOverOwnedContent()
	if !ok {
		return false, imgui.Vec2{}
	}
	// Recompute the in-viewport position so callers can feed it to
	// ImGui. Prefer ImGui's tracked MousePos when valid; fall back
	// to SDL's OS-level pos (raw or scaled — whichever fits a
	// viewport).
	mp := imgui.MousePos()
	const noMouseSentinel = -3.4e+37
	if mp.X > noMouseSentinel {
		return true, mp
	}
	gx, gy := sdlhack.GlobalMousePos()
	rawPos := imgui.Vec2{X: float32(gx), Y: float32(gy)}
	fbScale := imgui.CurrentIO().DisplayFramebufferScale().X
	if fbScale < 1 {
		fbScale = 1
	}
	scaledPos := imgui.Vec2{X: rawPos.X / fbScale, Y: rawPos.Y / fbScale}
	for _, w := range a.windows {
		if w.imViewport == nil {
			continue
		}
		pos := w.imViewport.Pos()
		size := w.imViewport.Size()
		if rawPos.X >= pos.X && rawPos.X < pos.X+size.X &&
			rawPos.Y >= pos.Y && rawPos.Y < pos.Y+size.Y {
			return true, rawPos
		}
		if scaledPos.X >= pos.X && scaledPos.X < pos.X+size.X &&
			scaledPos.Y >= pos.Y && scaledPos.Y < pos.Y+size.Y {
			return true, scaledPos
		}
	}
	return true, rawPos // shouldn't reach (mouseOverOwnedContent said ok), but be safe
}

func (a *App) mouseOverOwnedContent() bool {
	// Primary: ImGui's MouseHoveredViewport (platform-reported
	// viewport the cursor is over).
	hovered := imgui.CurrentIO().MouseHoveredViewport()
	if hovered != 0 {
		for _, w := range a.windows {
			if w.imViewport != nil && w.imViewport.ID() == hovered {
				return true
			}
		}
	}
	// Bounds-check the mouse pos against each viewport rect. Use
	// ImGui's MousePos if it has a real value, otherwise fall back
	// to SDL_GetGlobalMouseState. ImGui sets MousePos to -FLT_MAX
	// when the app has no mouse focus (e.g. mid-Cocoa-focus-shift),
	// which is *exactly* when the mirror needs to compensate.
	mp := imgui.MousePos()
	const noMouseSentinel = -3.4e+37 // approximates -FLT_MAX comparison
	if mp.X <= noMouseSentinel {
		gx, gy := sdlhack.GlobalMousePos()
		mp = imgui.Vec2{X: float32(gx), Y: float32(gy)}
	}
	// SDL on macOS sometimes returns physical pixels for
	// SDL_GetGlobalMouseState even though viewport.Pos/Size are in
	// logical pixels — observed in practice as (~2× the expected
	// values) on Retina. Compute a scale-corrected pos too so the
	// bounds check accepts either unit if it lands inside a viewport.
	fbScale := imgui.CurrentIO().DisplayFramebufferScale().X
	if fbScale < 1 {
		fbScale = 1
	}
	mpScaled := imgui.Vec2{X: mp.X / fbScale, Y: mp.Y / fbScale}
	for _, w := range a.windows {
		if w.imViewport == nil {
			continue
		}
		pos := w.imViewport.Pos()
		size := w.imViewport.Size()
		inRaw := mp.X >= pos.X && mp.X < pos.X+size.X &&
			mp.Y >= pos.Y && mp.Y < pos.Y+size.Y
		inScaled := mpScaled.X >= pos.X && mpScaled.X < pos.X+size.X &&
			mpScaled.Y >= pos.Y && mpScaled.Y < pos.Y+size.Y
		if inRaw || inScaled {
			return true
		}
	}
	return false
}

// startMouseMirrorWakePoller keeps the event-driven run loop responsive to
// macOS mouse-button edges even when SDL drops the underlying Cocoa event.
// It only pushes a wake event; the actual ImGui injection still happens on
// the main thread from Window.frame().
func (a *App) startMouseMirrorWakePoller() func() {
	if runtime.GOOS != "darwin" || disableMirror {
		return func() {}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)

		prev := sdlhack.LeftButtonPhysicalDown()
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				down := sdlhack.LeftButtonPhysicalDown()
				if down != prev {
					prev = down
					platform.PostWake()
				}
			case <-stop:
				return
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

// spawnWindowAdopting spawns a new Window like spawnWindow, except
// its first tab adopts the given already-running Terminal instead
// of starting a fresh shell. Used by cross-Window tab drag when the
// user drops the floating tab outside any existing Window.
func (a *App) spawnWindowAdopting(term terminal.Source) {
	a.spawnWindowImpl(term)
}

// spawnWindow adds a new Window to this App in the same process. The
// secondary Window shares the cimgui-go SDL backend + ImGui context
// + GL textures with the main Window, but has its own tabs manager,
// renderer, glyph cache, and per-window UI state. The render loop in
// Run() then wraps its frame body in an ImGui top-level window that
// multi-viewport auto-promotes to its own OS window.
func (a *App) spawnWindow() {
	a.spawnWindowImpl(nil)
}

func (a *App) spawnWindowImpl(adopt terminal.Source) {
	if len(a.windows) == 0 {
		return // can't spawn before Run() has set up the main Window
	}
	main := a.windows[0]
	if main.renderer == nil {
		return // main Window isn't fully initialized yet
	}

	w := newWindow(a)
	// Stable ImGui ID — display title is computed per-frame via the
	// "###" separator so it can reflect the active tab's title without
	// invalidating the window's identity.
	w.imguiName = fmt.Sprintf("xerottywin%d", len(a.windows))
	// Inherit the spawning Window's font size + cell metrics — Cmd+N
	// from a zoomed window opens a new window at the same zoom, just
	// like iTerm2. From then on the two diverge if user Cmd+= on
	// either independently.
	parent := a.active
	if parent == nil {
		parent = main
	}
	w.fontSize = parent.fontSize
	w.cellW = parent.cellW
	w.cellH = parent.cellH
	// Cascade the new Window's initial position so it doesn't open
	// exactly on top of its parent. Use the parent's current OS-window
	// position plus a small offset — same pattern macOS/iTerm2 use for
	// new-window placement.
	// Cascade by ~30% of the parent's size on each axis. xfce4-terminal
	// itself does no explicit cascade (it leaves placement to the WM —
	// gtk_window_move is only called for parsed --geometry strings,
	// confirmed in terminal-app.c upstream), so there's no "match xfce4"
	// number to copy. Fixed-pixel offsets (the old 30 / 60) look fine
	// on small windows and lost on large ones; scaling by parent size
	// keeps the offset visually consistent across zoom / configured cols
	// × rows. 30% is enough that the two title bars don't overlap and
	// the new window's content area starts well clear of the parent's,
	// without going so far that the new window lands off-screen for
	// users near a display edge.
	if parent.imViewport != nil {
		pp := parent.imViewport.Pos()
		ps := parent.imViewport.Size()
		w.initialPosX = pp.X + ps.X*0.30
		w.initialPosY = pp.Y + ps.Y*0.30
		w.hasInitialPos = true
	}
	// Geometry: configured Window.Columns × Window.Rows, NOT the
	// parent's width/height. New Windows always start with a single
	// tab so there's no tab bar; copying the parent's dimensions
	// (which on a multi-tab parent include tabBarH worth of vertical
	// space the new Window won't use) would manifest as the new
	// Window opening one row taller than configured. tabBarH starts
	// at 0 and is recomputed each frame in frame() based on tab
	// count.
	w.tabBarH = 0
	cfgCols, cfgRows := a.cfg.Window.Columns, a.cfg.Window.Rows
	if cfgCols < 2 {
		cfgCols = 80
	}
	if cfgRows < 2 {
		cfgRows = 24
	}
	padX2 := float32(a.cfg.Appearance.Padding) * 2
	w.width = int(math.Ceil(float64(float32(cfgCols)*w.cellW + padX2 + cellSafetyMarginH + cellOriginInsetX)))
	w.height = int(math.Ceil(float64(float32(cfgRows)*w.cellH + padX2 + cellSafetyMarginV)))
	// (Old: w.backend = main.backend — no per-Window backend now.
	// Every Window shares the process-wide platform layer and uses
	// ImGui multi-viewport for its OS window.)
	_ = main

	// Per-window renderer — shares the same font handles but gets its
	// own Metrics / scrollbar / glyph cache so per-Window theming etc.
	// stays possible later.
	pad := float32(a.cfg.Appearance.Padding)
	w.renderer = renderer.New(
		a.theme,
		renderer.CellMetrics{Width: w.cellW, Height: w.cellH},
		parent.renderer.Font, w.fontSize,
	)
	w.renderer.FontBold = parent.renderer.FontBold
	w.renderer.OffsetX = pad
	w.renderer.OffsetY = w.tabBarH + pad
	w.renderer.BoldIsBright = a.cfg.Appearance.BoldIsBright

	// New glyph cache for this Window (own textures via the shared
	// TextureManager).
	if fontsys.Default != nil {
		primaryPath := renderer.ResolveFontPath(&a.cfg)
		if primaryPath != "" {
			fbScale := imgui.CurrentIO().DisplayFramebufferScale().X
			if fbScale <= 0 {
				fbScale = 1
			}
			// Cache uses this Window's own fontSize so glyphs are
			// rasterized at the right physical size (each Window can
			// have a different zoom).
			if c, err := glyphcache.New(fontsys.Default, platform.Textures(), primaryPath, w.fontSize, fbScale); err == nil {
				w.renderer.Glyphs = c
			}
		}
	}

	// New tabs manager with a single starting tab. cfg.Tabs.InheritCWD
	// makes the new Window's shell start in the parent Window's active
	// tab's CWD — Cmd+N from inside ~/src/foo gives a new Window also
	// in ~/src/foo, matching iTerm/Terminal.app's behavior.
	w.tabs = tabs.NewManager(&a.cfg)
	w.tabs.SourceFactory = a.tabSourceFactory
	cols, rows := w.gridSize()
	if adopt != nil {
		// Adopt the already-running Terminal from a cross-Window
		// tab drag. Resize it to this Window's grid so its PTY +
		// vt emulator match what the new Window's renderer expects.
		if cols > 1 && rows > 1 {
			adopt.Resize(cols, rows)
		}
		w.tabs.AdoptTab(adopt)
	} else {
		var cwd string
		if a.cfg.Tabs.InheritCWD && parent != nil {
			if parentTab := parent.tabs.Active(); parentTab != nil && parentTab.Terminal != nil {
				cwd = parentTab.Terminal.GetCWD()
			}
		}
		if _, err := w.tabs.NewTab(cols, rows, cwd); err != nil {
			return
		}
	}

	// Skip the main-Window-specific first-frame init (CreateWindow,
	// SetWindowSize, etc.) — multi-viewport handles the platform
	// window creation for us when the Begin/End wrapper runs.
	w.ready = true
	// Flag this Window for OS-focus override once its viewport exists.
	// Without this, the first frame's `focused` calc (driven by ImGui's
	// IsWindowFocused) returns the spawning Window because OS focus
	// hasn't transitioned yet — clobbering our manual a.active=w
	// assignment below and routing the next keybind to the wrong
	// Window. The override is cleared once we've raised the new SDL
	// window in wrappedFrame's post-loop block.
	w.pendingFocus = true

	a.windows = append(a.windows, w)
	a.active = w
}

// initialWindowSize returns the pixel dimensions for the SDL window based on
// the configured columns/rows and an estimate of cell metrics. The estimate
// is corrected on the first frame once the font atlas is measured.
func (w *Window) initialWindowSize() (int, int) {
	px := renderer.PixelSize(&w.app.cfg)
	estCellW := px * 0.6
	estCellH := px * 1.2
	cols, rows := w.app.cfg.Window.Columns, w.app.cfg.Window.Rows
	if cols < 2 {
		cols = 80
	}
	if rows < 2 {
		rows = 24
	}
	pad := float32(w.app.cfg.Appearance.Padding) * 2
	// Add cellSafetyMargin so the eventual gridSize() after window creation
	// computes back to the same cols/rows we requested.
	wp := int(math.Ceil(float64(float32(cols)*estCellW + pad + cellSafetyMarginH + cellOriginInsetX)))
	hp := int(math.Ceil(float64(float32(rows)*estCellH + pad + cellSafetyMarginV)))
	return wp, hp
}

// Run starts the application main loop.
func (a *App) Run() error {
	w := a.active // initial Window allocated by New()

	// Load theme
	theme, err := themes.Load(a.cfg.Appearance.Theme)
	if err != nil {
		theme = renderer.DefaultTheme()
	}
	applyColorOverrides(&theme, &a.cfg)
	a.theme = theme

	w.width, w.height = w.initialWindowSize()

	// platform.Init creates the SDL3 window + GL context and brings up
	// Dear ImGui with the SDL3 + OpenGL3 backends. Replaces cimgui-go's
	// backend.CreateBackend + sdlbackend.NewSDLBackend + CreateWindow
	// chain — single call, no hidden carrier window (the OS window IS
	// the main window).
	if err := platform.Init("xerotty", w.width, w.height); err != nil {
		return fmt.Errorf("platform.Init: %w", err)
	}

	// Font reload runs BEFORE every frame's NewFrame. The alternative
	// (doing it inside our frame() callback after NewFrame already
	// snapshotted the current font) leaves a freed font in ImGui's
	// per-frame state and asserts when Render dereferences it.
	platform.SetBeforeRenderHook(a.beforeRender)

	// Hidden-carrier model: the SDL_Window platform.Init created holds
	// the GL context but isn't user-visible. Every Window — including
	// the first — pops out as its own OS window via ImGui multi-
	// viewport. Same approach as the SDL2 design; the platform layer
	// just needs an explicit hide call now.
	platform.HideMainWindow()
	w.imguiName = "xerottywin0"
	// The first visible terminal is also a multi-viewport pop-out, so it
	// needs the same explicit native focus handoff as later Cmd+N windows.
	// Otherwise macOS can leave keyboard focus on the hidden carrier until
	// the user clicks the terminal once.
	w.pendingFocus = true

	// Set background color from theme (ABGR → RGBA for the GL clear).
	bgR := float32((theme.Background>>0)&0xFF) / 255.0
	bgG := float32((theme.Background>>8)&0xFF) / 255.0
	bgB := float32((theme.Background>>16)&0xFF) / 255.0
	platform.SetBgColor(imgui.NewVec4(bgR, bgG, bgB, 1.0))
	// (Old SetTargetFPS(120) removed — the platform's WaitEventTimeout
	// loop is event-driven and self-paces to the display refresh.)

	// Keep multi-viewport enabled so child ImGui windows (preferences, etc.)
	// can be dragged out as native OS windows. The SDL backend already enables
	// this during init — re-asserting the bit defends against future changes
	// to that default. Coordinate-space conversions for mouse/scrollbar/draw
	// lists below use vp.Pos() to translate between desktop-absolute and
	// window-local pixels.
	io := imgui.CurrentIO()
	io.SetConfigFlags(io.ConfigFlags() | imgui.ConfigFlagsViewportsEnable)

	// Load font into atlas (must be after CreateWindow, before first frame)
	font, fontBold := renderer.LoadFont(&a.cfg)

	// Approximate metrics until first frame measures real ones.
	// baseFontSize is in pixels — that's what ImGui's atlas stores and what
	// gets passed to AddTextFontPtr each frame.
	pxSize := renderer.PixelSize(&a.cfg)
	a.baseFontSize = pxSize
	w.fontSize = pxSize
	w.cellW = pxSize * 0.6
	w.cellH = pxSize

	// Create renderer (metrics will be updated on first frame; the
	// OS-backed glyph cache is also built on first frame, once
	// DisplayFramebufferScale has been populated by ImGui's NewFrame).
	w.renderer = renderer.New(theme, renderer.CellMetrics{
		Width: w.cellW, Height: w.cellH,
	}, font, pxSize)
	w.renderer.FontBold = fontBold
	pad := float32(a.cfg.Appearance.Padding)
	w.renderer.OffsetX = pad
	w.renderer.OffsetY = w.tabBarH + pad
	w.renderer.BoldIsBright = a.cfg.Appearance.BoldIsBright

	// Tab manager (terminal creation deferred to first frame for accurate metrics)
	w.tabs = tabs.NewManager(&a.cfg)
	w.tabs.SourceFactory = a.tabSourceFactory

	// macOS-only: hook an SDL event watch so a real frame still renders
	// while AppKit's live-resize tracking mode holds the run loop.
	// Without this the OS just stretches the previous GL framebuffer
	// (the "image stretch" effect) until the user releases the drag.
	// No-op on other platforms.
	//
	// The wrapped frame body brackets each call with
	// liveResizeMainFrame{Begin,End}: while the main loop is inside its
	// frame body, the watch must not drive its own NewFrame (would
	// double-NewFrame and assert). When the main loop is between
	// frames — including blocked in SDL_WaitEventTimeout — the flag is
	// clear and the watch is free to render.
	//
	// Iterate every Window each frame. The main Window renders into
	// cimgui-go's primary SDL_Window via the main viewport. Secondary
	// Windows render inside an ImGui top-level window with NoDocking
	// + ConfigFlagsViewportsEnable, which makes multi-viewport auto-
	// promote each into its own OS window. Same SDL/GL/ImGui context,
	// same NSApplication on macOS — that's the trick that gives us
	// one Dock icon for N OS windows.
	wrappedFrame := func() {
		liveResizeMainFrameBegin()
		defer liveResizeMainFrameEnd()
		// (Modifier resync is in beforeRender — it has to run BEFORE
		// imgui.NewFrame consumes the input queue, otherwise the
		// re-asserted modifier state only takes effect next frame
		// and the current frame's KEY_DOWN edge sees a stale view.)
		// Detect "any window is currently being dragged" via the
		// live AppKit NSWindow.frame.origin, not ImGui's viewport.Pos
		// (which lags 1-2 frames during a continuous drag because
		// SDL_EVENT_WINDOW_MOVED + ImGui::UpdatePlatformWindows only
		// sync the position on the next frame).
		a.anyWindowMovingThisFrame = platform.CocoaAnyWindowMoved()
		// Track which Window has keyboard/click focus across this
		// frame. Input handling (keys, mouse-down, wheel, selection)
		// is gated to a.active in frame() — otherwise typing would
		// forward to every Window's PTY.
		var focused *Window
		for _, win := range a.windows {
			// Override the auto-derived viewport flags so the
			// platform window gets normal OS decorations (native
			// title bar, close/min/max). Without this override,
			// ImGui sees NoTitleBar on the ImGui window and
			// propagates NoDecoration to the platform viewport,
			// which makes SDL2 create the NSWindow as
			// SDL_WINDOW_BORDERLESS.
			windowClass := imgui.NewWindowClass()
			windowClass.SetViewportFlagsOverrideClear(imgui.ViewportFlagsNoDecoration)
			imgui.SetNextWindowClass(windowClass)
			windowClass.Destroy()
			// Position outside the main viewport the first time so
			// the auto-merge logic pops it out; after that ImGui
			// remembers per-window position. spawnWindow sets
			// hasInitialPos with a cascaded offset from the parent's
			// current OS position so new windows don't stack on top
			// of the parent.
			posX, posY := float32(100), float32(100)
			if win.hasInitialPos {
				posX, posY = win.initialPosX, win.initialPosY
				win.hasInitialPos = false
			}
			imgui.SetNextWindowPosV(
				imgui.Vec2{X: posX, Y: posY},
				imgui.CondFirstUseEver, imgui.Vec2{X: 0, Y: 0})
			sizeCond := imgui.CondFirstUseEver
			if win.pendingResize {
				sizeCond = imgui.CondAlways
				win.pendingResize = false
			}
			imgui.SetNextWindowSizeV(
				imgui.Vec2{X: float32(win.width), Y: float32(win.height)},
				sizeCond)
			// NoDocking: don't merge into the main viewport.
			// NoSavedSettings: don't pollute imgui.ini with per-
			//   window geometry; we manage that ourselves.
			// NoTitleBar / NoScrollbar / NoBackground / NoCollapse:
			//   the OS chrome (native title bar via the WindowClass
			//   override above) is the user-facing chrome; the
			//   ImGui window inside is just a transparent host
			//   for our terminal draw list.
			// NoMove: with NoTitleBar set, ImGui makes the entire window
			// drag-to-move. That makes clicks on the scrollbar (or any
			// terminal cell) start a window move. The OS title bar
			// (from the WindowClass viewport-flag override above)
			// handles window movement; the wrapper must not.
			flags := imgui.WindowFlagsNoDocking |
				imgui.WindowFlagsNoSavedSettings |
				imgui.WindowFlagsNoTitleBar |
				imgui.WindowFlagsNoMove |
				imgui.WindowFlagsNoResize |
				imgui.WindowFlagsNoScrollbar |
				imgui.WindowFlagsNoScrollWithMouse |
				imgui.WindowFlagsNoBackground |
				imgui.WindowFlagsNoCollapse |
				imgui.WindowFlagsNoBringToFrontOnFocus
			// Strip the wrapper's own padding / border so it
			// occupies exactly the OS window's content rect with
			// no offset. Without these, ImGui's default 8px
			// WindowPadding + 1px WindowBorderSize push the
			// rendering subtly inside the wrapper, which makes
			// the tab bar (positioned at viewport.Pos) appear to
			// overlap the terminal's first row by a few px.
			imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, imgui.Vec2{X: 0, Y: 0})
			imgui.PushStyleVarFloat(imgui.StyleVarWindowBorderSize, 0)
			// "<displayTitle>###<stableID>" — text after ### is the
			// stable ImGui ID, text before is the display name (which
			// becomes the OS NSWindow title via ImGui's
			// Platform_SetWindowTitle callback). So the OS title
			// tracks the active tab without invalidating the ImGui
			// window's identity / saved state.
			beginName := win.titleForWindow() + "###" + win.imguiName
			// Belt-and-suspenders alongside the post-loop
			// platform.RaiseWindow: when focus has been requested for
			// this Window, ask ImGui to focus it during this Begin.
			// ImGui's platform layer translates that into the OS focus
			// call for the popped-out viewport, which on macOS
			// pre-flights the makeKeyAndOrderFront: that RaiseWindow
			// does later.
			if win.pendingFocus {
				imgui.SetNextWindowFocus()
			}
			shouldRenderFrame := imgui.BeginV(beginName, nil, flags)
			// Pop the wrapper-only styles IMMEDIATELY after Begin so
			// they only affect the wrapper window (its content rect
			// is set by ImGui from the at-Begin-time style). Without
			// this early pop, the styles stay on the stack through
			// the entire frame body, and any popup/menu/dialog
			// opened inside frame() inherits WindowPadding=0 — making
			// the right-click context menu items have zero padding.
			imgui.PopStyleVarV(2)
			if shouldRenderFrame {
				win.imViewport = imgui.WindowViewport()
				// Capture the wrapper's actual content origin (NOT
				// viewport.Pos — see contentOriginY comment on
				// Window). CursorScreenPos right after Begin is the
				// authoritative top-left of the area we're allowed
				// to draw into.
				cp := imgui.CursorScreenPos()
				win.contentOriginX = cp.X
				win.contentOriginY = cp.Y
				// RootAndChildWindows so a click on the tab bar /
				// search overlay (separate ImGui windows nested
				// inside the wrapper's frame()) still counts as
				// focusing this Window — otherwise tab-bar
				// interaction would drop input gating.
				if imgui.IsWindowFocusedV(imgui.FocusedFlagsRootAndChildWindows) {
					focused = win
				}
				win.frame()
			}
			imgui.End()
			// Use viewport.PlatformRequestClose to detect the OS
			// close button — without a TitleBar the `&open` bool
			// passed to BeginV doesn't reliably propagate the close
			// (no Begin-internal close button). PlatformRequestClose
			// is set by ImGui_ImplSDL2 directly from
			// SDL_WINDOWEVENT_CLOSE so it's the source of truth.
			if win.imViewport != nil && win.imViewport.PlatformRequestClose() {
				win.imViewport.SetPlatformRequestClose(false)
				if win.swallowOSCloseFrames > 0 {
					// Suppressed: our close_tab keybind just fired
					// and the OS-level Cmd+W close event is the
					// echo of that same keystroke. See the
					// swallowOSCloseFrames doc on Window for the
					// macOS Cmd+W double-fire rationale.
				} else {
					win.pendingClose = true
				}
			}
			if win.swallowOSCloseFrames > 0 {
				win.swallowOSCloseFrames--
			}
		}
		if focused != nil {
			a.active = focused
		}
		// Override the focus-from-ImGui result if focus was explicitly
		// requested for a Window (new Window spawn, or focus returning
		// from an auxiliary viewport such as preferences). Two timing
		// issues to handle here:
		//
		//   1. ImGui's UpdatePlatformWindows runs in platform_end_frame
		//      (i.e. AFTER wrappedFrame). So on the FIRST frame after
		//      spawnWindow, the new Window's imViewport exists but its
		//      PlatformHandle is still 0 — the SDL_Window hasn't been
		//      created yet. We can't raise a window that doesn't exist.
		//      Wait for the next frame, when PlatformHandle is non-zero.
		//   2. ImGui's IsWindowFocused trails the OS keyboard focus by
		//      a frame or two on macOS. Without overriding here, the
		//      spawning Window stays a.active and the next keybind
		//      (Cmd+T immediately after Cmd+N) dispatches there
		//      instead of the new Window.
		//
		// So we keep pendingFocus set until we can BOTH raise the new
		// SDL_Window AND mark a.active. Clearing it only on successful
		// raise also handles the "spawn but viewport never gets a
		// handle" failure case gracefully.
		for _, win := range a.windows {
			if !win.pendingFocus {
				continue
			}
			if win.imViewport == nil {
				continue
			}
			handle := win.imViewport.PlatformHandle()
			if handle == 0 {
				// Viewport's SDL_Window not yet created — try next frame.
				a.active = win
				focused = win
				continue
			}
			a.active = win
			focused = win
			// SDL_RaiseWindow alone leaves macOS with deferred
			// makeKey: until the next event tick — i.e. the user has
			// to move the mouse before keybinds route to the new
			// Window. CocoaFocusWindow does NSApp.activate +
			// NSWindow.makeKeyAndOrderFront: directly so firstResponder
			// transitions synchronously, this frame.
			platform.RaiseWindow(handle)
			platform.CocoaFocusWindow(handle)
			// macOS drops modifier KEY_UP events during NSWindow
			// focus transitions, so a Cmd held through Cmd+N
			// → Cmd+T leaves ImGui thinking Cmd was released —
			// breaking the immediately-following Cmd+T keybind.
			// Re-assert the actual OS modifier state.
			platform.ResyncModifiers()
			win.pendingFocus = false
			break
		}
		if a.active != nil && a.active.pendingClose {
			// Active Window is being closed this frame — pick
			// another so input has somewhere to land for the
			// rest of the frame body, and pull OS focus to it so
			// keybinds in the remaining Window work immediately
			// (without this, macOS leaves keyboard focus on the
			// dying window's prior peer and the user has to click
			// the survivor before keys route to it).
			a.active = nil
			if win := a.bestFocusReplacement(); win != nil {
				a.active = win
				focused = win
				if win.imViewport != nil {
					if h := win.imViewport.PlatformHandle(); h != 0 {
						platform.RaiseWindow(h)
						platform.CocoaFocusWindow(h)
						platform.ResyncModifiers()
					}
				}
			}
		}
		// Process any closes deferred during the render pass. If
		// every Window is gone after the reap, the process quits.
		a.reapClosedWindows()
		if len(a.windows) == 0 {
			platform.Quit()
		}
		a.updateTabDragDrop()
	}
	installLiveResizeWatch(bgR, bgG, bgB, wrappedFrame, a.beforeRender)

	// Wake the platform loop when any PTY produces new data. Without
	// this the loop sleeps in WaitEventTimeout until the next timeout
	// (cursor blink etc.) and the user sees up to ~500ms latency on
	// incoming bytes.
	terminal.Wake = platform.PostWake

	stopMouseWakePoller := a.startMouseMirrorWakePoller()
	defer stopMouseWakePoller()

	// Main loop — event-driven via SDL_WaitEventTimeout inside
	// platform.Frame. CPU goes to kernel-sleep when nothing's
	// happening; PTY arrival, mouse, keys, timers all wake it.
	// beforeRender was registered above via platform.SetBeforeRenderHook
	// and fires automatically before each NewFrame inside Frame().
	for platform.Frame(wrappedFrame) {
	}
	platform.Shutdown()

	// Cleanup: close all tabs in every Window before exiting.
	for _, win := range a.windows {
		if win.tabs == nil {
			continue
		}
		for _, tab := range win.tabs.Tabs {
			tab.Terminal.Close()
		}
	}

	return nil
}

// cellSafetyMarginH / cellSafetyMarginV reserve extra pixels of gutter
// between the cell grid and the window edge, beyond the user-visible
// Appearance.Padding. Two reasons:
//
//  1. Glyph AA spill: CalcTextSize returns the font's advance width, but
//     font hinting / anti-aliasing can nudge glyph edge pixels slightly
//     past cellW. Without margin, when the floor-fitted cell count
//     produces a tight right-edge gutter, the rightmost glyph's AA edge
//     renders over the window boundary and appears clipped.
//
//  2. macOS rounded NSWindow corners (~5 px radius on all four corners)
//     mask any content rendering at the very edge. The bottom row's
//     leftmost/rightmost glyphs and the top row's corners get clipped
//     if cells run flush to the window bounds.
//
// Together they translate to "more padding on macOS than other
// platforms" — same reason iTerm uses noticeable inset.
//
// Must be added BACK to the window size in any code that wants to fit a
// specific cols×rows grid (e.g. font zoom resize), otherwise the next
// gridSize call will floor away one cell.
var (
	cellSafetyMarginH = func() float32 {
		if runtime.GOOS == "darwin" {
			return 12 // 8 for glyph AA + 4 for right corner radius
		}
		return 8
	}()
	cellSafetyMarginV = func() float32 {
		if runtime.GOOS == "darwin" {
			return 6 // covers bottom corner radius
		}
		return 0
	}()
	// cellOriginInsetX is added to OffsetX on macOS so the LEFT column
	// also clears the rounded window corner. cellSafetyMarginH alone
	// only reserves right-edge gutter; without this, the bottom-left
	// glyph's lower pixels get clipped by the corner mask.
	cellOriginInsetX = func() float32 {
		if runtime.GOOS == "darwin" {
			return 4
		}
		return 0
	}()
)

func (w *Window) gridSize() (cols, rows int) {
	pad := float32(w.app.cfg.Appearance.Padding) * 2 // padding on both sides
	availW := float32(w.width) - pad - cellSafetyMarginH - cellOriginInsetX
	availH := float32(w.height) - w.tabBarH - pad - cellSafetyMarginV
	cols = int(availW / w.cellW)
	rows = int(availH / w.cellH)
	if cols < 2 {
		cols = 2
	}
	if rows < 2 {
		rows = 2
	}
	return
}

// measureCell returns the cell width/height for the current font. When
// the OS-backed glyph cache is active, it uses the cache's primary-only
// metrics — that's just the user's chosen monospace font, no influence
// from any merged fallback. Falls back to ImGui's atlas-based
// MeasureCell when no cache is available (Linux until fontsys is
// implemented there).
//
// Cell height is ascent + descent (no leading) — terminals traditionally
// pack rows tightly. Cell width is the primary font's M advance.
//
// Returns the FLOAT advance straight from the font. Callers ceil
// `baseCellW * scale` (or `baseCellW * 1` at base zoom) when storing
// into the layout `cellW`. Storing baseCellW pre-ceil is what makes
// font-zoom scale linearly: at 1.0833× zoom of a 7.2 px advance you
// want ceil(7.8)=8, not ceil(8 * 1.0833)=10. (The latter is what we
// get if ceil happens at measure time and then again after scaling —
// the ceiling compounds and cells drift wider than the font wants on
// every zoom step.)
func (w *Window) measureCell() renderer.CellMetrics {
	if w.renderer != nil && w.renderer.Glyphs != nil {
		lm := w.renderer.Glyphs.LineMetrics()
		adv := w.renderer.Glyphs.PrimaryAdvance()
		h := lm.Ascent + lm.Descent
		if adv > 0 && h > 0 {
			return renderer.CellMetrics{Width: adv, Height: h}
		}
	}
	return renderer.MeasureCell()
}

// ceilCell ceils the cell metrics to whole logical pixels for the
// layout `cellW`/`cellH`. AppKit's setContentResizeIncrements rounds
// to integer points and gridSize()'s int(W/cellW) needs to match;
// integer cellW makes both deterministic. Ceil (vs round) keeps the
// cell at least as wide/tall as the font wants — glyphs never clip,
// box-drawing rects tile with no gaps, wide chars never overlap.
func ceilCell(w, h float32) (float32, float32) {
	return float32(math.Ceil(float64(w))), float32(math.Ceil(float64(h)))
}

func (w *Window) resizeTerminals() {
	cols, rows := w.gridSize()
	for _, tab := range w.tabs.Tabs {
		tab.Terminal.Resize(cols, rows)
	}
}

// beforeRender runs before every NewFrame — both via cimgui-go's
// SetBeforeRenderHook in the main loop and via the macOS live-resize
// watch in liveresize_darwin.go. Has to live BEFORE NewFrame because
// font reloads invalidate ImGui's per-frame font pointer; doing the
// reload mid-frame would assert in Render.
//
// Font reloads apply process-wide: every Window picks up the new
// font, glyph cache, and metrics. Each Window's renderer keeps its
// own *renderer.Renderer instance (so per-window theming stays
// possible later) but they all share the new Font/FontBold and a
// freshly-built glyph cache.
func (a *App) beforeRender() {
	// Resync modifier state every frame BEFORE imgui.NewFrame consumes
	// the input queue. macOS drops phantom KEY_UPs for held modifiers
	// during NSWindow focus transitions; SDL's modifier view inherits
	// that lie. NSEvent.modifierFlags reads the IOHID hardware state,
	// which stays correct across transitions. Done here (not in
	// wrappedFrame) because AddKeyEvent queues for the NEXT NewFrame
	// — calling it after NewFrame means the key edge for the current
	// frame's KEY_DOWN still sees the stale modifier and the keybind
	// match fails.
	platform.ResyncModifiers()

	if !a.pendingFontFace {
		return
	}
	a.pendingFontFace = false
	font, fontBold, _ := renderer.ReloadFont(&a.cfg)
	a.baseFontSize = renderer.PixelSize(&a.cfg)
	for _, w := range a.windows {
		if w.renderer != nil {
			w.renderer.Font = font
			w.renderer.FontBold = fontBold
			if w.renderer.Glyphs != nil {
				w.renderer.Glyphs.Close()
				w.renderer.Glyphs = nil
			}
		}
	}
	if fontsys.Default != nil {
		primaryPath := renderer.ResolveFontPath(&a.cfg)
		if primaryPath != "" {
			fbScale := imgui.CurrentIO().DisplayFramebufferScale().X
			if fbScale <= 0 {
				fbScale = 1
			}
			for _, w := range a.windows {
				if w.renderer == nil {
					continue
				}
				// Each Window has its own font size — rebuild the
				// cache at the Window's current zoom, not the
				// app-level base.
				size := w.fontSize
				if size <= 0 {
					size = a.baseFontSize
				}
				if c, err := glyphcache.New(fontsys.Default, platform.Textures(), primaryPath, size, fbScale); err == nil {
					w.renderer.Glyphs = c
				}
			}
		}
	}
	// Each Window measures cells in its own frame() — every Window can
	// be at a different font zoom, so a single shared "re-measure now"
	// flag would be consumed by whichever Window framed first and
	// stamp those metrics as if they applied everywhere. Per-Window
	// flags let each Window run its own measureCell() against its own
	// glyph cache; the first to consume also sets the app-wide
	// baseCellW/H (scaled back from its local fontSize).
	for _, w := range a.windows {
		w.pendingRemeasure = true
	}
}

// updateTabDragDrop advances and finalizes an in-flight cross-window tab drag.
// It runs once per app frame after all Windows rendered, so drop resolution is
// deterministic instead of whichever Window happens to frame first.
func (a *App) updateTabDragDrop() {
	d := a.dragTab
	if d == nil {
		return
	}
	// Update last-seen focus while drag is in flight. We do this in
	// every frame, but don't rely on a source-window match by itself:
	// Wayland's implicit pointer grab can keep reporting the source
	// surface even after the cursor is physically over another window.
	if cur := platform.MouseFocusWindowID(); cur != 0 {
		d.LastFocus = cur
	}
	a.refreshWaylandTabDrop(d)

	mouseDown := imgui.IsMouseDown(imgui.MouseButtonLeft)
	if d.WaylandStarted && (d.WaylandDropSeen || !platform.WaylandDragActive()) {
		mouseDown = false
	}
	if mouseDown {
		return
	}

	target, wait := a.tabDropTarget(d)
	if wait {
		return
	}
	if target != nil {
		target.adoptDraggedTab(d)
		a.dragTab = nil
		return
	}

	a.spawnWindowAdopting(d.Term)
	a.dragTab = nil
}

func (a *App) tabDropTarget(d *tabDrag) (*Window, bool) {
	a.refreshWaylandTabDrop(d)
	if d.WaylandStarted && !d.WaylandDropSeen {
		if target := platform.WaylandDropTarget(); target != 0 && !platform.WaylandDragActive() {
			d.WaylandDropSeen = true
			d.WaylandDropSurface = target
		} else if platform.WaylandDragActive() {
			return nil, true
		}
	}

	if d.WaylandDropSeen {
		if d.WaylandDropSurface == 0 {
			return nil, false
		}
		for _, w := range a.windows {
			if platform.WindowWLSurfacePtr(w.sdlWindowHandle()) == d.WaylandDropSurface {
				return w, false
			}
		}
		return nil, false
	}

	mp := imgui.MousePos()
	mouseValid := validMousePos(mp)
	if mouseValid && platform.VideoDriver() != "wayland" {
		for i := len(a.windows) - 1; i >= 0; i-- {
			if a.windows[i].containsMousePos(mp) {
				return a.windows[i], false
			}
		}
	}

	if d.LastFocus != 0 {
		for _, w := range a.windows {
			if w.sdlWindowHandle() != d.LastFocus {
				continue
			}
			if w == d.From && (!mouseValid || !w.containsMousePos(mp)) {
				return nil, false
			}
			return w, false
		}
	}

	if mouseValid {
		for i := len(a.windows) - 1; i >= 0; i-- {
			if a.windows[i].containsMousePos(mp) {
				return a.windows[i], false
			}
		}
	}

	return nil, false
}

func (a *App) refreshWaylandTabDrop(d *tabDrag) {
	if !d.WaylandStarted || d.WaylandDropSeen {
		return
	}
	if platform.WaylandDropFired() {
		d.WaylandDropSeen = true
		d.WaylandDropSurface = platform.WaylandDropTarget()
	}
}

func (w *Window) adoptDraggedTab(d *tabDrag) {
	cols, rows := w.gridSize()
	if cols > 1 && rows > 1 {
		d.Term.Resize(cols, rows)
	}
	w.tabs.AdoptTab(d.Term)
}

func validMousePos(mp imgui.Vec2) bool {
	const noMouseSentinel = -3.4e+37
	return mp.X > noMouseSentinel && mp.Y > noMouseSentinel
}

func (w *Window) containsMousePos(mp imgui.Vec2) bool {
	return validMousePos(mp) &&
		mp.X >= w.contentOriginX &&
		mp.X < w.contentOriginX+float32(w.width) &&
		mp.Y >= w.contentOriginY &&
		mp.Y < w.contentOriginY+float32(w.height)
}

func (a *Window) frame() {
	// macOS: after the first click that shifts the Cocoa first-responder,
	// SDL2 stops receiving subsequent mouse-button NSEvents — neither
	// presses nor releases reach the SDL event queue, so ImGui sees no
	// up→down transitions and tab clicks vanish. Bypass the broken event
	// path by polling the OS-level mouse-button state directly each frame
	// and injecting synthetic events into ImGui whenever its view of the
	// button diverges from reality.
	//
	// Asymmetric: only inject DOWN when the cursor is inside the main
	// window's content rect AND we're not in a live-resize-driven frame.
	// AppKit consumes clicks on window frames, resize handles, and
	// popped-out viewport title bars without delivering them to SDL —
	// without these guards the OS button-down poll would manufacture a
	// fake terminal click out of the window-management gesture and
	// start a phantom selection drag. Releases always inject so a real
	// drag-then-release that ends outside content still clears state.
	// Mouse mirror only runs for the active Window — the underlying
	// ImGui IO is global per-context, so injecting events here once
	// per active frame is correct; injecting from every Window per
	// frame would fire the same mouse-down event N times.
	if runtime.GOOS == "darwin" && a == a.app.active && !disableMirror {
		osDown := sdlhack.LeftButtonPhysicalDown()
		// Transition-based mirror: inject events on every OS button
		// edge, NOT just when ImGui's view diverges from OS state.
		// Diffing against ImGui's view (the previous approach) misses
		// edges when ImGui happens to already have the right state
		// from a partial event delivery — but the click never registers
		// as "fresh" because MouseClicked needs a transition. Injecting
		// on every real OS transition guarantees ImGui sees the
		// transition even if a real event also got through.
		// macOS window-drag commit takes ~100-200ms. The delay
		// only affects SDL-dropped clicks; SDL-delivered clicks
		// skip the mirror entirely.
		const mirrorDeferFrames = 15
		// Cursor motion during defer means this is a drag gesture
		// (window drag or selection drag). Both should abort the
		// synthetic DOWN.
		const mirrorAbortMotionPx = 6
		dbg := os.Getenv("XEROTTY_DEBUG_MIRROR") != ""
		imguiSeesDown := imgui.IsMouseDown(imgui.MouseButtonLeft)
		pendingCursorMoved := func() (bool, int, int) {
			gx, gy := sdlhack.GlobalMousePos()
			dx := gx - a.app.mirrorPendingGlobalX
			dy := gy - a.app.mirrorPendingGlobalY
			return dx*dx+dy*dy >= mirrorAbortMotionPx*mirrorAbortMotionPx, dx, dy
		}
		abortPendingReason := func() string {
			if a.app.anyWindowMovingThisFrame {
				return "window moved"
			}
			if moved, dx, dy := pendingCursorMoved(); moved {
				return fmt.Sprintf("cursor moved by %d,%d", dx, dy)
			}
			if platform.CocoaEventOnChrome() {
				return "cursor on chrome"
			}
			if over, _ := a.app.mouseOverOwnedContentPos(); !over {
				return "cursor left owned content"
			}
			return ""
		}
		switch {
		case osDown && !a.app.prevOsLeftDown:
			// OS-level press edge. If ImGui already sees the button
			// down, SDL delivered the click through the normal path
			// (contentView) — the mirror has nothing to compensate
			// for and any injection here would be a duplicate edge
			// (ImGui's double-click detector then fires word-select,
			// which the user sees as "click highlights stuff").
			//
			// Only defer-and-inject when SDL DIDN'T deliver — that's
			// either a real-click-that-SDL-dropped (mirror's
			// original purpose) or a window-drag gesture (which we
			// detect via motion during the defer window and abort).
			if imguiSeesDown {
				if dbg {
					fmt.Fprintf(os.Stderr, "[mirror] DOWN edge: SDL already delivered -> skip\n")
				}
				break
			}
			over, inViewportPos := a.app.mouseOverOwnedContentPos()
			onChrome := platform.CocoaEventOnChrome()
			if !inLiveResizeWatch() && over && !onChrome {
				gx, gy := sdlhack.GlobalMousePos()
				a.app.mirrorPendingDown = true
				a.app.mirrorPendingPos = inViewportPos
				a.app.mirrorPendingGlobalX = gx
				a.app.mirrorPendingGlobalY = gy
				a.app.mirrorPendingFramesLeft = mirrorDeferFrames
				if dbg {
					fmt.Fprintf(os.Stderr,
						"[mirror] DOWN edge: SDL dropped, over=%v onChrome=%v -> PENDING (%d frames)\n",
						over, onChrome, mirrorDeferFrames)
				}
			} else if dbg {
				fmt.Fprintf(os.Stderr,
					"[mirror] DOWN edge: SDL dropped + over=%v onChrome=%v inLR=%v -> skip\n",
					over, onChrome, inLiveResizeWatch())
			}
		case !osDown && a.app.prevOsLeftDown:
			// OS-level release edge.
			if a.app.mirrorPendingDown {
				// Fast release before the defer window expired —
				// either a real-click-that-SDL-dropped (inject so
				// terminal sees a click) or a window-drag tap (no
				// inject; abort). Run the same abort checks here as
				// the deferred path below; otherwise a quick release
				// can inject DOWN+UP before the pending abort block
				// gets a chance to see cursor/window movement.
				if reason := abortPendingReason(); reason != "" {
					if dbg {
						fmt.Fprintf(os.Stderr, "[mirror] UP edge while pending: skip (%s)\n", reason)
					}
				} else if imguiSeesDown {
					imgui.CurrentIO().AddMouseButtonEvent(int32(imgui.MouseButtonLeft), false)
					if dbg {
						fmt.Fprintf(os.Stderr, "[mirror] UP edge while pending: SDL caught DOWN, inject UP\n")
					}
				} else {
					mp := imgui.MousePos()
					const noMouseSentinel = -3.4e+37
					if mp.X <= noMouseSentinel {
						imgui.CurrentIO().AddMousePosEvent(a.app.mirrorPendingPos.X, a.app.mirrorPendingPos.Y)
					}
					imgui.CurrentIO().AddMouseButtonEvent(int32(imgui.MouseButtonLeft), true)
					imgui.CurrentIO().AddMouseButtonEvent(int32(imgui.MouseButtonLeft), false)
					if dbg {
						fmt.Fprintf(os.Stderr, "[mirror] UP edge while pending: INJECT DOWN+UP\n")
					}
				}
				a.app.mirrorPendingDown = false
			} else if imguiSeesDown {
				// ImGui still thinks the button is down → SDL
				// dropped the UP. Inject so the drag/click clears.
				imgui.CurrentIO().AddMouseButtonEvent(int32(imgui.MouseButtonLeft), false)
				if dbg {
					fmt.Fprintf(os.Stderr, "[mirror] UP edge: SDL dropped UP -> INJECT\n")
				}
			} else if dbg {
				fmt.Fprintf(os.Stderr, "[mirror] UP edge: SDL already delivered -> skip\n")
			}
		}
		// Process the deferred DOWN. Abort conditions:
		//   1. Any window started moving → window-drag gesture.
		//   2. Cursor moved ≥mirrorAbortMotionPx → drag gesture
		//      (window drag manifests as cursor motion BEFORE the
		//      NSWindow.frame.origin updates, so this catches it
		//      earlier than the window-moved check).
		//   3. Cursor is now on chrome or outside our owned content.
		//   4. SDL caught up and delivered DOWN to ImGui → the
		//      mirror is no longer needed; injecting now would
		//      double-up the edge and trigger word-select via
		//      ImGui's double-click detector.
		if a.app.mirrorPendingDown {
			mp := imgui.MousePos()
			const noMouseSentinel = -3.4e+37

			if reason := abortPendingReason(); reason != "" {
				if dbg {
					fmt.Fprintf(os.Stderr, "[mirror]   pending DOWN aborted (%s)\n", reason)
				}
				a.app.mirrorPendingDown = false
			} else if imguiSeesDown {
				if dbg {
					fmt.Fprintf(os.Stderr, "[mirror]   pending DOWN aborted (SDL caught up)\n")
				}
				a.app.mirrorPendingDown = false
			} else {
				a.app.mirrorPendingFramesLeft--
				if a.app.mirrorPendingFramesLeft <= 0 {
					if mp.X <= noMouseSentinel {
						imgui.CurrentIO().AddMousePosEvent(a.app.mirrorPendingPos.X, a.app.mirrorPendingPos.Y)
					}
					imgui.CurrentIO().AddMouseButtonEvent(int32(imgui.MouseButtonLeft), true)
					a.app.mirrorPendingDown = false
					if dbg {
						fmt.Fprintf(os.Stderr, "[mirror]   pending DOWN: INJECT (timer expired)\n")
					}
				}
			}
		}
		a.app.prevOsLeftDown = osDown
	}

	// First frame: measure font metrics and create terminal
	if !a.ready {
		a.ready = true

		// Build the OS-backed glyph cache now that ImGui's NewFrame has
		// populated DisplayFramebufferScale. Doing this earlier (in
		// Run() before the loop starts) gives a stale fbScale of 1 on
		// Retina, so glyphs would rasterize at half the physical
		// pixel size and look chunky until the user changed font in
		// prefs (which rebuilds the cache when fbScale is correct).
		if a.renderer.Glyphs == nil && fontsys.Default != nil {
			primaryPath := renderer.ResolveFontPath(&a.app.cfg)
			if primaryPath != "" {
				fbScale := imgui.CurrentIO().DisplayFramebufferScale().X
				if fbScale <= 0 {
					fbScale = 1
				}
				if c, err := glyphcache.New(fontsys.Default, platform.Textures(), primaryPath, a.app.baseFontSize, fbScale); err == nil {
					a.renderer.Glyphs = c
				}
			}
		}

		// Measure real cell dimensions now that the font atlas is built.
		// metrics carries the float advance straight from the font;
		// baseCellW/H stores it pre-ceil so font-zoom can scale it
		// linearly, layout cellW/H is the ceil'd integer used for the
		// grid + OS resize-snap.
		metrics := a.measureCell()
		if metrics.Width < 1 || metrics.Height < 1 {
			// Fallback if measurement fails — estimate from atlas pixel size
			px := renderer.PixelSize(&a.app.cfg)
			metrics = renderer.CellMetrics{Width: px * 0.6, Height: px * 1.2}
		}
		a.app.baseCellW = metrics.Width
		a.app.baseCellH = metrics.Height
		a.cellW, a.cellH = ceilCell(metrics.Width, metrics.Height)
		a.renderer.Metrics = renderer.CellMetrics{Width: a.cellW, Height: a.cellH}
		// (Resize-increment push happens lower, at the end of
		// frame() — by then the SDL_Window handle is valid on
		// repeat frames; this first-frame call would otherwise
		// hit SDL_GetWindowFromID(0) and no-op.)

		// Re-fit the window to the configured columns/rows now that we have
		// real cell metrics. The initial CreateWindow used estimated metrics,
		// so the actual window may be a few pixels off in each direction.
		cfgCols, cfgRows := a.app.cfg.Window.Columns, a.app.cfg.Window.Rows
		if cfgCols < 2 {
			cfgCols = 80
		}
		if cfgRows < 2 {
			cfgRows = 24
		}
		pad := float32(a.app.cfg.Appearance.Padding) * 2
		// Add cellSafetyMargin so gridSize() computes back to cfgCols/cfgRows.
		desiredW := int(math.Ceil(float64(float32(cfgCols)*a.cellW + pad + cellSafetyMarginH + cellOriginInsetX)))
		desiredH := int(math.Ceil(float64(float32(cfgRows)*a.cellH + pad + a.tabBarH + cellSafetyMarginV)))
		if desiredW != a.width || desiredH != a.height {
			// Every Window's OS geometry is driven by ImGui multi-
			// viewport. Set width/height + pendingResize so the
			// Begin in wrappedFrame uses CondAlways next frame.
			a.width = desiredW
			a.height = desiredH
			a.pendingResize = true
			a.skipDisplaySync = 2
		}

		// First-frame startup tab — no parent to inherit CWD from;
		// the shell uses xerotty's own CWD (launcher / cwd-at-launch).
		if _, err := a.tabs.NewTab(cfgCols, cfgRows, ""); err != nil {
			platform.Quit()
			return
		}
		return
	}

	// Font-face swap is handled in the backend's BeforeRender hook so
	// it happens BEFORE NewFrame, never mid-frame. Doing it mid-frame
	// (the way we used to) corrupts ImGui's per-frame state — at
	// NewFrame time it captured the OLD font pointer, then our user
	// code Clear()s the atlas and frees that font, then EndFrame /
	// Render dereferences the dangling pointer and asserts. Hook is
	// installed in Run() right after the backend is created.

	// Re-measure cell metrics after a font face swap (atlas was rebuilt).
	// Each Window does its own measurement against its own glyph cache
	// because Windows can be at different zoom levels — if this Window
	// is zoomed 2x, metrics.Width is 2x what baseCellW should be, so
	// divide by this Window's scale before stamping the app-wide base.
	if a.pendingRemeasure {
		a.pendingRemeasure = false
		if metrics := a.measureCell(); metrics.Width >= 1 && metrics.Height >= 1 {
			scale := float32(1)
			if a.app.baseFontSize > 0 && a.fontSize > 0 {
				scale = a.fontSize / a.app.baseFontSize
			}
			if scale > 0 {
				a.app.baseCellW = metrics.Width / scale
				a.app.baseCellH = metrics.Height / scale
			}
			a.cellW, a.cellH = ceilCell(metrics.Width, metrics.Height)
			a.renderer.Metrics = renderer.CellMetrics{Width: a.cellW, Height: a.cellH}
			a.renderer.FontSize = a.fontSize
			a.resizeTerminals()
			// (Resize-increment refresh handled at end of frame() —
			// same retry-until-handle-valid path used by first-frame
			// init.)
		}
	}

	// Sync window dimensions from ImGui IO every frame — more reliable than
	// SetSizeChangeCallback which some WMs/compositors don't always trigger.
	// Skip for a few frames after we issue SetWindowSize: DisplaySize lags the
	// WM by 1-2 frames, so a fresh shrink request would otherwise be clobbered
	// by the stale (pre-resize) DisplaySize value.
	if a.skipDisplaySync > 0 {
		a.skipDisplaySync--
	} else {
		// Pull size from this Window's viewport: main Window reads
		// io.DisplaySize (which is just MainViewport().Size()) — but
		// secondary Windows have their own popped-out viewport whose
		// Size() reflects the user's resizing of THAT OS window,
		// independent of the main.
		var dx, dy float32
		if vp := a.viewport(); vp != nil {
			sz := vp.Size()
			dx, dy = sz.X, sz.Y
		}
		if int(dx) > 0 && int(dy) > 0 {
			newW, newH := int(dx), int(dy)
			if newW != a.width || newH != a.height {
				a.width = newW
				a.height = newH
				a.resizeTerminals()
				a.resizeTime = imgui.Time()
				a.resizeOverlay = true
				a.resizeOverlayText = "" // drag-resize: live cols×rows
			}
		}
	}

	// Handle scroll wheel: tab bar = switch tabs, Ctrl+scroll = zoom, plain scroll = scrollback
	// Geometric scope: only the Window whose content rect contains the
	// cursor consumes the wheel. Using a.app.active fails the same way
	// the selection / right-click gates did on Wayland multi-viewport
	// — focus tracking is unreliable so the wheel would silently no-op.
	mpW := imgui.MousePos()
	wheelInThisWindow := mpW.X >= a.contentOriginX &&
		mpW.X < a.contentOriginX+float32(a.width) &&
		mpW.Y >= a.contentOriginY &&
		mpW.Y < a.contentOriginY+float32(a.height)
	wheel := imgui.CurrentIO().MouseWheel()
	if wheel != 0 && wheelInThisWindow {
		vpOffY := a.contentOriginY
		if a.tabBarH > 0 && imgui.MousePos().Y-vpOffY < a.tabBarH {
			// Mouse over tab bar — switch tabs
			if wheel > 0 {
				a.tabs.Prev()
			} else {
				a.tabs.Next()
			}
			if tab := a.tabs.Active(); tab != nil {
				a.tabSwitchReq = tab.ID
			}
		} else if imgui.IsKeyDown(imgui.ModCtrl) {
			// Ctrl+wheel zoom — per-window, mirrors Cmd+= / Cmd+-.
			if wheel > 0 {
				a.fontSize += 1
				a.updateFontMetrics()
			} else if a.fontSize > 6 {
				a.fontSize -= 1
				a.updateFontMetrics()
			}
		} else if tab := a.tabs.Active(); tab != nil {
			s := a.getScroll(tab.ID)
			scrollLines := a.app.cfg.Scrollback.ScrollSpeed
			if scrollLines <= 0 {
				scrollLines = 3
			}
			if wheel > 0 {
				s.ScrollUp(scrollLines, tab.Terminal.ScrollbackLen())
			} else {
				s.ScrollDown(scrollLines)
			}
		}
	}

	// Mouse-selection and link hover are GEOMETRICALLY scoped — they
	// only fire when the cursor's actual position lands in this
	// Window's content rect. handleMouseSelection has its own
	// inTerminal check (computed from MousePos vs contentOrigin /
	// width / height) and link detection bails when (col, row) is
	// outside (cols, rows). No focus-based gate needed; in fact
	// gating on a.app.active broke selection entirely because
	// IsWindowFocusedV is unreliable under multi-viewport on
	// Wayland — wrapper windows often never register as focused,
	// so a.app.active never matched and selection / middle-click /
	// right-click all silently no-op'd.
	a.handleMouseSelection()

	// Detect links under mouse cursor. Gate the whole pass on
	// Links.Enabled — when off, hoveredLink stays nil so the underline
	// decoration in the renderer disappears and the click handlers
	// below are no-ops by virtue of `hoveredLink != nil` checks.
	a.hoveredLink = nil
	if a.app.cfg.Links.Enabled {
		if tab := a.tabs.Active(); tab != nil {
			mousePos := imgui.MousePos()
			col := int((mousePos.X - a.renderer.OffsetX) / a.cellW)
			row := int((mousePos.Y - a.renderer.OffsetY) / a.cellH)
			cols, rows := a.gridSize()
			if col >= 0 && col < cols && row >= 0 && row < rows {
				scrollOff := 0
				if s, ok := a.scroll[tab.ID]; ok {
					scrollOff = s.Offset
				}
				a.hoveredLink = detectLinkAt(tab.Terminal.Emulator(), col, row, scrollOff)

				// Ctrl+click opens link
				if a.hoveredLink != nil && a.app.cfg.Links.CtrlClick && imgui.IsKeyDown(imgui.ModCtrl) && imgui.IsMouseClickedBool(imgui.MouseButtonLeft) {
					openURL(a.hoveredLink.URL, a.app.cfg.Links.Opener)
				}
			}
		}
	}

	// Drain data notifications
	a.tabs.DrainData()

	// Scroll handling on new output:
	// - If at live position: stay at bottom (auto-scroll)
	// - If scrolled back:
	//     scroll_on_output=true  → snap to bottom (gnome-terminal default)
	//     scroll_on_output=false → freeze viewport by adjusting offset
	//                              for new scrollback lines (xterm/iTerm default)
	scrollOnOutput := a.app.cfg.Scrollback.ScrollOnOutput
	for _, tab := range a.tabs.Tabs {
		if tab.Dirty {
			tab.Dirty = false
			s := a.getScroll(tab.ID)
			sbLen := tab.Terminal.ScrollbackLen()
			if s.IsScrolled() && s.PrevSBLen > 0 {
				if scrollOnOutput {
					s.Reset()
				} else {
					delta := sbLen - s.PrevSBLen
					if delta > 0 {
						s.Offset += delta
					}
				}
			}
			s.PrevSBLen = sbLen
		}
	}

	// Check for closed tabs and handle on_child_exit policy
	a.tabs.CheckClosed()
	for i := len(a.tabs.Tabs) - 1; i >= 0; i-- {
		tab := a.tabs.Tabs[i]
		if !tab.Closed {
			continue
		}
		switch a.app.cfg.Tabs.OnChildExit {
		case "close":
			a.tabs.CloseTab(i)
		case "hold":
			// Keep tab open — user can close manually
		case "hold_on_error":
			if tab.Terminal.ChildExitCode() == 0 {
				a.tabs.CloseTab(i)
			}
			// Non-zero exit: keep tab open so user can see output
		default:
			a.tabs.CloseTab(i)
		}
	}

	// No tabs left in this Window — close just this Window. The reap
	// pass in wrappedFrame removes it; if it was the last Window the
	// process exits.
	if a.tabs.Count() == 0 {
		a.pendingClose = true
		// Clear ImGui's input key state so any key the user was
		// holding at close time (Ctrl-D / Enter / etc. — i.e. the
		// keystroke that caused the last tab to exit) doesn't stay
		// stuck "down" in ImGui's state. SDL3 drops KEYUP events
		// during the Cocoa focus shift that closing a window
		// triggers; without this clear, focus transfers to the
		// surviving Window with the key still registered as held,
		// and ImGui's auto-repeat fires it again and again — which
		// is how closing one window cascades into closing them all.
		imgui.CurrentIO().ClearInputKeys()
		return
	}

	// Process queued key events — only the active Window forwards
	// keystrokes to its PTY, so typing doesn't end up in N PTYs at
	// once.
	if a == a.app.active {
		a.processKeys()
	}

	// Update tab bar height based on tab count and current font size.
	// FrameHeight = FontSize + FramePadding.Y*2 — the visible tab item
	// rect. +2 covers the underline separator drawn just inside Max.y.
	// With inline tab bar, this is the EXACT bar height — no separate
	// window with extra internal padding.
	oldTabBarH := a.tabBarH
	if a.tabs.Count() > 1 {
		a.tabBarH = imgui.FrameHeight() + 2
	} else {
		a.tabBarH = 0
	}
	pad := float32(a.app.cfg.Appearance.Padding)
	// Use the wrapper's actual content origin (captured in
	// wrappedFrame). With inline tab bar, the BarRect ends at
	// contentOrigin.Y + FrameHeight; cells render `pad` px below
	// that, naturally tight against the visible tab bar.
	vpOffX, vpOffY := a.contentOriginX, a.contentOriginY
	// Snap render offsets to whole pixels so glyphs don't get sub-pixel-drifted
	// between rows. Without this, half-block characters (▀ ▄) and continuous
	// lines (─ │) can develop visible gaps between rows because their AA
	// rendering shifts at fractional positions.
	a.renderer.OffsetX = float32(math.Floor(float64(vpOffX + pad + cellOriginInsetX)))
	a.renderer.OffsetY = float32(math.Floor(float64(vpOffY + a.tabBarH + pad)))
	// When tab bar visibility changes (1↔2 tabs), grow/shrink the SDL window
	// vertically by the tab bar's height so the terminal grid keeps the same
	// rows. Without this, gridSize() loses ~tabBarH/cellH rows and the user
	// sees the terminal shrink (e.g. 80x24 → 80x22) instead of the window
	// expanding to accommodate the bar. Skip in fullscreen — the WM ignores
	// SetWindowSize there and we can't grow past the display.
	if a.tabBarH != oldTabBarH {
		if !a.fullscreen {
			delta := int(math.Ceil(float64(a.tabBarH - oldTabBarH)))
			if delta != 0 {
				newH := a.height + delta
				a.height = newH
				a.pendingResize = true
				a.skipDisplaySync = 2
			}
		}
		a.resizeTerminals()
	}

	// Render terminal cells FIRST into the wrapper's window drawlist,
	// then the tab bar items after. ImGui adds draw commands in call
	// order so the later commands (tab bar items, decorations) layer
	// on top of the earlier ones (terminal cells). Without this
	// ordering the cells would draw over the tab bar's bottom edge
	// and clip the first terminal row when both share a drawlist.
	if tab := a.tabs.Active(); tab != nil {
		drawList := a.bgDrawList()
		if drawList != nil {
			scrollOff := 0
			if s, ok := a.scroll[tab.ID]; ok {
				scrollOff = s.Offset
			}
			a.renderer.Draw(tab.Terminal, drawList, scrollOff)

			// Draw selection highlight
			if a.sel.active {
				r1, c1, r2, c2 := a.sel.normalize()
				cols, rows := a.gridSize()
				a.renderer.DrawSelection(renderer.SelectionBounds{
					Active:   true,
					StartRow: r1, StartCol: c1,
					EndRow: r2, EndCol: c2,
				}, cols, rows, drawList)
			}

			// Draw link underline on hover
			if a.hoveredLink != nil {
				lh := a.hoveredLink
				y := a.renderer.OffsetY + float32(lh.Row)*a.cellH + a.cellH - 1
				x1 := a.renderer.OffsetX + float32(lh.StartCol)*a.cellW
				x2 := a.renderer.OffsetX + float32(lh.EndCol+1)*a.cellW
				drawList.AddLine(
					imgui.Vec2{X: x1, Y: y},
					imgui.Vec2{X: x2, Y: y},
					a.renderer.Theme.Foreground,
				)
			}

			// Only show cursor when at live position (not scrolled back)
			if scrollOff == 0 {
				showCursor := true
				if a.app.cfg.Appearance.CursorBlink {
					rate := float64(a.app.cfg.Appearance.BlinkRate) / 1000.0
					if rate <= 0 {
						rate = 0.53
					}
					showCursor = int(imgui.Time()/rate)%2 == 0
				}
				if showCursor {
					pos := tab.Terminal.Emulator().CursorPosition()
					a.renderer.DrawCursor(struct{ X, Y int }{pos.X, pos.Y},
						a.app.cfg.Appearance.CursorStyle, drawList)
				}
			}

			// Search highlights — refresh matches each frame so PTY output
			// doesn't cause stale coordinates. Preserve MatchIdx so
			// navigation (< >) isn't clobbered by the per-frame re-search.
			if s, ok := a.scroll[tab.ID]; ok && s.Searching && s.Query != "" {
				_, visRows := a.gridSize()
				savedIdx := s.MatchIdx
				s.Search(tab.Terminal.Emulator(), visRows)
				if savedIdx < len(s.Matches) {
					s.MatchIdx = savedIdx
				}
				if len(s.Matches) > 0 {
					a.drawSearchHighlights(s, drawList)
				}
			}

			// Scrollbar
			a.drawScrollbar(tab, scrollOff, drawList)
		}
	}

	// Tab bar — rendered AFTER terminal cells so it visually layers
	// on top of them when they share the wrapper's drawlist.
	a.renderTabBar()

	// Search overlay
	a.renderSearchOverlay()

	// Context menu — open on right-click. We detect the click manually because
	// the terminal isn't an ImGui window (it renders into the main viewport's
	// BackgroundDrawList), so HoveredWindow is nil over the terminal area.
	//
	// That same lack-of-window breaks hover-while-held: when ImGui sees a
	// mouse-down whose HoveredWindow is nil and no popup is open yet, it
	// stamps MouseDownOwned[btn]=false for the click, which then forces
	// HoveredWindow back to nil every subsequent frame the button is held.
	// Items in the popup never highlight on hover, and at EndFrame the
	// MouseClicked[1] && HoveredId == 0 path runs ClosePopupsOverWindow and
	// shuts the menu we just opened.
	//
	// Reclaim ownership right after OpenPopup: flip MouseDownOwned to true
	// so the next frame keeps HoveredWindow tracking the popup, and pin a
	// non-zero HoveredID for this frame so the right-click-on-empty-space
	// close path skips us. Result: terminal-style hover preview while the
	// right button is held; a separate click still confirms.
	// Gate to the active Window: ImGui's IsMouseClicked / OpenPopup are
	// global, so without this every Window's frame() would see the click,
	// each call OpenPopupStr("##contextmenu") with the same ID (last write
	// wins), and the wrong Window's renderContextMenu would receive the
	// chosen action. Result: right-click "New Tab" in Window 1 actually
	// created a tab in the last-iterated Window. Same reason processKeys
	// gates on a.app.active.
	// Geometric scope: only the Window whose content rect contains the
	// cursor opens its context menu. (Focus-based gating fails on
	// Wayland multi-viewport — wrappers often don't register as
	// focused. Mouse-position is unambiguous: exactly one Window
	// contains the cursor at any moment.)
	//
	// We don't use ImGui's BeginPopup / OpenPopupStr machinery
	// because that ties the menu lifecycle to the host viewport's
	// focus — once the cursor crosses the terminal edge ImGui
	// considers the popup "closed by click-out" and snaps it shut,
	// even though the user is just moving toward the menu. Instead
	// just record the position; renderContextMenu draws via BeginV
	// and we control when it closes.
	mp := imgui.MousePos()
	mouseInThisWindow := mp.X >= a.contentOriginX &&
		mp.X < a.contentOriginX+float32(a.width) &&
		mp.Y >= a.contentOriginY &&
		mp.Y < a.contentOriginY+float32(a.height)
	// Only OPEN the menu via right-click; never re-open or reposition
	// while it's already open. If the user right-clicks the terminal
	// with the menu showing, the click counts as a click-outside and
	// dismisses the menu via menu.Render's close-on-click path. If we
	// re-set contextMenuOpenedFrame here, allowCloseClick would be
	// false for that frame and the menu would silently reposition
	// instead of closing.
	// Only the focused Window opens its menu — without this gate,
	// overlapping multi-viewport popout rects mean a right-click in
	// the overlap area fires "menu open" on BOTH windows (verified:
	// same mouse pos passes mouseInThisWindow for both), producing
	// duplicate menus and an ImGui ID conflict. We use IsWindowFocused
	// inline (not a.app.active, which is last-frame's value updated
	// post-render) so the *clicked* window is the one that responds.
	thisWindowFocused := imgui.IsWindowFocusedV(imgui.FocusedFlagsRootAndChildWindows)
	if thisWindowFocused && mouseInThisWindow && imgui.IsMouseClickedBool(imgui.MouseButtonRight) && !a.contextMenuOpen {
		a.contextMenuOpen = true
		a.contextMenuX = mp.X
		a.contextMenuY = mp.Y
		// Remember which frame we opened on — renderContextMenu skips
		// its close-on-click-outside check for that frame so the very
		// right-click that opened the menu doesn't immediately close
		// it (IsMouseClickedBool stays true for the rest of this
		// frame).
		a.contextMenuOpenedFrame = int(imgui.FrameCount())
		// (Previously called SDL_CaptureMouse here for global click
		// delivery, but under sdl2-compat → SDL3 it's a silent no-op
		// on X11 anyway and observed under VNC to interfere with
		// MenuItem's hover-based activation. Click-inside-xerotty +
		// Escape are the only reliable dismiss paths until SDL3's
		// proper popup window primitive is wired in.)
	}
	a.renderContextMenu()

	// Unsafe paste confirmation dialog
	a.renderPasteDialog()

	// Tab rename dialog
	a.renderRenameDialog()

	// Preferences dialog
	a.renderPreferences()

	// Resize overlay
	a.renderResizeOverlay()

	// Push cell-width resize increments to the OS window each time
	// the cell dimensions change (font zoom, metric refresh) AND
	// once at startup when this Window's real popped-out SDL_Window
	// replaces the hidden carrier/main viewport handle. Retry when
	// either the handle or the cell size changes, but only cache the
	// state after the native call succeeds.
	cellW := int(a.cellW)
	cellH := int(a.cellH)
	if cellW > 0 && cellH > 0 {
		if h := a.sdlWindowHandle(); h != 0 &&
			(h != a.resizeIncrementSetWindow ||
				cellW != a.resizeIncrementSetCellW ||
				cellH != a.resizeIncrementSetCellH) {
			if platform.SetResizeIncrements(h, cellW, cellH) {
				a.resizeIncrementSetWindow = h
				a.resizeIncrementSetCellW = cellW
				a.resizeIncrementSetCellH = cellH
			}
		}
	}

	// Apply the embedded app icon to this Window's native SDL_Window
	// once it exists. Linux WM uses it for taskbar / Alt-Tab; macOS
	// reads from the .icns in the bundle (this is a no-op there).
	// Same retry-until-handle-valid story as SetResizeIncrements
	// above — first frame has no handle yet, subsequent frames do.
	if !a.iconApplied {
		if h := a.sdlWindowHandle(); h != 0 {
			applyWindowIcon(h)
			a.iconApplied = true
		}
	}
}

func (w *Window) isSearching() bool {
	if tab := w.tabs.Active(); tab != nil {
		if s, ok := w.scroll[tab.ID]; ok {
			return s.Searching
		}
	}
	return false
}

func (w *Window) popupActive() bool {
	// Note: prefDialog is a non-modal window. It manages its own focus
	// through ImGui's WantCaptureKeyboard, so it shouldn't gate terminal input.
	return w.renamingTab || w.pendingPaste != ""
}

func (w *Window) processKeys() {
	// Only the focused Window consumes keyboard input. ImGui's IsKeyPressed
	// is global, so without this gate every Window's frame() would see the
	// same keybind event and dispatchAction would fire in every Window —
	// e.g. Cmd+T would create a new tab in every open Window at once
	// instead of just the one the user is typing into.
	if w != w.app.active {
		return
	}
	tab := w.tabs.Active()
	searching := w.isSearching()
	searchInputFocused := searching && w.searchInputFocused
	popupOpen := w.popupActive()

	// Modal popups (rename, unsafe paste) eat all input.
	if popupOpen {
		return
	}

	// Yield to ImGui only when a text-entry widget is actually wanting chars
	// (prefs InputText, etc). WantCaptureKeyboard is too broad — it also flips
	// true when a non-text window has plain focus, so e.g. clicking a tab or
	// recovering focus after SetWindowSize would silently swallow PTY input
	// even though nothing on screen needs the keys.
	if imgui.CurrentIO().WantTextInput() && !searchInputFocused {
		return
	}

	// Poll ImGui key state (SDL backend's SetKeyCallback is not implemented).
	// Pass the active tab's DECCKM state so arrow keys render as `ESC O X`
	// in pagers (less, git diff) and other apps that request application
	// cursor mode.
	appCursor := false
	if tab != nil && tab.Terminal != nil {
		appCursor = tab.Terminal.AppCursorMode()
	}
	events := input.PollKeys(w.app.cfg.Keybinds, appCursor, input.KeyOptions{
		Backspace:  w.app.cfg.Keys.Backspace,
		Delete:     w.app.cfg.Keys.Delete,
		ShiftEnter: w.app.cfg.Keys.ShiftEnter,
	})
	actionDispatched := false

	for _, ev := range events {
		// Escape closes search whenever search is open, even if the
		// InputText hasn't visibly grabbed focus yet — searchInputFocused
		// lags by a frame on re-open because IsItemFocused() is set
		// after the InputText is submitted, which happens AFTER
		// processKeys runs in the same frame. Gating on `searching`
		// (the synchronous flag from OpenSearch) makes the first
		// Escape on a re-opened search always close it.
		if searching && tab != nil && ev.Action == "" && len(ev.Bytes) == 1 && ev.Bytes[0] == 0x1b {
			s := w.getScroll(tab.ID)
			s.CloseSearch()
			// Inject a synthetic Escape-UP into ImGui's IO queue.
			// SDL can drop the real KEYUP during the Cocoa focus
			// shift triggered by InputText deactivating — leaving
			// ImGui with KeyEscape stuck "down" even though the
			// user released it. The next time the user presses
			// Escape (to close a re-opened search), ImGui sees no
			// edge transition (because it's "already down") and our
			// PollKeys returns no event. Force the up edge now so
			// the next real press is a clean transition.
			imgui.CurrentIO().AddKeyEvent(imgui.KeyEscape, false)
			searching = false
			searchInputFocused = false
			w.searchInputFocused = false
			continue
		}
		// Enter / Shift+Enter for next/prev match — these still gate
		// on searchInputFocused because if the user clicked the
		// terminal mid-search, Enter should go to the terminal.
		if searchInputFocused && tab != nil {
			s := w.getScroll(tab.ID)
			if ev.Action == "" && len(ev.Bytes) > 0 {
				switch ev.Bytes[0] {
				case '\r': // Enter — same as > (next match)
					s.NextMatch()
					if _, rows := w.gridSize(); rows > 0 {
						s.ScrollToCurrentMatch(rows)
					}
					w.searchFocusInput = true
					continue
				case '\n': // Shift+Enter — same as < (previous match)
					s.PrevMatch()
					if _, rows := w.gridSize(); rows > 0 {
						s.ScrollToCurrentMatch(rows)
					}
					w.searchFocusInput = true
					continue
				}
			}
		}

		if ev.Action != "" {
			w.dispatchAction(ev.Action)
			actionDispatched = true
			continue
		}

		// Don't forward key bytes to terminal during search
		if searchInputFocused {
			continue
		}

		if len(ev.Bytes) > 0 && tab != nil {
			// scroll_on_keystroke: gnome-terminal-style "any keypress
			// snaps back to live position". When off, the user can
			// type into the prompt (still sends to PTY) while staying
			// scrolled back to inspect history.
			if w.app.cfg.Scrollback.ScrollOnKeystroke {
				if s, ok := w.scroll[tab.ID]; ok {
					s.Reset()
				}
			}
			w.sel.clear() // typing clears selection
			tab.Terminal.Write(ev.Bytes)
		}
	}

	// Don't forward text input when: searching, a keybind just fired,
	// ImGui wants keyboard, or Ctrl is held (avoids leaking chars from
	// Ctrl+key combos). On macOS the cimgui ModSuper flag carries the
	// physical Ctrl key (ImGui's ConfigMacOSXBehaviors swaps the two), so
	// check both flags to suppress leaks for both physical Ctrl combos
	// and Cmd shortcuts.
	ctrlHeld := imgui.IsKeyDown(imgui.ModCtrl) ||
		(runtime.GOOS == "darwin" && imgui.IsKeyDown(imgui.ModSuper))
	if searchInputFocused || actionDispatched || imgui.CurrentIO().WantTextInput() || ctrlHeld {
		return
	}

	// Read text input from ImGui's character queue (SDL_TEXTINPUT events)
	if tab != nil {
		io := imgui.CurrentIO()
		chars := io.InputQueueCharacters()
		altHeld := imgui.IsKeyDown(imgui.ModAlt)
		if chars.Size > 0 {
			// scroll_on_keystroke: same gate as the translated-key
			// path above. Without checking here, plain letters /
			// numbers (which come through InputQueueCharacters, not
			// the special-key dispatcher) would always snap back to
			// live and the pref would do nothing for typing.
			if w.app.cfg.Scrollback.ScrollOnKeystroke {
				if s, ok := w.scroll[tab.ID]; ok {
					s.Reset()
				}
			}
			w.sel.clear()
			for _, ch := range chars.Slice() {
				if ch > 0 && ch < 0x10FFFF {
					buf := make([]byte, 5)
					var n int
					// Alt+key → ESC key (Meta mapping)
					if altHeld {
						buf[0] = 0x1b
						n = 1 + encodeRune(buf[1:], rune(ch))
					} else {
						n = encodeRune(buf, rune(ch))
					}
					tab.Terminal.Write(buf[:n])
				}
			}
		}
	}
}

// encodeRune encodes a rune to UTF-8 bytes.
func encodeRune(buf []byte, r rune) int {
	switch {
	case r < 0x80:
		buf[0] = byte(r)
		return 1
	case r < 0x800:
		buf[0] = byte(0xC0 | (r >> 6))
		buf[1] = byte(0x80 | (r & 0x3F))
		return 2
	case r < 0x10000:
		buf[0] = byte(0xE0 | (r >> 12))
		buf[1] = byte(0x80 | ((r >> 6) & 0x3F))
		buf[2] = byte(0x80 | (r & 0x3F))
		return 3
	default:
		buf[0] = byte(0xF0 | (r >> 18))
		buf[1] = byte(0x80 | ((r >> 12) & 0x3F))
		buf[2] = byte(0x80 | ((r >> 6) & 0x3F))
		buf[3] = byte(0x80 | (r & 0x3F))
		return 4
	}
}

func (w *Window) dispatchAction(action string) {
	switch action {
	case "new_tab":
		cols, rows := w.gridSize()
		// Inherit the currently active tab's CWD when the pref is on
		// so "New Tab" picks up wherever the user was working. Falls
		// through to xerotty's CWD when there's no active tab (first
		// tab) or GetCWD returns "" (process gone, /proc lookup
		// failed, etc.).
		var cwd string
		if w.app.cfg.Tabs.InheritCWD {
			if parentTab := w.tabs.Active(); parentTab != nil && parentTab.Terminal != nil {
				cwd = parentTab.Terminal.GetCWD()
			}
		}
		if tab, err := w.tabs.NewTab(cols, rows, cwd); err == nil && tab != nil {
			// AutoSelectNewTabs only catches new tabs once the bar has prior
			// frame state. On the 1→2 transition (tab bar first appears) it
			// can't, so request an explicit switch to the new tab.
			w.tabSwitchReq = tab.ID
		}
	case "close_tab":
		w.tabs.CloseActive()
		// macOS's NSWindow performClose: default-binds to Cmd+W and
		// fires AFTER our keybind handler, so without swallowing the
		// "close one tab" keypress also closes the whole window. The
		// flag is checked in the PlatformRequestClose path below. ~5
		// frames is enough to cover the latency between the keybind
		// firing and SDL3 surfacing the OS-level close event.
		w.swallowOSCloseFrames = 5
	case "new_window":
		// Single-process multi-window: append a new Window to this
		// App's slice. The render loop in Run() picks it up next
		// frame and wraps it in an ImGui top-level window that
		// multi-viewport auto-promotes to its own OS window. Same
		// NSApplication on macOS = one Dock icon for N windows;
		// same WM_CLASS on Linux = one taskbar group. See
		// docs/MULTI_WINDOW_REFACTOR.md for the architectural why.
		w.app.spawnWindow()
	case "next_tab":
		w.tabs.Next()
		if t := w.tabs.Active(); t != nil {
			w.tabSwitchReq = t.ID
		}
	case "prev_tab":
		w.tabs.Prev()
		if t := w.tabs.Active(); t != nil {
			w.tabSwitchReq = t.ID
		}
	case "copy":
		// selectedText already applies cfg.Clipboard.TrimTrailingWhitespace
		// per-row via extractText, so no extra trimming here.
		text := w.selectedText()
		if text != "" {
			input.ClipboardWrite(text)
		}
	case "paste":
		text, err := input.ClipboardRead()
		if err == nil && text != "" {
			w.pasteText(text)
		}
	case "paste_selection":
		text, err := input.PrimaryRead()
		if err == nil && text != "" {
			w.pasteText(text)
		}
	case "fullscreen":
		w.fullscreen = !w.fullscreen
		// Multi-window: target THIS Window's SDL_Window, not the
		// hidden carrier. SDL_GL_GetCurrentWindow() would silently
		// fullscreen the invisible carrier and look like a no-op.
		if h := w.sdlWindowHandle(); h != 0 {
			platform.SetFullscreen(h, w.fullscreen)
		}
	case "scroll_page_up":
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			_, rows := w.gridSize()
			s.PageUp(rows, tab.Terminal.ScrollbackLen())
		}
	case "scroll_page_down":
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			_, rows := w.gridSize()
			s.PageDown(rows)
		}
	case "scroll_top":
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			s.Offset = tab.Terminal.ScrollbackLen()
		}
	case "scroll_bottom":
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			s.Reset()
		}
	case "search":
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			s.OpenSearch()
			w.searchFocusInput = true
		}
	case "font_size_up":
		// Per-window zoom — only this Window's font size changes.
		// Other Windows keep their own zoom level (iTerm2-style).
		w.fontSize += 1
		w.updateFontMetrics()
	case "font_size_down":
		if w.fontSize > 6 {
			w.fontSize -= 1
			w.updateFontMetrics()
		}
	case "font_size_reset":
		// Reset to the configured default for this Window only.
		w.fontSize = renderer.PixelSize(&w.app.cfg)
		w.updateFontMetrics()
	case "select_all":
		if tab := w.tabs.Active(); tab != nil {
			cols := tab.Terminal.Emulator().Width()
			rows := tab.Terminal.Emulator().Height()
			w.sel.startCol = 0
			w.sel.startRow = 0
			w.sel.endCol = cols - 1
			w.sel.endRow = rows - 1
			w.sel.active = true
			w.sel.dragging = false
		}
	case "clear_scrollback":
		if tab := w.tabs.Active(); tab != nil {
			tab.Terminal.Emulator().ClearScrollback()
			if s, ok := w.scroll[tab.ID]; ok {
				s.Reset()
			}
		}
	case "reset_terminal":
		if tab := w.tabs.Active(); tab != nil {
			// Send RIS (Reset to Initial State) escape sequence
			tab.Terminal.Write([]byte("\x1bc"))
			tab.Terminal.Emulator().ClearScrollback()
			if s, ok := w.scroll[tab.ID]; ok {
				s.Reset()
			}
			w.sel.clear()
		}
	case "open_link":
		if w.hoveredLink != nil {
			openURL(w.hoveredLink.URL, w.app.cfg.Links.Opener)
		}
	case "copy_link":
		if w.hoveredLink != nil {
			input.ClipboardWrite(w.hoveredLink.URL)
		}
	case "rename_tab":
		if tab := w.tabs.Active(); tab != nil {
			w.renameBuffer = tab.DisplayTitle()
			w.renamingTab = true
			imgui.OpenPopupStr("Rename Tab")
		}
	case "preferences":
		w.openPreferences()
	default:
		// Check for parameterized actions
		if strings.HasPrefix(action, "goto_tab:") {
			nStr := strings.TrimPrefix(action, "goto_tab:")
			if n, err := strconv.Atoi(nStr); err == nil {
				w.tabs.GoTo(n)
				if t := w.tabs.Active(); t != nil {
					w.tabSwitchReq = t.ID
				}
			}
		} else if strings.HasPrefix(action, "set_theme:") {
			name := strings.TrimPrefix(action, "set_theme:")
			if t, err := themes.Load(name); err == nil {
				applyColorOverrides(&t, &w.app.cfg)
				w.app.theme = t
				// Theme is process-wide: every Window's renderer needs
				// the new palette or peer Windows render against the
				// stale one until they're individually re-themed. Same
				// loop applyPreferences uses for the prefs-driven path.
				for _, win := range w.app.windows {
					if win.renderer != nil {
						win.renderer.Theme = t
					}
				}
				// Update SDL background color to match new theme.
				bgR := float32((t.Background>>0)&0xFF) / 255.0
				bgG := float32((t.Background>>8)&0xFF) / 255.0
				bgB := float32((t.Background>>16)&0xFF) / 255.0
				platform.SetBgColor(imgui.NewVec4(bgR, bgG, bgB, 1.0))
			}
		} else if strings.HasPrefix(action, "exec:") {
			ctx := w.menuContext()
			menu.ExecAction(action, ctx)
		}
	}
}

func (w *Window) getScroll(tabID int) *scrollback.State {
	if s, ok := w.scroll[tabID]; ok {
		return s
	}
	s := scrollback.New()
	w.scroll[tabID] = s
	return s
}

func (w *Window) updateFontMetrics() {
	// Per-window font size — Cmd+= / Cmd+- on one Window doesn't
	// touch the others. w.fontSize is initialized from cfg.Font.Size
	// at spawn; from then on each Window has its own zoom level.
	if w.fontSize <= 0 {
		w.fontSize = renderer.PixelSize(&w.app.cfg)
	}
	pxSize := w.fontSize
	if w.app.baseFontSize <= 0 {
		w.app.baseFontSize = pxSize
	}

	// Capture current grid dimensions BEFORE scaling so we can resize
	// the window to keep the same number of cols/rows.
	cols, rows := w.gridSize()

	// Scale cell metrics proportionally. Ceil AFTER scaling —
	// baseCellW/H is the pre-ceil float advance so `ceil(baseCellW *
	// scale)` is the same answer measureCell would give at this zoom
	// (no compounding of ceiling errors per step).
	scale := pxSize / w.app.baseFontSize
	w.cellW, w.cellH = ceilCell(w.app.baseCellW*scale, w.app.baseCellH*scale)
	w.renderer.Metrics = renderer.CellMetrics{Width: w.cellW, Height: w.cellH}
	w.renderer.FontSize = pxSize

	// Rebuild the glyph cache at the new pxSize. Terminal cells render
	// through r.Glyphs.Get → AddImageV at the cached texture's native
	// size, so the cache's pxSize IS the size glyphs render at —
	// scaling the cell width without rebuilding the cache leaves
	// glyphs frozen at the old size and cells grow around them (the
	// "space zooms but text doesn't" symptom). Old textures are queued
	// for GPU deletion via the TextureManager and are safe to drop
	// here because frame() runs the wheel handler before any
	// renderer.Draw / AddImageV calls reference them.
	if w.renderer.Glyphs != nil && fontsys.Default != nil {
		primaryPath := renderer.ResolveFontPath(&w.app.cfg)
		if primaryPath != "" {
			fbScale := imgui.CurrentIO().DisplayFramebufferScale().X
			if fbScale <= 0 {
				fbScale = 1
			}
			if c, err := glyphcache.New(fontsys.Default, platform.Textures(), primaryPath, pxSize, fbScale); err == nil {
				w.renderer.Glyphs.Close()
				w.renderer.Glyphs = c
			}
		}
	}

	// Resize window to maintain the same grid at the new cell size.
	// Set w.width/w.height immediately so this frame renders correctly;
	// the per-frame DisplaySize sync will correct them on the next
	// frame if the WM didn't honour the request.
	pad := float32(w.app.cfg.Appearance.Padding) * 2
	// Add back the cellSafetyMargin so the post-resize gridSize() returns the
	// SAME cols/rows we're trying to preserve. Without this, every zoom step
	// loses one row+col because gridSize subtracts the margin from available
	// space.
	// cellOriginInsetX must be included alongside cellSafetyMarginH so
	// gridSize (which subtracts BOTH) recomputes back to the same cols.
	// Without the inset on Darwin (where it's 4px to clear the NSWindow
	// corner radius), each zoom step floors away one column.
	newW := int(math.Ceil(float64(float32(cols)*w.cellW + pad + cellSafetyMarginH + cellOriginInsetX)))
	newH := int(math.Ceil(float64(float32(rows)*w.cellH + pad + w.tabBarH + cellSafetyMarginV)))
	// Every Window's OS geometry rides ImGui multi-viewport. Stash
	// the target size and flip pendingResize; wrappedFrame's Begin
	// uses CondAlways instead of CondFirstUseEver on the next frame,
	// which ImGui propagates to the platform window via
	// Platform_SetWindowSize.
	w.width = newW
	w.height = newH
	w.pendingResize = true
	w.skipDisplaySync = 2
	w.resizeTerminals()

	// Show overlay with the new zoom level. Percent is current pxSize
	// over the configured base, rounded — so default reads 100%, zoom-in
	// reads >100%, zoom-out reads <100%. skipDisplaySync prevents the
	// drag-resize trigger in frame() from clobbering this with cols×rows
	// for the next couple frames.
	percent := int(math.Round(float64(pxSize / w.app.baseFontSize * 100)))
	w.resizeOverlayText = fmt.Sprintf("%d%%", percent)
	w.resizeTime = imgui.Time()
	w.resizeOverlay = true
}

func (w *Window) renderTabBar() {
	w.tabBarHovered = false
	if w.tabs.Count() <= 1 {
		return // Don't show tab bar with single tab
	}

	// Custom tab bar — every tab gets an equal share of the available
	// width (totalW / numTabs), iTerm/Terminal.app style. ImGui's
	// BeginTabBar doesn't have a stretch-to-fit option (only Shrink
	// and Scroll fitting policies), so we lay out tabs manually with
	// InvisibleButtons for hit detection and drawlist primitives for
	// the visuals. Rendered into the wrapper's WindowDrawList so the
	// tab visuals layer ON TOP of the terminal cells drawn earlier in
	// frame().
	originX := w.contentOriginX
	originY := w.contentOriginY
	totalW := float32(w.width)
	height := w.tabBarH
	numTabs := w.tabs.Count()
	tabW := totalW / float32(numTabs)

	style := imgui.CurrentStyle()
	col := func(c imgui.Col) uint32 { return imgui.ColorU32Col(c) }
	activeBg := col(imgui.ColTabSelected)
	hoverBg := col(imgui.ColTabHovered)
	inactiveBg := col(imgui.ColTab)
	textCol := col(imgui.ColText)
	underlineCol := col(imgui.ColTabSelectedOverline)

	drawList := imgui.WindowDrawList()
	closeBtnW := imgui.FrameHeight() * 0.6
	framePad := style.FramePadding()
	clickedIdx := -1
	closedIdx := -1

	for i, tab := range w.tabs.Tabs {
		x0 := originX + float32(i)*tabW
		x1 := originX + float32(i+1)*tabW
		y0 := originY
		y1 := originY + height
		isActive := i == w.tabs.ActiveIdx

		// Whole-tab invisible button for hit detection. We use ONE
		// button rather than two (with the close X as a second
		// overlapping InvisibleButton), because ImGui's "first item
		// wins hover" rule means a later-submitted close-button
		// InvisibleButton can't override the tab's. Instead we
		// dispatch the click based on where the cursor was: in the
		// close-button rect → close; otherwise → switch.
		closeX0 := x1 - closeBtnW - framePad.X
		closeY0 := y0 + (height-closeBtnW)/2
		closeX1 := closeX0 + closeBtnW
		closeY1 := closeY0 + closeBtnW

		imgui.SetCursorScreenPos(imgui.Vec2{X: x0, Y: y0})
		tabID := fmt.Sprintf("##tab%d%s", tab.ID, w.imguiSuffix())
		// PressedOnClick → fires on mouse-DOWN instead of mouse-UP,
		// so tab switches and close-button hits feel snappy rather
		// than waiting for the release edge. Flag lives in
		// ButtonFlagsPrivate, hence the cast.
		clicked := imgui.InvisibleButtonV(tabID, imgui.Vec2{X: tabW, Y: height}, imgui.ButtonFlags(imgui.ButtonFlagsPressedOnClick))
		hovered := imgui.IsItemHovered()
		if hovered {
			w.tabBarHovered = true
		}

		// Background
		bg := inactiveBg
		switch {
		case isActive:
			bg = activeBg
		case hovered:
			bg = hoverBg
		}
		// Visible tab body: inset only on the right edge (except for
		// the last tab) so adjacent tabs have a single-pixel gap
		// between them. Rounded top corners + square bottom so the
		// tab sits on the underline like a tab dock.
		const sideGap = 1
		const tabRounding = 3
		bgX0 := x0
		bgX1 := x1
		if i < numTabs-1 {
			bgX1 -= sideGap
		}
		drawList.AddRectFilledV(
			imgui.Vec2{X: bgX0, Y: y0},
			imgui.Vec2{X: bgX1, Y: y1},
			bg,
			tabRounding,
			imgui.DrawFlagsRoundCornersTop,
		)
		// Active-tab underline (matches ImGui's TabBarOverline style).
		if isActive {
			overlineH := float32(2)
			drawList.AddRectFilled(
				imgui.Vec2{X: bgX0, Y: y1 - overlineH},
				imgui.Vec2{X: bgX1, Y: y1},
				underlineCol,
			)
		}

		// Centered label, clipped to the tab's interior so long
		// titles don't bleed into the close button or the next tab.
		label := tab.DisplayTitle()
		labelSize := imgui.CalcTextSize(label)
		labelX := x0 + framePad.X
		labelMaxX := x1 - closeBtnW - framePad.X*2
		if labelMaxX-labelX > labelSize.X {
			// Center if there's slack.
			labelX = x0 + (tabW-closeBtnW-labelSize.X)/2
		}
		labelY := y0 + (height-labelSize.Y)/2
		// PushClipRect so long labels truncate at the close-button edge.
		drawList.PushClipRectV(
			imgui.Vec2{X: x0 + framePad.X, Y: y0},
			imgui.Vec2{X: labelMaxX, Y: y1},
			true,
		)
		drawList.AddTextVec2V(imgui.Vec2{X: labelX, Y: labelY}, textCol, label)
		drawList.PopClipRect()

		// Manual hover-test for the close-button area (no separate
		// InvisibleButton — see comment above).
		mp := imgui.MousePos()
		mouseInClose := hovered &&
			mp.X >= closeX0 && mp.X < closeX1 &&
			mp.Y >= closeY0 && mp.Y < closeY1
		// Show the X on hover of the tab or for the active tab.
		// Use full textCol (not dim) so it's readable against the
		// tab background; mouseInClose paints a hover-bg rect behind
		// the X to give clear feedback the button is targetable.
		if hovered || isActive {
			xCol := textCol
			if mouseInClose {
				drawList.AddRectFilled(
					imgui.Vec2{X: closeX0, Y: closeY0},
					imgui.Vec2{X: closeX1, Y: closeY1},
					hoverBg,
				)
			}
			pad := closeBtnW * 0.25
			thick := float32(1.5)
			drawList.AddLineV(
				imgui.Vec2{X: closeX0 + pad, Y: closeY0 + pad},
				imgui.Vec2{X: closeX1 - pad, Y: closeY1 - pad},
				xCol, thick,
			)
			drawList.AddLineV(
				imgui.Vec2{X: closeX1 - pad, Y: closeY0 + pad},
				imgui.Vec2{X: closeX0 + pad, Y: closeY1 - pad},
				xCol, thick,
			)
		}

		// Dispatch the click: cursor inside the close X rect → close,
		// anywhere else on the tab → switch.
		if clicked {
			if mouseInClose {
				closedIdx = i
			} else {
				clickedIdx = i
			}
		}
	}

	// Honor any pending Cmd+N / wheel / etc. switch request before
	// applying click-based switches; the click handler below would
	// otherwise be a no-op for the keyboard path since clickedIdx<0.
	if w.tabSwitchReq != -1 {
		for i, tab := range w.tabs.Tabs {
			if tab.ID == w.tabSwitchReq {
				w.tabs.ActiveIdx = i
				break
			}
		}
		w.tabSwitchReq = -1
	}

	if clickedIdx >= 0 {
		w.tabs.ActiveIdx = clickedIdx
		// Click seeds drag-from-this-tab state. Drag only activates
		// once the user moves the cursor past a threshold from the
		// press position, so plain clicks still feel like clicks.
		w.tabDragIdx = clickedIdx
		mp := imgui.MousePos()
		w.tabDragStartX = mp.X
		w.tabDragStartY = mp.Y
	}

	// Drag-to-reorder within this tab bar, OR lift-off into a cross-
	// Window drag. Thresholds are RELATIVE to the press position, not
	// absolute screen Y — otherwise a click that happened to land
	// near either vertical edge of the tab bar would instantly trigger
	// detach with no actual movement.
	const liftOffPx = 30 // vertical movement required to detach
	const dragSlopPx = 4 // dead zone around the click before we consider any drag
	if w.tabDragIdx >= 0 && imgui.IsMouseDown(imgui.MouseButtonLeft) {
		mp := imgui.MousePos()
		dx := mp.X - w.tabDragStartX
		dy := mp.Y - w.tabDragStartY
		moved := dx*dx+dy*dy > dragSlopPx*dragSlopPx

		// Cursor feedback once we're past the dead zone — "Hand"
		// reads as "you're dragging this".
		if moved {
			imgui.SetMouseCursor(imgui.MouseCursorHand)
		}

		if dy > liftOffPx || dy < -liftOffPx {
			// Lift off — pull the tab out of this Window into the
			// process-wide drag state. After this point the tab
			// is no longer in any Window's tabs.Manager. We also
			// kick off a Wayland data_device drag so the compositor
			// will route cursor enter/leave/drop to other surfaces
			// during the held drag (Wayland's implicit pointer
			// grab otherwise blocks cross-surface events).
			if t := w.tabs.RemoveTab(w.tabDragIdx); t != nil {
				drag := &tabDrag{
					Term:      t.Terminal,
					Label:     t.DisplayTitle(),
					From:      w,
					LastFocus: w.sdlWindowHandle(),
				}
				if cur := platform.MouseFocusWindowID(); cur != 0 {
					drag.LastFocus = cur
				}
				drag.WaylandStarted = platform.StartWaylandTabDrag(w.sdlWindowHandle())
				w.app.dragTab = drag
			}
			w.tabDragIdx = -1
		} else if moved && mp.Y >= originY && mp.Y < originY+height && mp.X >= originX {
			targetIdx := int((mp.X - originX) / tabW)
			if targetIdx < 0 {
				targetIdx = 0
			}
			if targetIdx >= numTabs {
				targetIdx = numTabs - 1
			}
			if targetIdx != w.tabDragIdx {
				w.tabs.MoveTab(w.tabDragIdx, targetIdx)
				w.tabDragIdx = targetIdx
			}
		}
	} else if !imgui.IsMouseDown(imgui.MouseButtonLeft) {
		w.tabDragIdx = -1
	}

	// While a cross-Window drag is in flight (any Window's drag),
	// show the "moving" cursor everywhere over our wrapper so the
	// user has feedback that the floating tab is held.
	if w.app.dragTab != nil {
		imgui.SetMouseCursor(imgui.MouseCursorResizeAll)
	}
	if closedIdx >= 0 {
		w.tabs.CloseTab(closedIdx)
		// Wake the run loop so the next frame fires immediately
		// instead of waiting for WaitEventTimeout to expire. The
		// tab-bar→no-tab-bar transition triggers an SDL window
		// resize at the START of the next frame; without the wake,
		// the user can see up to ~30ms of stale window-size before
		// the resize lands.
		platform.PostWake()
	}
}

func (w *Window) renderContextMenu() {
	if !w.contextMenuOpen {
		return
	}

	// Open an SDL3 xdg_popup parented to THIS Window's OS window so
	// the compositor positions the menu relative to the parent
	// surface (Wayland forbids absolute toplevel positioning, which
	// is why the old multi-viewport-pop-out approach landed the menu
	// in the middle of the screen). RunImGuiPopup blocks until the
	// menu dismisses — the rest of the app pauses, which matches
	// native context-menu semantics on every OS.
	parentID := uintptr(0)
	if vp := w.viewport(); vp != nil {
		parentID = vp.PlatformHandle()
	}
	// Click pos is in absolute desktop coords. The popup wants
	// parent-relative coords. Compute relative from the Window's
	// captured contentOrigin (the wrapper Begin's screen-space top-
	// left) so the popup opens at the click position even if the
	// compositor stuck the OS window somewhere we didn't ask for.
	relX := int(w.contextMenuX - w.contentOriginX)
	relY := int(w.contextMenuY - w.contentOriginY)
	if relX < 0 {
		relX = 0
	}
	if relY < 0 {
		relY = 0
	}

	ctx := w.menuContext()
	// Pre-compute popup size matching the popup's actual rendering.
	// Values are empirical — captured by tracing CursorPosY of an
	// 11-item / 4-separator menu in the popup's default font + style
	// and back-solving. See popup_imgui.cpp trace lines.
	//
	//   items * 17 + separators * 4 + 16 (top+bottom WindowPadding)
	//   == actual rendered height
	//
	// These are properties of ImGui's default font + default style
	// rendering MenuItem/Separator, so they hold for any
	// user-customized menu items list.
	const (
		popupItemAdvance = 17.0 // MenuItem cursor advance (default font+style)
		popupSepAdvance  = 4.0  // Separator cursor advance
		popupWindowPadY  = 8.0
		popupWindowPadX  = 8.0
		popupCharW       = 7.0 // ~ default ImGui font monospace width
	)
	popupW := float32(120)
	popupH := float32(popupWindowPadY * 2)
	for _, item := range w.app.cfg.Menu.Items {
		if item.Action == "separator" {
			popupH += popupSepAdvance
			continue
		}
		labelW := float32(len(item.Label)) * popupCharW
		shortcutW := float32(len(item.Shortcut)) * popupCharW
		w := labelW + shortcutW + 30
		if w > popupW {
			popupW = w
		}
		popupH += popupItemAdvance
	}
	popupW += float32(popupWindowPadX * 2)
	var selectedAction string
	platform.RunImGuiPopup(parentID, relX, relY, int(popupW), int(popupH),
		func() platform.PopupMenuDrawResult {
			io := imgui.CurrentIO()
			imgui.SetNextWindowPos(imgui.Vec2{X: 0, Y: 0})
			imgui.SetNextWindowSize(io.DisplaySize())
			flags := imgui.WindowFlagsNoTitleBar |
				imgui.WindowFlagsNoResize |
				imgui.WindowFlagsNoMove |
				imgui.WindowFlagsNoSavedSettings |
				imgui.WindowFlagsNoCollapse |
				imgui.WindowFlagsNoScrollbar
			var action string
			var contentH float32
			var contentW float32
			if imgui.BeginV("##popupmenu", nil, flags) {
				action = menu.RenderItemsOnly(w.app.cfg.Menu.Items, ctx)
				// CursorPosY after the last item is exactly where the
				// next item would go — i.e. the true rendered content
				// height. Use it (plus a bit of bottom padding) as
				// the desired SDL popup size; the C side then shrinks
				// the OS window to fit. Robust against any
				// miscalculation in the pre-Open estimate.
				contentH = imgui.CursorPosY() + 8 // bottom padding
				contentW = imgui.WindowSize().X
			}
			imgui.End()
			res := platform.PopupMenuDrawResult{
				DesiredWidth:  int(contentW),
				DesiredHeight: int(contentH),
			}
			if action != "" {
				selectedAction = action
				res.Close = true
			}
			return res
		})

	w.contextMenuOpen = false
	w.contextMenuCaptured = false
	if selectedAction != "" {
		w.dispatchAction(selectedAction)
	}
	platform.PostWake()
}

func (w *Window) menuContext() *menu.Context {
	ctx := &menu.Context{
		HasSelection: w.sel.active,
		Selection:    w.selectedText(),
	}
	if tab := w.tabs.Active(); tab != nil {
		ctx.TabTitle = tab.DisplayTitle()
		// CWD detection via /proc
		if tab.Terminal != nil {
			ctx.CWD = getCWD(tab.Terminal)
		}
	}
	if w.hoveredLink != nil {
		ctx.HasLink = true
		ctx.Link = w.hoveredLink.URL
	}
	return ctx
}

func (w *Window) renderSearchOverlay() {
	tab := w.tabs.Active()
	if tab == nil {
		w.searchInputFocused = false
		return
	}
	s := w.getScroll(tab.ID)
	if !s.Searching {
		w.searchInputFocused = false
		return
	}

	// AlwaysAutoResize so the overlay stays as compact as the original
	// did. The match counter uses a fixed-width Dummy slot below so
	// digit-count changes (1/9 vs 10/14 vs 100/120) don't trigger
	// the auto-resize and reshuffle the buttons.
	vpX, vpY := w.contentOriginX, w.contentOriginY
	// Anchor by TOP-RIGHT pivot so the overlay's right edge sits
	// inside the wrapper regardless of its actual width — using a
	// fixed left-edge offset breaks any time the overlay's natural
	// auto-resized width grows past that offset. Inset by the
	// scrollbar width so the overlay doesn't cover the scrollbar.
	scrollbarInset := float32(w.app.cfg.Scrollbar.Width)
	// Pin the overlay to the wrapper's viewport so multi-viewport's
	// auto-pop-out logic can't detach it into its own OS window —
	// without this, the overlay can end up "floating behind" the
	// wrapper and the user has to close/reopen to get it back.
	if w.imViewport != nil {
		imgui.SetNextWindowViewport(w.imViewport.ID())
	}
	imgui.SetNextWindowPosV(
		imgui.Vec2{X: vpX + float32(w.width) - scrollbarInset, Y: vpY + w.tabBarH},
		imgui.CondAlways,
		imgui.Vec2{X: 1, Y: 0},
	)
	// NOT setting NoBringToFrontOnFocus here even though the wrapper has
	// it — this overlay needs to be the focused ImGui window so the
	// InputText keyboard nav routes correctly. NavWindow has to be the
	// overlay; without that, re-opening Ctrl-F (overlay → Esc → overlay)
	// can leave keyboard focus stuck on the wrapper and the input never
	// captures keystrokes. Z-order is enforced separately via
	// InternalBringWindowToDisplayFront at the end of this function.
	flags := imgui.WindowFlagsNoTitleBar | imgui.WindowFlagsNoResize |
		imgui.WindowFlagsNoMove | imgui.WindowFlagsNoScrollbar |
		imgui.WindowFlagsAlwaysAutoResize | imgui.WindowFlagsNoDocking

	w.searchInputFocused = false
	searchWinName := "##search" + w.imguiSuffix()
	if imgui.BeginV(searchWinName, nil, flags) {
		// Track actual rendered width for the selection hit-test.
		w.searchOverlayW = imgui.WindowWidth()

		imgui.SetNextItemWidth(180)

		// Re-focus the input when requested (on open, after < > clicks, or
		// when it loses focus to the terminal). Guard only on IsMouseDown
		// so we don't snatch ActiveId mid-click. We DO NOT guard on
		// IsMouseReleased — the macOS mirror's transition-based synthetic
		// UP events flip IsMouseReleased true on unrelated frames, which
		// was blocking the focus grab on Ctrl-F re-open.
		if w.searchFocusInput && !imgui.IsMouseDown(0) {
			imgui.SetKeyboardFocusHere()
			w.searchFocusInput = false
		}

		_, rows := w.gridSize()
		prevQuery := s.Query
		changed := imgui.InputTextWithHint("##searchinput", "Search...", &s.Query, 0, nil)
		w.searchInputFocused = imgui.IsItemFocused()
		if changed && s.Query != prevQuery {
			s.Search(tab.Terminal.Emulator(), rows)
			s.ScrollToCurrentMatch(rows)
		}
		// Counter: render into a fixed-width Dummy slot via drawList
		// so the window's auto-resize doesn't care about the digit
		// count. The text is drawn manually centered inside the slot.
		imgui.SameLineV(0, 4)
		slotW := float32(56)
		slotH := imgui.FrameHeight()
		slotPos := imgui.CursorScreenPos()
		// AlignTextToFramePadding so the Dummy's Y matches a button row.
		imgui.AlignTextToFramePadding()
		imgui.Dummy(imgui.Vec2{X: slotW, Y: slotH})
		var counterText string
		if len(s.Matches) > 0 {
			counterText = fmt.Sprintf("%d/%d", s.MatchIdx+1, len(s.Matches))
		} else if s.Query != "" {
			counterText = "0"
		}
		if counterText != "" {
			ts := imgui.CalcTextSize(counterText)
			tx := slotPos.X
			ty := slotPos.Y + (slotH-ts.Y)/2
			imgui.WindowDrawList().AddTextVec2V(
				imgui.Vec2{X: tx, Y: ty},
				imgui.ColorU32Col(imgui.ColText),
				counterText,
			)
		}

		imgui.SameLineV(0, 4)

		// Buttons: use ButtonV + debug trace to diagnose click issues.
		prevClicked := imgui.ButtonV("<", imgui.Vec2{X: 20, Y: 0})
		imgui.SameLineV(0, 2)
		nextClicked := imgui.ButtonV(">", imgui.Vec2{X: 20, Y: 0})
		imgui.SameLineV(0, 2)
		closeClicked := imgui.ButtonV("X", imgui.Vec2{X: 20, Y: 0})

		if prevClicked {
			s.PrevMatch()
			s.ScrollToCurrentMatch(rows)
			w.searchFocusInput = true
		}
		if nextClicked {
			s.NextMatch()
			s.ScrollToCurrentMatch(rows)
			w.searchFocusInput = true
		}
		if closeClicked {
			s.CloseSearch()
			w.searchFocusInput = false
			w.searchInputFocused = false
		}

		// Row 2: search options
		optChanged := imgui.Checkbox("CASE", &s.CaseSensitive)
		imgui.SameLineV(0, 8)
		optChanged = imgui.Checkbox("RE", &s.UseRegex) || optChanged
		imgui.SameLineV(0, 8)
		optChanged = imgui.Checkbox("EXACT", &s.WholeWord) || optChanged
		imgui.SameLineV(0, 8)
		optChanged = imgui.Checkbox("WRAP", &s.WrapAround) || optChanged
		if optChanged && s.Query != "" {
			s.Search(tab.Terminal.Emulator(), rows)
			s.ScrollToCurrentMatch(rows)
		}
	}
	imgui.End()

	// Force the search overlay to the END of ImGui's global windows
	// list so it renders LAST = on top. Without this the wrapper's
	// drawlist (which gets the terminal cells drawn into it earlier
	// in frame()) ends up rendering after the overlay's drawlist
	// when focus events shuffle the order, and the overlay disappears
	// behind the terminal.
	if sw := imgui.InternalFindWindowByName(searchWinName); sw != nil {
		imgui.InternalBringWindowToDisplayFront(sw)
	}
	// Highlights are drawn in the terminal window's draw list (see frame())
}

func (w *Window) drawSearchHighlights(s *scrollback.State, drawList *imgui.DrawList) {
	cellW := w.cellW
	cellH := w.cellH
	_, rows := w.gridSize()
	matchBg := uint32(0x4400FFFF)   // yellow, semi-transparent (ABGR)
	currentBg := uint32(0x8800AAFF) // orange, more opaque (ABGR)

	for i, m := range s.Matches {
		// Convert absolute line index to screen row accounting for scroll offset.
		// Match lines: negative = scrollback, 0+ = live screen.
		// Scroll offset pushes everything down: scrollback lines become visible.
		screenRow := m.Line + s.Offset
		if screenRow < 0 || screenRow >= rows {
			continue
		}

		x := w.renderer.OffsetX + float32(m.Col)*cellW
		y := w.renderer.OffsetY + float32(screenRow)*cellH
		wd := float32(m.Len) * cellW

		bg := matchBg
		if i == s.MatchIdx {
			bg = currentBg
		}

		drawList.AddRectFilled(
			imgui.Vec2{X: x, Y: y},
			imgui.Vec2{X: x + wd, Y: y + cellH},
			bg,
		)
	}
}

func (w *Window) renderResizeOverlay() {
	if !w.resizeOverlay {
		return
	}
	// Honor cfg.Appearance.ResizeOverlay (master on/off) — also clear
	// the latched flag so a later toggle of the pref doesn't stall
	// with a stale overlay sitting at the resizeTime from before.
	if !w.app.cfg.Appearance.ResizeOverlay {
		w.resizeOverlay = false
		w.resizeOverlayText = ""
		return
	}

	elapsed := imgui.Time() - w.resizeTime
	// Total display time from prefs (seconds). Fade occupies the last
	// third (matches the original 1.0s solid + 0.5s fade = 1.5s total
	// ratio that was hardcoded before).
	duration := float64(w.app.cfg.Appearance.ResizeOverlayDuration)
	if duration <= 0 {
		duration = 1.0
	}
	fadeStart := duration * (2.0 / 3.0)

	if elapsed > duration {
		w.resizeOverlay = false
		w.resizeOverlayText = ""
		return
	}

	cols, rows := w.gridSize()
	primary := fmt.Sprintf("%d × %d", cols, rows)
	secondary := w.resizeOverlayText // empty unless triggered by zoom
	primarySize := imgui.CalcTextSize(primary)
	var secondarySize imgui.Vec2
	if secondary != "" {
		secondarySize = imgui.CalcTextSize(secondary)
	}

	lineGap := primarySize.Y // ~one line of blank space between primary and secondary
	innerW := primarySize.X
	if secondarySize.X > innerW {
		innerW = secondarySize.X
	}
	innerH := primarySize.Y
	if secondary != "" {
		innerH += lineGap + secondarySize.Y
	}
	padX := float32(16)
	padY := float32(10)
	boxW := innerW + padX*2
	boxH := innerH + padY*2

	// Center on the window's viewport in absolute desktop space — under
	// multi-viewport the global foreground drawlist isn't tied to the
	// SDL window.
	var vpX, vpY float32
	if vp := w.viewport(); vp != nil {
		vpX, vpY = vp.Pos().X, vp.Pos().Y
	}
	cx := vpX + float32(w.width)/2
	cy := vpY + float32(w.height)/2

	// Fade out alpha
	alpha := float32(1.0)
	if elapsed > fadeStart {
		alpha = float32(1.0 - (elapsed-fadeStart)/(duration-fadeStart))
	}

	bgColor := uint32(uint8(alpha*180)) << 24 // semi-transparent black
	fgColor := uint32(0x00FFFFFF) | (uint32(uint8(alpha*255)) << 24)

	dl := w.fgDrawList()
	dl.AddRectFilledV(
		imgui.Vec2{X: cx - boxW/2, Y: cy - boxH/2},
		imgui.Vec2{X: cx + boxW/2, Y: cy + boxH/2},
		bgColor, 6, 0,
	)
	topY := cy - innerH/2
	if secondary != "" {
		// Zoom % rides above cols×rows; the dimensions remain the
		// dominant bottom line so users glancing at the overlay during
		// either drag-resize or zoom see the same anchor.
		dl.AddTextVec2(
			imgui.Vec2{X: cx - secondarySize.X/2, Y: topY},
			fgColor,
			secondary,
		)
		primaryY := topY + secondarySize.Y + lineGap
		dl.AddTextVec2(
			imgui.Vec2{X: cx - primarySize.X/2, Y: primaryY},
			fgColor,
			primary,
		)
	} else {
		dl.AddTextVec2(
			imgui.Vec2{X: cx - primarySize.X/2, Y: topY},
			fgColor,
			primary,
		)
	}
}

func (w *Window) drawScrollbar(tab *tabs.Tab, scrollOff int, drawList *imgui.DrawList) {
	vis := w.app.cfg.Scrollbar.Visible
	if vis == "never" {
		return
	}

	sbLen := tab.Terminal.ScrollbackLen()
	_, rows := w.gridSize()
	totalLines := sbLen + rows

	// auto mode: only show when scrolled back
	if vis == "auto" && scrollOff == 0 {
		return
	}

	barW := float32(w.app.cfg.Scrollbar.Width)
	vpOffX, vpOffY := w.contentOriginX, w.contentOriginY
	barX := vpOffX + float32(w.width) - barW
	barY := vpOffY + w.tabBarH
	termH := float32(w.height) - w.tabBarH // full height below tab bar

	// Check if mouse is hovering the thumb
	mpos := imgui.MousePos()
	hovered := mpos.X >= barX && mpos.X <= barX+barW && mpos.Y >= barY && mpos.Y <= barY+termH

	thumbY, thumbH := w.renderer.DrawScrollbar(renderer.ScrollbarParams{
		X:              barX,
		Y:              barY,
		Width:          barW,
		Height:         termH,
		ScrollOffset:   scrollOff,
		TotalLines:     totalLines,
		VisibleLines:   rows,
		MinThumbHeight: float32(w.app.cfg.Scrollbar.MinThumbHeight),
		Hovered:        hovered,
	}, drawList)

	// Handle scrollbar click-drag
	if imgui.IsMouseClickedBool(0) && hovered {
		if mpos.Y >= thumbY && mpos.Y <= thumbY+thumbH {
			// Click ON the thumb — start drag
			w.sbDragging = true
		} else if mpos.Y < thumbY {
			// Click above thumb: page up
			if s, ok := w.scroll[tab.ID]; ok {
				s.PageUp(rows, sbLen)
			}
		} else if mpos.Y > thumbY+thumbH {
			// Click below thumb: page down
			if s, ok := w.scroll[tab.ID]; ok {
				s.PageDown(rows)
			}
		}
	}

	// End scrollbar drag on mouse release
	if !imgui.IsMouseDown(0) {
		w.sbDragging = false
	}

	// Drag thumb: map mouse Y to scroll offset.
	// Once dragging starts, track Y regardless of X (user may drift sideways).
	if w.sbDragging {
		trackSpace := termH - thumbH
		if trackSpace > 0 {
			frac := 1.0 - (mpos.Y-barY-thumbH/2)/trackSpace
			if frac < 0 {
				frac = 0
			}
			if frac > 1 {
				frac = 1
			}
			maxOff := sbLen
			newOff := int(frac * float32(maxOff))
			if s, ok := w.scroll[tab.ID]; ok {
				s.Offset = newOff
			}
		}
	}
}

func (w *Window) pasteText(text string) {
	tab := w.tabs.Active()
	if tab == nil {
		return
	}
	cfg := w.app.cfg.Clipboard.UnsafePaste
	if !cfg.Enabled {
		tab.Terminal.Paste(text)
		return
	}

	shouldWarn := false
	if cfg.MultilineWarning && strings.Contains(text, "\n") {
		shouldWarn = true
	}
	if !shouldWarn && cfg.NewlineGuard && (strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\r\n")) {
		shouldWarn = true
	}

	if !shouldWarn && len(cfg.Patterns) > 0 {
		for _, pattern := range cfg.Patterns {
			m, err := regexp.MatchString(pattern, text)
			if err != nil {
				// Treat invalid regex as unsafe so a broken pattern in
				// preferences fails closed instead of silently disabling
				// the guard.
				shouldWarn = true
				break
			}
			if m {
				shouldWarn = true
				break
			}
		}
	}

	if shouldWarn {
		w.pendingPaste = text
		imgui.OpenPopupStr("Unsafe Paste")
		return
	}
	tab.Terminal.Paste(text)
}

func (w *Window) renderPasteDialog() {
	if w.pendingPaste == "" {
		return
	}

	// Dim THIS Window's content rect only. ImGui's BeginPopupModalV
	// would auto-dim every OTHER viewport too (hardcoded in
	// imgui.cpp's RenderDimmedBackgrounds — "Draw dimming background
	// on _other_ viewports than the ones our windows are in"), which
	// made the user's peer terminals look greyed-out even though
	// they're fully usable. Use a regular BeginV with our own dim
	// quad scoped to this Window's wrapper drawlist instead.
	dimCol := uint32(0x80000000) // 50% black, ABGR
	w.bgDrawList().AddRectFilled(
		imgui.Vec2{X: w.contentOriginX, Y: w.contentOriginY},
		imgui.Vec2{X: w.contentOriginX + float32(w.width), Y: w.contentOriginY + float32(w.height)},
		dimCol,
	)

	// Center the dialog in the OWNING Window's content rect in
	// absolute desktop coords. Pin to this Window's viewport so it
	// doesn't pop out into its own OS window for a tiny dialog.
	center := imgui.Vec2{X: w.contentOriginX + float32(w.width)/2, Y: w.contentOriginY + float32(w.height)/2}
	if vp := w.viewport(); vp != nil {
		imgui.SetNextWindowViewport(vp.ID())
	}
	imgui.SetNextWindowPosV(center, imgui.CondAppearing, imgui.Vec2{X: 0.5, Y: 0.5})

	flags := imgui.WindowFlagsAlwaysAutoResize |
		imgui.WindowFlagsNoCollapse |
		imgui.WindowFlagsNoSavedSettings |
		imgui.WindowFlagsNoDocking
	if imgui.BeginV("Unsafe Paste###pastedlg"+w.imguiSuffix(), nil, flags) {
		lines := strings.Count(w.pendingPaste, "\n") + 1
		imgui.Text(fmt.Sprintf("Paste %d lines into terminal?", lines))
		imgui.Text("Multi-line paste may execute commands.")
		imgui.TextDisabled("Enter = paste   Esc = cancel")
		imgui.Separator()

		// Keyboard shortcuts: Enter / KeypadEnter = accept (paste),
		// Esc = cancel. Input gating happens in processKeys via
		// popupActive() (returns true when pendingPaste != ""), so
		// these IsKeyPressedBool calls catch the keys before they
		// reach the PTY. The button row is the authoritative path;
		// this just makes the common case fast.
		accept := imgui.Button("Paste")
		imgui.SameLineV(0, 8)
		cancel := imgui.Button("Cancel")
		if imgui.IsKeyPressedBool(imgui.KeyEnter) || imgui.IsKeyPressedBool(imgui.KeyKeypadEnter) {
			accept = true
		}
		if imgui.IsKeyPressedBool(imgui.KeyEscape) {
			cancel = true
		}
		if accept {
			if tab := w.tabs.Active(); tab != nil {
				tab.Terminal.Paste(w.pendingPaste)
			}
			w.pendingPaste = ""
		} else if cancel {
			w.pendingPaste = ""
		}
	}
	imgui.End()
}

func (w *Window) renderRenameDialog() {
	if !w.renamingTab {
		return
	}

	// Dim THIS Window only — same reason as renderPasteDialog.
	dimCol := uint32(0x80000000)
	w.bgDrawList().AddRectFilled(
		imgui.Vec2{X: w.contentOriginX, Y: w.contentOriginY},
		imgui.Vec2{X: w.contentOriginX + float32(w.width), Y: w.contentOriginY + float32(w.height)},
		dimCol,
	)

	if vp := w.viewport(); vp != nil {
		imgui.SetNextWindowViewport(vp.ID())
	}
	center := imgui.Vec2{X: w.contentOriginX + float32(w.width)/2, Y: w.contentOriginY + float32(w.height)/2}
	imgui.SetNextWindowPosV(center, imgui.CondAppearing, imgui.Vec2{X: 0.5, Y: 0.5})

	flags := imgui.WindowFlagsAlwaysAutoResize |
		imgui.WindowFlagsNoCollapse |
		imgui.WindowFlagsNoSavedSettings |
		imgui.WindowFlagsNoDocking
	if imgui.BeginV("Rename Tab###renamedlg"+w.imguiSuffix(), nil, flags) {
		imgui.Text("Tab name:")
		imgui.InputTextWithHint("##rename", "tab name", &w.renameBuffer, 0, nil)

		if imgui.IsItemFocused() && imgui.IsKeyPressedBool(imgui.KeyEnter) {
			if tab := w.tabs.Active(); tab != nil {
				tab.Title = w.renameBuffer
			}
			w.renamingTab = false
		}

		if imgui.Button("OK") {
			if tab := w.tabs.Active(); tab != nil {
				tab.Title = w.renameBuffer
			}
			w.renamingTab = false
		}
		imgui.SameLineV(0, 8)
		if imgui.Button("Cancel") {
			w.renamingTab = false
		}
	}
	imgui.End()
}

func (w *Window) handleMouseSelection() {
	tab := w.tabs.Active()
	if tab == nil {
		return
	}

	mousePos := imgui.MousePos()
	col := int((mousePos.X - w.renderer.OffsetX) / w.cellW)
	row := int((mousePos.Y - w.renderer.OffsetY) / w.cellH)

	// Window-local pixel coordinates. ImGui draw lists and MousePos() are in
	// absolute desktop space when multi-viewport is enabled, but w.width /
	// w.height / w.tabBarH / w.searchOverlayW are window-local — subtract
	// the wrapper's content origin to bring them into the same space.
	vpOffX, vpOffY := w.contentOriginX, w.contentOriginY
	wmX := mousePos.X - vpOffX
	wmY := mousePos.Y - vpOffY

	// Clamp to grid bounds
	cols, rows := w.gridSize()
	if col < 0 {
		col = 0
	}
	if col >= cols {
		col = cols - 1
	}
	if row < 0 {
		row = 0
	}
	if row >= rows {
		row = rows - 1
	}

	// Left click starts selection — but only in the terminal area.
	// Explicit rect checks skip the scrollbar column and the search
	// overlay region. The "any other ImGui window is on top" filter
	// used to be CurrentIO().WantCaptureMouse(), but post-multi-window
	// the terminal renders INSIDE the wrapper ImGui window so
	// WantCaptureMouse is ALWAYS true over the terminal — that made
	// selection silently no-op everywhere.
	//
	// IsWindowHovered() (no flags) is the right primitive: returns
	// true only when the wrapper itself is the topmost hovered window
	// — false when a popup or modal (rename, unsafe-paste, the
	// context menu) is over the wrapper, but true when the mouse is
	// over plain terminal cells with nothing on top.
	barW := float32(w.app.cfg.Scrollbar.Width)
	onScrollbar := wmX >= float32(w.width)-barW
	onSearch := tab != nil && w.getScroll(tab.ID).Searching &&
		wmX >= float32(w.width)-w.searchOverlayW &&
		wmY <= w.tabBarH+65
	wrapperHovered := imgui.IsWindowHovered()
	inTerminal := wrapperHovered && wmY >= w.tabBarH && !onScrollbar && !onSearch && !w.sbDragging

	if imgui.IsMouseDoubleClicked(imgui.MouseButtonLeft) && inTerminal {
		// Links.DoubleClick: open the URL under the cursor on
		// double-click. Wins over the word-select default — the user
		// who turned this on wanted clicks on links to navigate, not
		// select. Plain double-click on non-link text falls through
		// to the word/whitespace selection paths.
		if w.hoveredLink != nil && w.app.cfg.Links.Enabled && w.app.cfg.Links.DoubleClick {
			openURL(w.hoveredLink.URL, w.app.cfg.Links.Opener)
			w.lastDblClickTime = imgui.Time()
			w.lastDblClickRow = row
			w.lastDblClickCol = col
			return
		}
		scrollOff := 0
		if s, ok := w.scroll[tab.ID]; ok {
			scrollOff = s.Offset
		}
		cell := cellAtViewport(tab.Terminal.Emulator(), col, row, scrollOff)
		if cell != nil && isSelWordChar(cell.Content) {
			w.sel.selectWord(tab.Terminal.Emulator(), col, row, scrollOff)
		} else {
			w.sel.selectSpace(tab.Terminal.Emulator(), col, row, scrollOff)
		}
		if w.sel.active {
			// iTerm2-style: hold-and-drag after a double-click extends
			// the selection by word, with the original word as the
			// anchor. Release without movement just keeps the word
			// selection.
			w.sel.dragging = true
			w.writeSelection(w.sel.extractText(tab.Terminal.Emulator(), scrollOff, w.app.cfg.Clipboard.TrimTrailingWhitespace))
		}
		w.lastDblClickTime = imgui.Time()
		w.lastDblClickRow = row
		w.lastDblClickCol = col
	} else if imgui.IsMouseClickedBool(imgui.MouseButtonLeft) {
		// Triple-click detection: click shortly after a double-click on the same row
		if inTerminal && imgui.Time()-w.lastDblClickTime < 0.4 && row == w.lastDblClickRow {
			scrollOff := 0
			if s, ok := w.scroll[tab.ID]; ok {
				scrollOff = s.Offset
			}
			w.sel.selectLine(tab.Terminal.Emulator(), row, scrollOff)
			if w.sel.active {
				// Drag after triple-click extends the selection by full rows.
				w.sel.dragging = true
				w.writeSelection(w.sel.extractText(tab.Terminal.Emulator(), scrollOff, w.app.cfg.Clipboard.TrimTrailingWhitespace))
			}
			w.lastDblClickTime = 0 // consumed
		} else if inTerminal {
			w.sel.clear()
			w.sel.startCharDrag(row, col)
		}
	}

	// Dragging extends selection. Mode (set when the drag started)
	// decides whether the moving end snaps to char / word / line.
	if w.sel.dragging && imgui.IsMouseDown(imgui.MouseButtonLeft) {
		scrollOff := 0
		if s, ok := w.scroll[tab.ID]; ok {
			scrollOff = s.Offset
		}
		w.sel.extendDrag(row, col, tab.Terminal.Emulator(), scrollOff)
	}

	// Release finalizes selection and copies to PRIMARY (+ CLIPBOARD
	// when copy_on_select is set).
	if w.sel.dragging && imgui.IsMouseReleased(imgui.MouseButtonLeft) {
		w.sel.dragging = false
		if w.sel.active {
			scrollOff := 0
			if s, ok := w.scroll[tab.ID]; ok {
				scrollOff = s.Offset
			}
			w.writeSelection(w.sel.extractText(tab.Terminal.Emulator(), scrollOff, w.app.cfg.Clipboard.TrimTrailingWhitespace))
		}
	}

	// Middle-click pastes from PRIMARY selection (terminal area only, not on
	// ImGui windows like prefs/search). Gated on Clipboard.PasteOnMiddleClick
	// — when off, middle-click is a no-op (some users rebind their X11
	// middle button or don't want accidental pastes from the primary
	// selection while reading).
	if w.app.cfg.Clipboard.PasteOnMiddleClick && imgui.IsMouseClickedBool(imgui.MouseButtonMiddle) {
		if wmY >= w.tabBarH && wrapperHovered {
			text, err := input.PrimaryRead()
			if err == nil && text != "" {
				w.pasteText(text)
			}
		}
	}
}

// writeSelection publishes selected text to the X11/Wayland PRIMARY
// selection (the middle-click target — Linux convention is to refresh
// it on every selection regardless of preference) and, when
// cfg.Clipboard.CopyOnSelect is set, also to the system clipboard
// (Ctrl+C target). The per-row cell-grid trim is done by
// extractText() (gated on the same pref); this function only handles
// the publish.
//
// Empty / whitespace-only text is dropped: writing it would clobber
// whatever the user previously copied without giving them anything
// useful in return.
func (w *Window) writeSelection(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	input.PrimaryWrite(text)
	if w.app.cfg.Clipboard.CopyOnSelect {
		input.ClipboardWrite(text)
	}
}

func (w *Window) selectedText() string {
	if !w.sel.active {
		return ""
	}
	tab := w.tabs.Active()
	if tab == nil {
		return ""
	}
	scrollOff := 0
	if s, ok := w.scroll[tab.ID]; ok {
		scrollOff = s.Offset
	}
	return w.sel.extractText(tab.Terminal.Emulator(), scrollOff, w.app.cfg.Clipboard.TrimTrailingWhitespace)
}

func getCWD(term interface{}) string {
	t, ok := term.(*terminal.Terminal)
	if !ok {
		return ""
	}
	return t.GetCWD()
}
