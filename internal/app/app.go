// Package app handles the SDL2/ImGui lifecycle and main render loop.
package app

import (
	"fmt"
	"github.com/AllenDang/cimgui-go/backend"
	"github.com/AllenDang/cimgui-go/backend/sdlbackend"
	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/fontsys"
	"github.com/LXXero/xerotty/internal/glyphcache"
	"github.com/LXXero/xerotty/internal/input"
	"github.com/LXXero/xerotty/internal/menu"
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
)

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
	cfg              config.Config
	theme            renderer.Theme
	baseFontSize     float32 // font size the atlas was built at
	baseCellW        float32 // cell width at base font size
	baseCellH        float32 // cell height at base font size
	pendingFontFace  bool    // rebuild font atlas at start of next frame
	pendingRemeasure bool    // re-run cell measurement next frame (e.g. after font swap)

	windows []*Window // every OS window currently open in this process
	active  *Window   // the window with input focus, or windows[0] if none
}

// New creates a new App with the given config. The initial Window
// struct is allocated here; its SDL_Window, GL context, tabs, and
// renderer are populated during Run() once the cimgui-go backend is
// initialized.
func New(cfg config.Config) *App {
	a := &App{cfg: cfg}
	w := newWindow(a)
	a.windows = append(a.windows, w)
	a.active = w
	return a
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
	wp := int(math.Ceil(float64(float32(cols)*estCellW + pad + cellSafetyMargin)))
	hp := int(math.Ceil(float64(float32(rows)*estCellH + pad + cellSafetyMargin)))
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

	w.backend, _ = backend.CreateBackend(sdlbackend.NewSDLBackend())

	// Font reload runs here — BEFORE every frame's NewFrame. The
	// alternative (doing it inside our frame() callback after NewFrame
	// already snapshotted the current font) leaves a freed font in
	// ImGui's per-frame state and asserts when Render dereferences it.
	w.backend.SetBeforeRenderHook(a.beforeRender)

	w.width, w.height = w.initialWindowSize()
	w.backend.CreateWindow("xerotty", w.width, w.height)

	// Cascade child windows: when spawned via new_window, parent passes
	// XEROTTY_WIN_X/Y so each new xerotty appears offset from its predecessor
	// instead of stacking exactly on top.
	if xs, ys := os.Getenv("XEROTTY_WIN_X"), os.Getenv("XEROTTY_WIN_Y"); xs != "" && ys != "" {
		if x, errX := strconv.Atoi(xs); errX == nil {
			if y, errY := strconv.Atoi(ys); errY == nil {
				if sb, ok := w.backend.(*sdlbackend.SDLBackend); ok {
					sb.SetWindowPos(x, y)
				}
			}
		}
	}

	// Set background color from theme (ABGR → RGBA for SDL)
	bgR := float32((theme.Background>>0)&0xFF) / 255.0
	bgG := float32((theme.Background>>8)&0xFF) / 255.0
	bgB := float32((theme.Background>>16)&0xFF) / 255.0
	w.backend.SetBgColor(imgui.NewVec4(bgR, bgG, bgB, 1.0))
	w.backend.SetTargetFPS(120)

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
	// Phase 3 note: this still renders only a.active per frame.
	// Phase 4 will iterate a.windows here so additional Windows opened
	// via new_window get rendered too.
	wrappedFrame := func() {
		liveResizeMainFrameBegin()
		defer liveResizeMainFrameEnd()
		if a.active != nil {
			a.active.frame()
		}
		setEventLoopIdleTimeout(a.idleTimeout())
	}
	installLiveResizeWatch(bgR, bgG, bgB, wrappedFrame, a.beforeRender)

	// Wake the main loop when any PTY produces new data. Without this the
	// loop sleeps in WaitEventTimeout until the next timeout (cursor
	// blink etc.) and the user sees up to ~500ms latency on incoming
	// bytes.
	terminal.Wake = wakeEventLoop

	// Main loop — event-driven via SDL_WaitEventTimeout. CPU goes to
	// kernel-sleep when nothing's happening; PTY arrival, mouse, keys,
	// timers all wake it.
	runEventLoop(bgR, bgG, bgB, wrappedFrame, a.beforeRender, nil)

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

// cellSafetyMargin reserves a few extra pixels of right/bottom gutter beyond
// what the cell count strictly needs. CalcTextSize returns the font's advance
// width, but font hinting / anti-aliasing can nudge glyph edge pixels slightly
// past cellW. Without this margin, when the floor-fitted cell count produces
// a tight gutter, the rightmost character's AA edge can render over the
// window boundary and appear clipped before the count reduces.
// Must be added BACK to the window size in any code that wants to fit a
// specific cols×rows grid (e.g. font zoom resize), otherwise the next gridSize
// call will floor away one cell.
const cellSafetyMargin = 8

func (w *Window) gridSize() (cols, rows int) {
	pad := float32(w.app.cfg.Appearance.Padding) * 2 // padding on both sides
	availW := float32(w.width) - pad - cellSafetyMargin
	availH := float32(w.height) - w.tabBarH - pad - cellSafetyMargin
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

// idleTimeout returns the maximum milliseconds the WaitEventTimeout
// can sleep before forcing a render. The runloop only wakes early on
// real SDL events (mouse, keyboard, window) or PTY-data Wake signals
// — this is the *fallback* tick for animations and periodic side
// effects. Active interaction or fade animation: 60Hz. Cursor blink:
// the configured blink rate. Truly idle: a long safety wake.
//
// Aggregates over every Window — if any of them has a drag or fade
// in progress, we tick at 60 Hz so that Window stays smooth even if
// the user is currently focused on a different one.
func (a *App) idleTimeout() int {
	for _, w := range a.windows {
		if w.sel.dragging || w.sbDragging || w.resizeOverlay ||
			imgui.IsMouseDown(imgui.MouseButtonLeft) {
			return 16
		}
	}
	if a.pendingFontFace || a.pendingRemeasure {
		return 16
	}
	if a.cfg.Appearance.CursorBlink {
		rate := a.cfg.Appearance.BlinkRate
		if rate <= 0 {
			rate = 530
		}
		return rate
	}
	return 5000
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
				if w.renderer == nil || w.backend == nil {
					continue
				}
				if c, err := glyphcache.New(fontsys.Default, w.backend, primaryPath, a.baseFontSize, fbScale); err == nil {
					w.renderer.Glyphs = c
				}
			}
		}
	}
	a.pendingRemeasure = true
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
	if runtime.GOOS == "darwin" {
		osDown := sdlhack.LeftButtonGlobalDown()
		imguiDown := imgui.IsMouseDown(imgui.MouseButtonLeft)
		switch {
		case osDown && !imguiDown:
			if sdlhack.MouseInMainContent() && !inLiveResizeWatch() {
				imgui.CurrentIO().AddMouseButtonEvent(int32(imgui.MouseButtonLeft), true)
			}
		case !osDown && imguiDown:
			imgui.CurrentIO().AddMouseButtonEvent(int32(imgui.MouseButtonLeft), false)
		}
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
				if c, err := glyphcache.New(fontsys.Default, a.backend, primaryPath, a.app.baseFontSize, fbScale); err == nil {
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
		desiredW := int(math.Ceil(float64(float32(cfgCols)*a.cellW + pad + cellSafetyMargin)))
		desiredH := int(math.Ceil(float64(float32(cfgRows)*a.cellH + pad + a.tabBarH + cellSafetyMargin)))
		if desiredW != a.width || desiredH != a.height {
			a.backend.SetWindowSize(desiredW, desiredH)
			a.width = desiredW
			a.height = desiredH
			a.skipDisplaySync = 2
			sdlRaiseWindow()
		}

		// Snap drag-resize to (cellW, cellH) so the window only resizes at
		// cell boundaries. macOS only — see cellsnap_darwin.m. No-op
		// elsewhere.
		setContentResizeIncrements(a.cellW, a.cellH)

		if _, err := a.tabs.NewTab(cfgCols, cfgRows); err != nil {
			sdlQuit()
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
	// Done once, then resize terminals to fit the new cell dimensions.
	if a.app.pendingRemeasure {
		a.app.pendingRemeasure = false
		if metrics := a.measureCell(); metrics.Width >= 1 && metrics.Height >= 1 {
			a.app.baseCellW = metrics.Width
			a.app.baseCellH = metrics.Height
			a.cellW, a.cellH = ceilCell(metrics.Width, metrics.Height)
			a.renderer.Metrics = renderer.CellMetrics{Width: a.cellW, Height: a.cellH}
			a.renderer.FontSize = a.app.baseFontSize
			a.resizeTerminals()
			setContentResizeIncrements(a.cellW, a.cellH)
		}
	}

	// Sync window dimensions from ImGui IO every frame — more reliable than
	// SetSizeChangeCallback which some WMs/compositors don't always trigger.
	// Skip for a few frames after we issue SetWindowSize: DisplaySize lags the
	// WM by 1-2 frames, so a fresh shrink request would otherwise be clobbered
	// by the stale (pre-resize) DisplaySize value.
	if a.skipDisplaySync > 0 {
		a.skipDisplaySync--
	} else if ds := imgui.CurrentIO().DisplaySize(); int(ds.X) > 0 && int(ds.Y) > 0 {
		newW, newH := int(ds.X), int(ds.Y)
		if newW != a.width || newH != a.height {
			a.width = newW
			a.height = newH
			a.resizeTerminals()
			a.resizeTime = imgui.Time()
			a.resizeOverlay = true
			a.resizeOverlayText = "" // drag-resize: live cols×rows
		}
	}

	// Handle scroll wheel: tab bar = switch tabs, Ctrl+scroll = zoom, plain scroll = scrollback
	wheel := imgui.CurrentIO().MouseWheel()
	if wheel != 0 {
		var vpOffY float32
		if vp := a.viewport(); vp != nil {
			vpOffY = vp.Pos().Y
		}
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
			if wheel > 0 {
				a.app.cfg.Font.Size += 1
				a.updateFontMetrics()
			} else if a.app.cfg.Font.Size > 6 {
				a.app.cfg.Font.Size -= 1
				a.updateFontMetrics()
			}
		} else if tab := a.tabs.Active(); tab != nil {
			s := a.getScroll(tab.ID)
			scrollLines := a.app.cfg.Scrollback.ScrollSpeed
			if scrollLines <= 0 {
				scrollLines = 3
			}
			if wheel > 0 {
				s.ScrollUp(scrollLines, tab.Terminal.Emu.ScrollbackLen())
			} else {
				s.ScrollDown(scrollLines)
			}
		}
	}

	// Handle mouse selection
	a.handleMouseSelection()

	// Detect links under mouse cursor
	a.hoveredLink = nil
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
			a.hoveredLink = detectLinkAt(tab.Terminal.Emu, col, row, scrollOff)

			// Ctrl+click opens link
			if a.hoveredLink != nil && a.app.cfg.Links.CtrlClick && imgui.IsKeyDown(imgui.ModCtrl) && imgui.IsMouseClickedBool(imgui.MouseButtonLeft) {
				openURL(a.hoveredLink.URL, a.app.cfg.Links.Opener)
			}
		}
	}

	// Drain data notifications
	a.tabs.DrainData()

	// Scroll handling on new output:
	// - If at live position: stay at bottom (auto-scroll)
	// - If scrolled back: freeze viewport by adjusting offset for new scrollback lines
	for _, tab := range a.tabs.Tabs {
		if tab.Dirty {
			tab.Dirty = false
			s := a.getScroll(tab.ID)
			sbLen := tab.Terminal.Emu.ScrollbackLen()
			if s.IsScrolled() && s.PrevSBLen > 0 {
				delta := sbLen - s.PrevSBLen
				if delta > 0 {
					s.Offset += delta
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
			if tab.Terminal.ExitCode == 0 {
				a.tabs.CloseTab(i)
			}
			// Non-zero exit: keep tab open so user can see output
		default:
			a.tabs.CloseTab(i)
		}
	}

	// Exit if no tabs remain — push SDL_QUIT since SetShouldClose is unimplemented.
	if a.tabs.Count() == 0 {
		sdlQuit()
		return
	}

	// Process queued key events
	a.processKeys()

	// Update tab bar height based on tab count and current font size.
	// imgui.FrameHeight() = FontSize + style.FramePadding.Y*2, which is
	// exactly the vertical space a tab item needs. Add a few px so the
	// active-tab underline (drawn at the bottom edge) isn't clipped.
	oldTabBarH := a.tabBarH
	if a.tabs.Count() > 1 {
		a.tabBarH = imgui.FrameHeight() + 2
	} else {
		a.tabBarH = 0
	}
	pad := float32(a.app.cfg.Appearance.Padding)
	// Add the window's viewport offset so terminal lands inside the SDL
	// window when multi-viewport is enabled (ImGui draw lists are in
	// absolute desktop coords).
	var vpOffX, vpOffY float32
	if vp := a.viewport(); vp != nil {
		vpOffX, vpOffY = vp.Pos().X, vp.Pos().Y
	}
	// Snap render offsets to whole pixels so glyphs don't get sub-pixel-drifted
	// between rows. Without this, half-block characters (▀ ▄) and continuous
	// lines (─ │) can develop visible gaps between rows because their AA
	// rendering shifts at fractional positions. A resize "fixes" this only
	// because the new offsets happen to land cleanly — pixel-snapping makes
	// every offset land cleanly.
	a.renderer.OffsetX = float32(math.Floor(float64(vpOffX + pad)))
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
				a.backend.SetWindowSize(a.width, newH)
				a.height = newH
				a.skipDisplaySync = 2
				sdlRaiseWindow()
			}
		}
		a.resizeTerminals()
	}

	// Render tab bar
	a.renderTabBar()

	// Render active terminal directly onto the window's background draw
	// list. Using a wrapping ImGui window breaks under multi-viewport
	// (the window can be promoted to its own viewport, leaving the SDL
	// surface blank). BackgroundDrawListV pinned to this Window's
	// viewport guarantees draws hit the right OS window.
	if tab := a.tabs.Active(); tab != nil {
		drawList := a.bgDrawList()
		if drawList != nil {
			scrollOff := 0
			if s, ok := a.scroll[tab.ID]; ok {
				scrollOff = s.Offset
			}
			a.renderer.Draw(tab.Terminal.Emu, drawList, scrollOff)

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
					pos := tab.Terminal.Emu.CursorPosition()
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
				s.Search(tab.Terminal.Emu, visRows)
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
	if imgui.IsMouseClickedBool(imgui.MouseButtonRight) {
		imgui.OpenPopupStr("##contextmenu")
		io := imgui.CurrentIO()
		owned := io.MouseDownOwned()
		owned[imgui.MouseButtonRight] = true
		io.SetMouseDownOwned(&owned)
		ownedAlt := io.MouseDownOwnedUnlessPopupClose()
		ownedAlt[imgui.MouseButtonRight] = true
		io.SetMouseDownOwnedUnlessPopupClose(&ownedAlt)
		imgui.InternalSetHoveredID(imgui.ID(1))
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
	events := input.PollKeys(w.app.cfg.Keybinds, appCursor)
	actionDispatched := false

	for _, ev := range events {
		// During search, handle Escape and Enter specially
		if searchInputFocused && tab != nil {
			s := w.getScroll(tab.ID)
			if ev.Action == "" && len(ev.Bytes) > 0 {
				switch ev.Bytes[0] {
				case 0x1b: // Escape
					if len(ev.Bytes) == 1 {
						s.CloseSearch()
						searching = false
						searchInputFocused = false
						w.searchInputFocused = false
						continue
					}
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
			if s, ok := w.scroll[tab.ID]; ok {
				s.Reset()
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
			// Snap to bottom on text input
			if s, ok := w.scroll[tab.ID]; ok {
				s.Reset()
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
		if tab, err := w.tabs.NewTab(cols, rows); err == nil && tab != nil {
			// AutoSelectNewTabs only catches new tabs once the bar has prior
			// frame state. On the 1→2 transition (tab bar first appears) it
			// can't, so request an explicit switch to the new tab.
			w.tabSwitchReq = tab.ID
		}
	case "close_tab":
		w.tabs.CloseActive()
	case "new_window":
		exe, err := os.Executable()
		if err == nil {
			// Cascade: place child ~30px below and right of parent so it
			// doesn't stack exactly on top.
			var envExtras []string
			if sb, ok := w.backend.(*sdlbackend.SDLBackend); ok {
				px, py := sb.GetWindowPos()
				envExtras = []string{
					fmt.Sprintf("XEROTTY_WIN_X=%d", int(px)+30),
					fmt.Sprintf("XEROTTY_WIN_Y=%d", int(py)+30),
				}
			}
			// On macOS inside a .app bundle, routes through `open -n`
			// so LaunchServices coalesces all xerotty processes under
			// one Dock icon. See newwindow_darwin.go.
			newWindowCommand(exe, envExtras).Start()
		}
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
		sdlSetFullscreen(w.fullscreen)
	case "scroll_page_up":
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			_, rows := w.gridSize()
			s.PageUp(rows, tab.Terminal.Emu.ScrollbackLen())
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
			s.Offset = tab.Terminal.Emu.ScrollbackLen()
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
		w.app.cfg.Font.Size += 1
		w.updateFontMetrics()
	case "font_size_down":
		if w.app.cfg.Font.Size > 6 {
			w.app.cfg.Font.Size -= 1
			w.updateFontMetrics()
		}
	case "font_size_reset":
		w.app.cfg.Font.Size = 14
		w.updateFontMetrics()
	case "select_all":
		if tab := w.tabs.Active(); tab != nil {
			cols := tab.Terminal.Emu.Width()
			rows := tab.Terminal.Emu.Height()
			w.sel.startCol = 0
			w.sel.startRow = 0
			w.sel.endCol = cols - 1
			w.sel.endRow = rows - 1
			w.sel.active = true
			w.sel.dragging = false
		}
	case "clear_scrollback":
		if tab := w.tabs.Active(); tab != nil {
			tab.Terminal.Emu.ClearScrollback()
			if s, ok := w.scroll[tab.ID]; ok {
				s.Reset()
			}
		}
	case "reset_terminal":
		if tab := w.tabs.Active(); tab != nil {
			// Send RIS (Reset to Initial State) escape sequence
			tab.Terminal.Write([]byte("\x1bc"))
			tab.Terminal.Emu.ClearScrollback()
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
			w.renameBuffer = tab.Title
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
				w.renderer.Theme = t
				w.app.theme = t
				// Update SDL background color to match new theme — both
				// the cimgui-go backend (used briefly during init before
				// runEventLoop takes over) and our runloop's clear color.
				bgR := float32((t.Background>>0)&0xFF) / 255.0
				bgG := float32((t.Background>>8)&0xFF) / 255.0
				bgB := float32((t.Background>>16)&0xFF) / 255.0
				w.backend.SetBgColor(imgui.NewVec4(bgR, bgG, bgB, 1.0))
				updateEventLoopBg(bgR, bgG, bgB)
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
	pxSize := renderer.PixelSize(&w.app.cfg)
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
			if c, err := glyphcache.New(fontsys.Default, w.backend, primaryPath, pxSize, fbScale); err == nil {
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
	newW := int(math.Ceil(float64(float32(cols)*w.cellW + pad + cellSafetyMargin)))
	newH := int(math.Ceil(float64(float32(rows)*w.cellH + pad + w.tabBarH + cellSafetyMargin)))
	w.backend.SetWindowSize(newW, newH)
	w.width = newW
	w.height = newH
	// Don't let the next 2 frames of stale DisplaySize undo this shrink.
	w.skipDisplaySync = 2
	// Some WMs drop input focus across the unmap/remap of SetWindowSize.
	sdlRaiseWindow()
	w.resizeTerminals()
	// Update the macOS resize-increment to the new cell size so subsequent
	// drag-resizes stay on the cell grid.
	setContentResizeIncrements(w.cellW, w.cellH)

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

	var vpX, vpY float32
	if vp := w.viewport(); vp != nil {
		vpX, vpY = vp.Pos().X, vp.Pos().Y
	}
	imgui.SetNextWindowPosV(imgui.Vec2{X: vpX, Y: vpY}, imgui.CondAlways, imgui.Vec2{})
	// Asymmetric size update to keep auto-repeat fast in both directions:
	//   - On GROW: defer the tab bar size update while the SDL window is in
	//     mid-resize (skipDisplaySync > 0). Stacking SetNextWindowSizeV on
	//     top of in-flight Wayland configure cycles starves keyboard
	//     auto-repeat. Safe to defer because the tab bar's old (smaller)
	//     size still fits inside the new (larger) viewport, so multi-viewport
	//     doesn't promote it to its own platform window.
	//   - On SHRINK: always update immediately. If we deferred, the tab bar's
	//     old (larger) size would exceed the new (smaller) viewport, and
	//     multi-viewport WOULD promote it to its own platform window —
	//     triggering another configure cycle per shrink. Wayland handles
	//     shrink configures fast, so the immediate path is cheap here.
	curW, curH := float32(w.width), w.tabBarH
	sizeChanged := curW != w.lastTabBarW || curH != w.lastTabBarH
	isShrink := curW < w.lastTabBarW
	if sizeChanged && (isShrink || w.skipDisplaySync == 0) {
		imgui.SetNextWindowSizeV(imgui.Vec2{X: curW, Y: curH}, imgui.CondAlways)
		w.lastTabBarW = curW
		w.lastTabBarH = curH
	}
	// The tab bar is purely a click target — it should never take keyboard
	// focus. With multiple tabs the bar is the only ImGui window that exists,
	// so any focus event (initial appearance, click, post-resize re-evaluation)
	// would otherwise land on it and set WantCaptureKeyboard, which makes
	// processKeys drop terminal input until the user clicks back on the
	// terminal area. Explicitly opt out of every focus path:
	//   NoFocusOnAppearing  — first 1→2-tab transition doesn't grab focus
	//   NoNav               — no keyboard/gamepad nav targets here
	//   NoBringToFrontOnFocus — clicking a tab doesn't promote the bar to focus
	flags := imgui.WindowFlagsNoTitleBar | imgui.WindowFlagsNoResize |
		imgui.WindowFlagsNoMove | imgui.WindowFlagsNoScrollbar |
		imgui.WindowFlagsNoScrollWithMouse | imgui.WindowFlagsNoBackground |
		imgui.WindowFlagsNoFocusOnAppearing | imgui.WindowFlagsNoNav |
		imgui.WindowFlagsNoBringToFrontOnFocus

	innerSpX := imgui.CurrentStyle().ItemInnerSpacing().X
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, imgui.Vec2{X: 0, Y: 0})
	imgui.PushStyleVarVec2(imgui.StyleVarItemInnerSpacing, imgui.Vec2{X: innerSpX, Y: 0})
	defer imgui.PopStyleVarV(2)

	if imgui.BeginV("##tabbar", nil, flags) {
		tabFlags := imgui.TabBarFlagsReorderable | imgui.TabBarFlagsAutoSelectNewTabs
		if imgui.BeginTabBarV("tabs", tabFlags) {
			for i, tab := range w.tabs.Tabs {
				label := tab.Title
				if label == "" {
					label = fmt.Sprintf("shell %d", tab.ID)
				}
				label = fmt.Sprintf("%s###tab%d", label, tab.ID)

				open := true
				itemFlags := imgui.TabItemFlags(0)
				if w.tabSwitchReq == tab.ID {
					itemFlags = imgui.TabItemFlagsSetSelected
					w.tabSwitchReq = -1
				}
				if imgui.BeginTabItemV(label, &open, itemFlags) {
					w.tabs.ActiveIdx = i
					imgui.EndTabItem()
				}
				if !open {
					w.tabs.CloseTab(i)
					break // tab slice mutated
				}
			}
			imgui.EndTabBar()
		}
	}
	imgui.End()
}

func (w *Window) renderContextMenu() {
	ctx := w.menuContext()
	action := menu.Render(w.app.cfg.Menu.Items, ctx)
	if action != "" {
		w.dispatchAction(action)
	}
}

func (w *Window) menuContext() *menu.Context {
	ctx := &menu.Context{
		HasSelection: w.sel.active,
		Selection:    w.selectedText(),
	}
	if tab := w.tabs.Active(); tab != nil {
		ctx.TabTitle = tab.Title
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

	var vpX, vpY float32
	if vp := w.viewport(); vp != nil {
		vpX, vpY = vp.Pos().X, vp.Pos().Y
	}
	imgui.SetNextWindowPosV(imgui.Vec2{X: vpX + float32(w.width) - 320, Y: vpY + w.tabBarH}, imgui.CondAlways, imgui.Vec2{})
	flags := imgui.WindowFlagsNoTitleBar | imgui.WindowFlagsNoResize |
		imgui.WindowFlagsNoMove | imgui.WindowFlagsNoScrollbar | imgui.WindowFlagsAlwaysAutoResize

	w.searchInputFocused = false
	if imgui.BeginV("##search", nil, flags) {
		// Track actual rendered width for the selection hit-test.
		w.searchOverlayW = imgui.WindowWidth()

		imgui.SetNextItemWidth(180)

		// Re-focus the input when requested (on open, after < > clicks, or
		// when it loses focus to the terminal).  Guard with mouse-idle so
		// SetKeyboardFocusHere never fires while a button click is in
		// progress — it would steal ActiveId and swallow the click.
		if w.searchFocusInput && !imgui.IsMouseDown(0) && !imgui.IsMouseReleased(imgui.MouseButtonLeft) {
			imgui.SetKeyboardFocusHere()
			w.searchFocusInput = false
		}

		_, rows := w.gridSize()
		prevQuery := s.Query
		changed := imgui.InputTextWithHint("##searchinput", "Search...", &s.Query, 0, nil)
		w.searchInputFocused = imgui.IsItemFocused()
		if changed && s.Query != prevQuery {
			s.Search(tab.Terminal.Emu, rows)
			s.ScrollToCurrentMatch(rows)
		}
		imgui.SameLineV(0, 4)
		if len(s.Matches) > 0 {
			imgui.Text(fmt.Sprintf("%d/%d", s.MatchIdx+1, len(s.Matches)))
		} else if s.Query != "" {
			imgui.Text("0")
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
			s.Search(tab.Terminal.Emu, rows)
			s.ScrollToCurrentMatch(rows)
		}
	}
	imgui.End()
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

	elapsed := imgui.Time() - w.resizeTime
	duration := 1.5  // total display time in seconds
	fadeStart := 1.0 // start fading at this point

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

	sbLen := tab.Terminal.Emu.ScrollbackLen()
	_, rows := w.gridSize()
	totalLines := sbLen + rows

	// auto mode: only show when scrolled back
	if vis == "auto" && scrollOff == 0 {
		return
	}

	barW := float32(w.app.cfg.Scrollbar.Width)
	var vpOffX, vpOffY float32
	if vp := w.viewport(); vp != nil {
		vpOffX, vpOffY = vp.Pos().X, vp.Pos().Y
	}
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

	center := imgui.Vec2{X: float32(w.width) / 2, Y: float32(w.height) / 2}
	imgui.SetNextWindowPosV(center, imgui.CondAppearing, imgui.Vec2{X: 0.5, Y: 0.5})

	if imgui.BeginPopupModalV("Unsafe Paste", nil, imgui.WindowFlagsAlwaysAutoResize) {
		lines := strings.Count(w.pendingPaste, "\n") + 1
		imgui.Text(fmt.Sprintf("Paste %d lines into terminal?", lines))
		imgui.Text("Multi-line paste may execute commands.")
		imgui.TextDisabled("Enter = paste   Esc = cancel")
		imgui.Separator()

		// Keyboard shortcuts: Enter / KeypadEnter = accept (paste),
		// Esc = cancel. Both close the popup. The button row is the
		// authoritative path; this just makes the common case fast.
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
			imgui.CloseCurrentPopup()
		} else if cancel {
			w.pendingPaste = ""
			imgui.CloseCurrentPopup()
		}
		imgui.EndPopup()
	}
}

func (w *Window) renderRenameDialog() {
	if !w.renamingTab {
		return
	}

	center := imgui.Vec2{X: float32(w.width) / 2, Y: float32(w.height) / 2}
	imgui.SetNextWindowPosV(center, imgui.CondAppearing, imgui.Vec2{X: 0.5, Y: 0.5})

	if imgui.BeginPopupModalV("Rename Tab", nil, imgui.WindowFlagsAlwaysAutoResize) {
		imgui.Text("Tab name:")
		imgui.InputTextWithHint("##rename", "tab name", &w.renameBuffer, 0, nil)

		if imgui.IsItemFocused() && imgui.IsKeyPressedBool(imgui.KeyEnter) {
			if tab := w.tabs.Active(); tab != nil {
				tab.Title = w.renameBuffer
			}
			w.renamingTab = false
			imgui.CloseCurrentPopup()
		}

		if imgui.Button("OK") {
			if tab := w.tabs.Active(); tab != nil {
				tab.Title = w.renameBuffer
			}
			w.renamingTab = false
			imgui.CloseCurrentPopup()
		}
		imgui.SameLineV(0, 8)
		if imgui.Button("Cancel") {
			w.renamingTab = false
			imgui.CloseCurrentPopup()
		}
		imgui.EndPopup()
	}
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
	// the main viewport position to bring them into the same space.
	var vpOffX, vpOffY float32
	if vp := w.viewport(); vp != nil {
		vpOffX, vpOffY = vp.Pos().X, vp.Pos().Y
	}
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
	// IsAnyItemHovered() is reset by ImGui's NewFrame() and is always 0 here
	// (before any items are rendered this frame), so we use explicit rect
	// checks instead: skip the scrollbar column and the search overlay region.
	// WantCaptureMouse is true when the cursor is over any ImGui window
	// (prefs, popups, tab bar) — that catches the catch-all case so a click
	// on the prefs window doesn't seed a phantom terminal selection.
	barW := float32(w.app.cfg.Scrollbar.Width)
	onScrollbar := wmX >= float32(w.width)-barW
	onSearch := tab != nil && w.getScroll(tab.ID).Searching &&
		wmX >= float32(w.width)-w.searchOverlayW &&
		wmY <= w.tabBarH+65
	imguiCaptured := imgui.CurrentIO().WantCaptureMouse()
	inTerminal := wmY >= w.tabBarH && !onScrollbar && !onSearch && !w.sbDragging && !imguiCaptured

	if imgui.IsMouseDoubleClicked(imgui.MouseButtonLeft) && inTerminal {
		scrollOff := 0
		if s, ok := w.scroll[tab.ID]; ok {
			scrollOff = s.Offset
		}
		cell := cellAtViewport(tab.Terminal.Emu, col, row, scrollOff)
		if cell != nil && isSelWordChar(cell.Content) {
			w.sel.selectWord(tab.Terminal.Emu, col, row, scrollOff)
		} else {
			w.sel.selectSpace(tab.Terminal.Emu, col, row, scrollOff)
		}
		if w.sel.active {
			// iTerm2-style: hold-and-drag after a double-click extends
			// the selection by word, with the original word as the
			// anchor. Release without movement just keeps the word
			// selection.
			w.sel.dragging = true
			text := w.sel.extractText(tab.Terminal.Emu, scrollOff)
			if text != "" {
				input.PrimaryWrite(text)
			}
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
			w.sel.selectLine(tab.Terminal.Emu, row, scrollOff)
			if w.sel.active {
				// Drag after triple-click extends the selection by full rows.
				w.sel.dragging = true
				text := w.sel.extractText(tab.Terminal.Emu, scrollOff)
				if text != "" {
					input.PrimaryWrite(text)
				}
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
		w.sel.extendDrag(row, col, tab.Terminal.Emu, scrollOff)
	}

	// Release finalizes selection and copies to PRIMARY
	if w.sel.dragging && imgui.IsMouseReleased(imgui.MouseButtonLeft) {
		w.sel.dragging = false
		if w.sel.active {
			scrollOff := 0
			if s, ok := w.scroll[tab.ID]; ok {
				scrollOff = s.Offset
			}
			text := w.sel.extractText(tab.Terminal.Emu, scrollOff)
			if text != "" {
				input.PrimaryWrite(text)
			}
		}
	}

	// Middle-click pastes from PRIMARY selection (terminal area only, not on
	// ImGui windows like prefs/search).
	if imgui.IsMouseClickedBool(imgui.MouseButtonMiddle) {
		if wmY >= w.tabBarH && !imguiCaptured {
			text, err := input.PrimaryRead()
			if err == nil && text != "" {
				w.pasteText(text)
			}
		}
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
	return w.sel.extractText(tab.Terminal.Emu, scrollOff)
}

func getCWD(term interface{}) string {
	t, ok := term.(*terminal.Terminal)
	if !ok {
		return ""
	}
	return t.GetCWD()
}
