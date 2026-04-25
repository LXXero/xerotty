// Package app handles the SDL2/ImGui lifecycle and main render loop.
package app

import (
	"fmt"
	"github.com/AllenDang/cimgui-go/backend"
	"github.com/AllenDang/cimgui-go/backend/sdlbackend"
	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/input"
	"github.com/LXXero/xerotty/internal/menu"
	"github.com/LXXero/xerotty/internal/renderer"
	"github.com/LXXero/xerotty/internal/scrollback"
	"github.com/LXXero/xerotty/internal/tabs"
	"github.com/LXXero/xerotty/internal/themes"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

func init() {
	runtime.LockOSThread()
}

// App is the main application struct.
type App struct {
	cfg      config.Config
	backend  backend.Backend[sdlbackend.SDLWindowFlags]
	tabs     *tabs.Manager
	renderer *renderer.Renderer
	scroll   map[int]*scrollback.State // per-tab scroll state
	width    int
	height   int
	cellW    float32
	cellH    float32
	tabBarH  float32

	fullscreen       bool
	tabBarHovered    bool    // true when mouse is over the tab bar
	tabSwitchReq     int     // tab ID to force-select, -1 = none
	ready            bool    // true after first frame measures fonts
	baseFontSize     float32 // font size the atlas was built at
	baseCellW        float32 // cell width at base font size
	baseCellH        float32 // cell height at base font size
	theme            renderer.Theme
	sel              selection    // text selection state
	pendingPaste     string       // text awaiting unsafe-paste confirmation
	resizeTime       float64      // imgui.Time() when last resize occurred
	resizeOverlay    bool         // whether to show overlay
	lastCols         int          // cols at last resize check
	lastRows         int          // rows at last resize check
	hoveredLink      *linkHit     // URL under mouse cursor, nil if none
	renamingTab      bool         // whether rename popup is open
	renameBuffer     string       // text input for tab rename
	sbDragging       bool         // true while dragging the scrollbar thumb
	searchFocusInput bool         // request keyboard focus to search input next frame
	searchOverlayW   float32      // actual rendered width of search overlay (updated each frame)
	lastDblClickTime float64      // imgui.Time() of last double-click (for triple-click detection)
	lastDblClickRow  int          // row of last double-click
	lastDblClickCol  int          // col of last double-click
	prefDialog       configDialog // preferences dialog state
}

// New creates a new App with the given config.
func New(cfg config.Config) *App {
	return &App{
		cfg:          cfg,
		width:        800,
		height:       600,
		scroll:       make(map[int]*scrollback.State),
		tabBarH:      0, // starts at 0, set to 25 when >1 tab
		tabSwitchReq: -1,
	}
}

// Run starts the application main loop.
func (a *App) Run() error {
	// Load theme
	theme, err := themes.Load(a.cfg.Appearance.Theme)
	if err != nil {
		theme = renderer.DefaultTheme()
	}
	a.theme = theme

	a.backend, _ = backend.CreateBackend(sdlbackend.NewSDLBackend())

	a.backend.CreateWindow("xerotty", a.width, a.height)

	// Set background color from theme (ABGR → RGBA for SDL)
	bgR := float32((theme.Background>>0)&0xFF) / 255.0
	bgG := float32((theme.Background>>8)&0xFF) / 255.0
	bgB := float32((theme.Background>>16)&0xFF) / 255.0
	a.backend.SetBgColor(imgui.NewVec4(bgR, bgG, bgB, 1.0))
	a.backend.SetTargetFPS(120)

	// Keep viewports enabled so selected UI panels can use native WM windows.
	io := imgui.CurrentIO()
	io.SetConfigFlags(io.ConfigFlags() | imgui.ConfigFlagsViewportsEnable)

	// Load font into atlas (must be after CreateWindow, before first frame)
	font := renderer.LoadFont(&a.cfg)

	// Approximate metrics until first frame measures real ones
	fontSize := a.cfg.Font.Size
	if fontSize <= 0 {
		fontSize = 14
	}
	a.baseFontSize = fontSize
	a.cellW = fontSize * 0.6
	a.cellH = fontSize

	// Create renderer (metrics will be updated on first frame)
	a.renderer = renderer.New(theme, renderer.CellMetrics{
		Width: a.cellW, Height: a.cellH,
	}, font, fontSize)
	pad := float32(a.cfg.Appearance.Padding)
	a.renderer.OffsetX = pad
	a.renderer.OffsetY = a.tabBarH + pad
	// vpOffsetX/Y are added each frame so coords match MainViewport position
	// when ConfigFlagsViewportsEnable is on (draws are in desktop space).

	// Window resize is handled per-frame via ImGui IO.DisplaySize in frame().

	// Tab manager (terminal creation deferred to first frame for accurate metrics)
	a.tabs = tabs.NewManager(&a.cfg)

	// Main loop
	a.backend.Run(func() {
		a.frame()
	})

	// Cleanup all tabs
	for _, tab := range a.tabs.Tabs {
		tab.Terminal.Close()
	}

	return nil
}

func (a *App) gridSize() (cols, rows int) {
	pad := float32(a.cfg.Appearance.Padding) * 2 // padding on both sides
	availH := float32(a.height) - a.tabBarH - pad
	cols = int((float32(a.width) - pad) / a.cellW)
	rows = int(availH / a.cellH)
	if cols < 2 {
		cols = 2
	}
	if rows < 2 {
		rows = 2
	}
	return
}

func (a *App) resizeTerminals() {
	cols, rows := a.gridSize()
	for _, tab := range a.tabs.Tabs {
		tab.Terminal.Resize(cols, rows)
	}
}

func (a *App) frame() {
	// First frame: measure font metrics and create terminal
	if !a.ready {
		a.ready = true

		// Measure real cell dimensions now that the font atlas is built
		metrics := renderer.MeasureCell()
		if metrics.Width < 1 || metrics.Height < 1 {
			// Fallback if measurement fails
			fs := a.cfg.Font.Size
			if fs <= 0 {
				fs = 14
			}
			metrics = renderer.CellMetrics{Width: fs * 0.6, Height: fs * 1.2}
		}
		a.cellW = metrics.Width
		a.cellH = metrics.Height
		a.baseCellW = metrics.Width
		a.baseCellH = metrics.Height
		a.renderer.Metrics = metrics

		cols, rows := a.gridSize()
		if _, err := a.tabs.NewTab(cols, rows); err != nil {
			sdlQuit()
			return
		}
		return
	}

	// Sync window dimensions from ImGui IO every frame — more reliable than
	// SetSizeChangeCallback which some WMs/compositors don't always trigger.
	if ds := imgui.CurrentIO().DisplaySize(); int(ds.X) > 0 && int(ds.Y) > 0 {
		newW, newH := int(ds.X), int(ds.Y)
		if newW != a.width || newH != a.height {
			a.width = newW
			a.height = newH
			a.resizeTerminals()
			a.resizeTime = imgui.Time()
			a.resizeOverlay = true
		}
	}

	// Handle scroll wheel: tab bar = switch tabs, Ctrl+scroll = zoom, plain scroll = scrollback
	wheel := imgui.CurrentIO().MouseWheel()
	if wheel != 0 {
		if a.tabBarH > 0 && imgui.MousePos().Y < a.tabBarH {
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
				a.cfg.Font.Size += 1
				a.updateFontMetrics()
			} else if a.cfg.Font.Size > 6 {
				a.cfg.Font.Size -= 1
				a.updateFontMetrics()
			}
		} else if tab := a.tabs.Active(); tab != nil {
			s := a.getScroll(tab.ID)
			scrollLines := a.cfg.Scrollback.ScrollSpeed
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
			if a.hoveredLink != nil && a.cfg.Links.CtrlClick && imgui.IsKeyDown(imgui.ModCtrl) && imgui.IsMouseClickedBool(imgui.MouseButtonLeft) {
				openURL(a.hoveredLink.URL, a.cfg.Links.Opener)
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
		switch a.cfg.Tabs.OnChildExit {
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

	// Update tab bar height based on tab count
	oldTabBarH := a.tabBarH
	if a.tabs.Count() > 1 {
		a.tabBarH = 25
	} else {
		a.tabBarH = 0
	}
	pad := float32(a.cfg.Appearance.Padding)
	// Add MainViewport offset so terminal lands inside the SDL window when
	// multi-viewport is enabled (ImGui draw lists are in absolute desktop coords).
	var vpOffX, vpOffY float32
	if vp := imgui.MainViewport(); vp != nil {
		vpOffX, vpOffY = vp.Pos().X, vp.Pos().Y
	}
	a.renderer.OffsetX = vpOffX + pad
	a.renderer.OffsetY = vpOffY + a.tabBarH + pad
	// Resize terminals if tab bar visibility changed (available space changed)
	if a.tabBarH != oldTabBarH {
		a.resizeTerminals()
	}

	// Render tab bar
	a.renderTabBar()

	// Render active terminal directly onto the main viewport's background
	// draw list. Using a wrapping ImGui window breaks under multi-viewport
	// (the window can be promoted to its own viewport, leaving the SDL
	// surface blank). BackgroundDrawListV pinned to MainViewport guarantees
	// draws hit the primary SDL window.
	if tab := a.tabs.Active(); tab != nil {
		drawList := imgui.BackgroundDrawListV(imgui.MainViewport())
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
				if a.cfg.Appearance.CursorBlink {
					rate := float64(a.cfg.Appearance.BlinkRate) / 1000.0
					if rate <= 0 {
						rate = 0.53
					}
					showCursor = int(imgui.Time()/rate)%2 == 0
				}
				if showCursor {
					pos := tab.Terminal.Emu.CursorPosition()
					a.renderer.DrawCursor(struct{ X, Y int }{pos.X, pos.Y},
						a.cfg.Appearance.CursorStyle, drawList)
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

	// Context menu — open on right-click (manual detection because terminal window has NoInputs)
	if imgui.IsMouseClickedBool(imgui.MouseButtonRight) {
		imgui.OpenPopupStr("##contextmenu")
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

func (a *App) isSearching() bool {
	if tab := a.tabs.Active(); tab != nil {
		if s, ok := a.scroll[tab.ID]; ok {
			return s.Searching
		}
	}
	return false
}

func (a *App) popupActive() bool {
	return a.renamingTab || a.pendingPaste != "" || a.prefDialog.open
}

func (a *App) processKeys() {
	tab := a.tabs.Active()
	searching := a.isSearching()
	popupOpen := a.popupActive()

	// When a popup/modal is active, don't process any keys for the terminal.
	// ImGui handles input for its own widgets.
	if popupOpen {
		return
	}

	// Poll ImGui key state (SDL backend's SetKeyCallback is not implemented)
	events := input.PollKeys(a.cfg.Keybinds, false)
	actionDispatched := false

	for _, ev := range events {
		// During search, handle Escape and Enter specially
		if searching && tab != nil {
			s := a.getScroll(tab.ID)
			if ev.Action == "" && len(ev.Bytes) > 0 {
				switch ev.Bytes[0] {
				case 0x1b: // Escape
					if len(ev.Bytes) == 1 {
						s.CloseSearch()
						searching = false
						continue
					}
				case '\r': // Enter — go up toward older content
					s.PrevMatch()
					if _, rows := a.gridSize(); rows > 0 {
						s.ScrollToCurrentMatch(rows)
					}
					continue
				case '\n': // Shift+Enter — go down toward newer content
					s.NextMatch()
					if _, rows := a.gridSize(); rows > 0 {
						s.ScrollToCurrentMatch(rows)
					}
					continue
				}
			}
		}

		if ev.Action != "" {
			a.dispatchAction(ev.Action)
			actionDispatched = true
			continue
		}

		// Don't forward key bytes to terminal during search
		if searching {
			continue
		}

		if len(ev.Bytes) > 0 && tab != nil {
			if s, ok := a.scroll[tab.ID]; ok {
				s.Reset()
			}
			a.sel.clear() // typing clears selection
			tab.Terminal.Write(ev.Bytes)
		}
	}

	// Don't forward text input when: searching, a keybind just fired,
	// ImGui wants keyboard, or Ctrl is held (avoids leaking chars from Ctrl+key combos)
	if searching || actionDispatched || imgui.CurrentIO().WantTextInput() || imgui.IsKeyDown(imgui.ModCtrl) {
		return
	}

	// Read text input from ImGui's character queue (SDL_TEXTINPUT events)
	if tab != nil {
		io := imgui.CurrentIO()
		chars := io.InputQueueCharacters()
		if chars.Size > 0 {
			// Snap to bottom on text input
			if s, ok := a.scroll[tab.ID]; ok {
				s.Reset()
			}
			a.sel.clear()
			for _, ch := range chars.Slice() {
				if ch > 0 && ch < 0x10FFFF {
					buf := make([]byte, 4)
					n := encodeRune(buf, rune(ch))
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

func (a *App) dispatchAction(action string) {
	switch action {
	case "new_tab":
		cols, rows := a.gridSize()
		a.tabs.NewTab(cols, rows)
	case "close_tab":
		a.tabs.CloseActive()
	case "new_window":
		exe, err := os.Executable()
		if err == nil {
			cmd := exec.Command(exe)
			cmd.Start()
		}
	case "next_tab":
		a.tabs.Next()
	case "prev_tab":
		a.tabs.Prev()
	case "copy":
		text := a.selectedText()
		if text != "" {
			input.ClipboardWrite(text)
		}
	case "paste":
		text, err := input.ClipboardRead()
		if err == nil && text != "" {
			a.pasteText(text)
		}
	case "paste_selection":
		text, err := input.PrimaryRead()
		if err == nil && text != "" {
			a.pasteText(text)
		}
	case "fullscreen":
		a.fullscreen = !a.fullscreen
		sdlSetFullscreen(a.fullscreen)
	case "scroll_page_up":
		if tab := a.tabs.Active(); tab != nil {
			s := a.getScroll(tab.ID)
			_, rows := a.gridSize()
			s.PageUp(rows, tab.Terminal.Emu.ScrollbackLen())
		}
	case "scroll_page_down":
		if tab := a.tabs.Active(); tab != nil {
			s := a.getScroll(tab.ID)
			_, rows := a.gridSize()
			s.PageDown(rows)
		}
	case "scroll_top":
		if tab := a.tabs.Active(); tab != nil {
			s := a.getScroll(tab.ID)
			s.Offset = tab.Terminal.Emu.ScrollbackLen()
		}
	case "scroll_bottom":
		if tab := a.tabs.Active(); tab != nil {
			s := a.getScroll(tab.ID)
			s.Reset()
		}
	case "search":
		if tab := a.tabs.Active(); tab != nil {
			s := a.getScroll(tab.ID)
			s.OpenSearch()
			a.searchFocusInput = true
		}
	case "font_size_up":
		a.cfg.Font.Size += 1
		a.updateFontMetrics()
	case "font_size_down":
		if a.cfg.Font.Size > 6 {
			a.cfg.Font.Size -= 1
			a.updateFontMetrics()
		}
	case "font_size_reset":
		a.cfg.Font.Size = 14
		a.updateFontMetrics()
	case "select_all":
		if tab := a.tabs.Active(); tab != nil {
			cols := tab.Terminal.Emu.Width()
			rows := tab.Terminal.Emu.Height()
			a.sel.startCol = 0
			a.sel.startRow = 0
			a.sel.endCol = cols - 1
			a.sel.endRow = rows - 1
			a.sel.active = true
			a.sel.dragging = false
		}
	case "clear_scrollback":
		if tab := a.tabs.Active(); tab != nil {
			tab.Terminal.Emu.ClearScrollback()
			if s, ok := a.scroll[tab.ID]; ok {
				s.Reset()
			}
		}
	case "reset_terminal":
		if tab := a.tabs.Active(); tab != nil {
			// Send RIS (Reset to Initial State) escape sequence
			tab.Terminal.Write([]byte("\x1bc"))
			tab.Terminal.Emu.ClearScrollback()
			if s, ok := a.scroll[tab.ID]; ok {
				s.Reset()
			}
			a.sel.clear()
		}
	case "open_link":
		if a.hoveredLink != nil {
			openURL(a.hoveredLink.URL, a.cfg.Links.Opener)
		}
	case "copy_link":
		if a.hoveredLink != nil {
			input.ClipboardWrite(a.hoveredLink.URL)
		}
	case "rename_tab":
		if tab := a.tabs.Active(); tab != nil {
			a.renameBuffer = tab.Title
			a.renamingTab = true
			imgui.OpenPopupStr("Rename Tab")
		}
	case "preferences":
		a.openPreferences()
	default:
		// Check for parameterized actions
		if strings.HasPrefix(action, "goto_tab:") {
			nStr := strings.TrimPrefix(action, "goto_tab:")
			if n, err := strconv.Atoi(nStr); err == nil {
				a.tabs.GoTo(n)
			}
		} else if strings.HasPrefix(action, "set_theme:") {
			name := strings.TrimPrefix(action, "set_theme:")
			if t, err := themes.Load(name); err == nil {
				a.renderer.Theme = t
				a.theme = t
				// Update SDL background color to match new theme
				bgR := float32((t.Background>>0)&0xFF) / 255.0
				bgG := float32((t.Background>>8)&0xFF) / 255.0
				bgB := float32((t.Background>>16)&0xFF) / 255.0
				a.backend.SetBgColor(imgui.NewVec4(bgR, bgG, bgB, 1.0))
			}
		} else if strings.HasPrefix(action, "exec:") {
			ctx := a.menuContext()
			menu.ExecAction(action, ctx)
		}
	}
}

func (a *App) getScroll(tabID int) *scrollback.State {
	if s, ok := a.scroll[tabID]; ok {
		return s
	}
	s := scrollback.New()
	a.scroll[tabID] = s
	return s
}

func (a *App) updateFontMetrics() {
	fontSize := a.cfg.Font.Size
	if fontSize <= 0 {
		fontSize = 14
	}
	if a.baseFontSize <= 0 {
		a.baseFontSize = 14
	}

	// Capture current grid dimensions BEFORE scaling so we can resize
	// the window to keep the same number of cols/rows.
	cols, rows := a.gridSize()

	// Scale cell metrics proportionally (no atlas rebuild needed —
	// the renderer uses AddTextFontPtr with explicit font size)
	scale := fontSize / a.baseFontSize
	a.cellW = a.baseCellW * scale
	a.cellH = a.baseCellH * scale
	a.renderer.Metrics = renderer.CellMetrics{Width: a.cellW, Height: a.cellH}
	a.renderer.FontSize = fontSize

	// Resize window to maintain the same grid at the new cell size.
	// Set a.width/a.height immediately so this frame renders correctly;
	// the per-frame DisplaySize sync (line ~188) will correct them on the
	// next frame if the WM didn't honour the request.
	pad := float32(a.cfg.Appearance.Padding) * 2
	newW := int(math.Ceil(float64(float32(cols)*a.cellW + pad)))
	newH := int(math.Ceil(float64(float32(rows)*a.cellH + pad + a.tabBarH)))
	a.backend.SetWindowSize(newW, newH)
	a.width = newW
	a.height = newH
	a.resizeTerminals()

	// Show resize overlay so user sees the new grid dimensions
	a.resizeTime = imgui.Time()
	a.resizeOverlay = true
}

func (a *App) renderTabBar() {
	a.tabBarHovered = false
	if a.tabs.Count() <= 1 {
		return // Don't show tab bar with single tab
	}

	var vpX, vpY float32
	if vp := imgui.MainViewport(); vp != nil {
		vpX, vpY = vp.Pos().X, vp.Pos().Y
	}
	imgui.SetNextWindowPosV(imgui.Vec2{X: vpX, Y: vpY}, imgui.CondAlways, imgui.Vec2{})
	imgui.SetNextWindowSizeV(imgui.Vec2{X: float32(a.width), Y: a.tabBarH}, imgui.CondAlways)
	flags := imgui.WindowFlagsNoTitleBar | imgui.WindowFlagsNoResize |
		imgui.WindowFlagsNoMove | imgui.WindowFlagsNoScrollbar |
		imgui.WindowFlagsNoScrollWithMouse | imgui.WindowFlagsNoBackground

	if imgui.BeginV("##tabbar", nil, flags) {
		tabFlags := imgui.TabBarFlagsReorderable | imgui.TabBarFlagsAutoSelectNewTabs
		if imgui.BeginTabBarV("tabs", tabFlags) {
			for i, tab := range a.tabs.Tabs {
				label := tab.Title
				if label == "" {
					label = fmt.Sprintf("shell %d", tab.ID)
				}
				label = fmt.Sprintf("%s###tab%d", label, tab.ID)

				open := true
				itemFlags := imgui.TabItemFlags(0)
				if a.tabSwitchReq == tab.ID {
					itemFlags = imgui.TabItemFlagsSetSelected
					a.tabSwitchReq = -1
				}
				if imgui.BeginTabItemV(label, &open, itemFlags) {
					a.tabs.ActiveIdx = i
					imgui.EndTabItem()
				}
				if !open {
					a.tabs.CloseTab(i)
					break // tab slice mutated
				}
			}
			imgui.EndTabBar()
		}
	}
	imgui.End()
}

func (a *App) renderContextMenu() {
	ctx := a.menuContext()
	action := menu.Render(a.cfg.Menu.Items, ctx)
	if action != "" {
		a.dispatchAction(action)
	}
}

func (a *App) menuContext() *menu.Context {
	ctx := &menu.Context{
		HasSelection: a.sel.active,
		Selection:    a.selectedText(),
	}
	if tab := a.tabs.Active(); tab != nil {
		ctx.TabTitle = tab.Title
		// CWD detection via /proc
		if tab.Terminal != nil {
			ctx.CWD = getCWD(tab.Terminal)
		}
	}
	if a.hoveredLink != nil {
		ctx.HasLink = true
		ctx.Link = a.hoveredLink.URL
	}
	return ctx
}

func (a *App) renderSearchOverlay() {
	tab := a.tabs.Active()
	if tab == nil {
		return
	}
	s := a.getScroll(tab.ID)
	if !s.Searching {
		return
	}

	var vpX, vpY float32
	if vp := imgui.MainViewport(); vp != nil {
		vpX, vpY = vp.Pos().X, vp.Pos().Y
	}
	imgui.SetNextWindowPosV(imgui.Vec2{X: vpX + float32(a.width) - 320, Y: vpY + a.tabBarH}, imgui.CondAlways, imgui.Vec2{})
	flags := imgui.WindowFlagsNoTitleBar | imgui.WindowFlagsNoResize |
		imgui.WindowFlagsNoMove | imgui.WindowFlagsNoScrollbar | imgui.WindowFlagsAlwaysAutoResize

	if imgui.BeginV("##search", nil, flags) {
		// Track actual rendered width for the selection hit-test.
		a.searchOverlayW = imgui.WindowWidth()

		imgui.SetNextItemWidth(180)

		// Re-focus the input when requested (on open, after < > clicks, or
		// when it loses focus to the terminal).  Guard with mouse-idle so
		// SetKeyboardFocusHere never fires while a button click is in
		// progress — it would steal ActiveId and swallow the click.
		if a.searchFocusInput && !imgui.IsMouseDown(0) && !imgui.IsMouseReleased(imgui.MouseButtonLeft) {
			imgui.SetKeyboardFocusHere()
			a.searchFocusInput = false
		}

		_, rows := a.gridSize()
		prevQuery := s.Query
		changed := imgui.InputTextWithHint("##searchinput", "Search...", &s.Query, 0, nil)
		if changed && s.Query != prevQuery {
			s.Search(tab.Terminal.Emu, rows)
			s.ScrollToCurrentMatch(rows)
		}
		// Schedule re-focus if input lost focus (e.g. clicked terminal).
		// The mouse-idle guard above ensures this won't fire mid-click.
		if !imgui.IsItemFocused() {
			a.searchFocusInput = true
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
			a.searchFocusInput = true
		}
		if nextClicked {
			s.NextMatch()
			s.ScrollToCurrentMatch(rows)
			a.searchFocusInput = true
		}
		if closeClicked {
			s.CloseSearch()
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

func (a *App) drawSearchHighlights(s *scrollback.State, drawList *imgui.DrawList) {
	cellW := a.cellW
	cellH := a.cellH
	_, rows := a.gridSize()
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

		x := a.renderer.OffsetX + float32(m.Col)*cellW
		y := a.renderer.OffsetY + float32(screenRow)*cellH
		w := float32(m.Len) * cellW

		bg := matchBg
		if i == s.MatchIdx {
			bg = currentBg
		}

		drawList.AddRectFilled(
			imgui.Vec2{X: x, Y: y},
			imgui.Vec2{X: x + w, Y: y + cellH},
			bg,
		)
	}
}

func (a *App) renderResizeOverlay() {
	if !a.resizeOverlay {
		return
	}

	elapsed := imgui.Time() - a.resizeTime
	duration := 1.5  // total display time in seconds
	fadeStart := 1.0 // start fading at this point

	if elapsed > duration {
		a.resizeOverlay = false
		return
	}

	cols, rows := a.gridSize()
	text := fmt.Sprintf("%d × %d", cols, rows)
	textSize := imgui.CalcTextSize(text)

	padX := float32(16)
	padY := float32(10)
	boxW := textSize.X + padX*2
	boxH := textSize.Y + padY*2

	cx := float32(a.width) / 2
	cy := float32(a.height) / 2

	// Fade out alpha
	alpha := float32(1.0)
	if elapsed > fadeStart {
		alpha = float32(1.0 - (elapsed-fadeStart)/(duration-fadeStart))
	}

	bgColor := uint32(uint8(alpha*180)) << 24 // semi-transparent black
	fgColor := uint32(0x00FFFFFF) | (uint32(uint8(alpha*255)) << 24)

	dl := imgui.ForegroundDrawListViewportPtr()
	dl.AddRectFilledV(
		imgui.Vec2{X: cx - boxW/2, Y: cy - boxH/2},
		imgui.Vec2{X: cx + boxW/2, Y: cy + boxH/2},
		bgColor, 6, 0,
	)
	dl.AddTextVec2(
		imgui.Vec2{X: cx - textSize.X/2, Y: cy - textSize.Y/2},
		fgColor,
		text,
	)
}

func (a *App) drawScrollbar(tab *tabs.Tab, scrollOff int, drawList *imgui.DrawList) {
	vis := a.cfg.Scrollbar.Visible
	if vis == "never" {
		return
	}

	sbLen := tab.Terminal.Emu.ScrollbackLen()
	_, rows := a.gridSize()
	totalLines := sbLen + rows

	// auto mode: only show when scrolled back
	if vis == "auto" && scrollOff == 0 {
		return
	}

	barW := float32(a.cfg.Scrollbar.Width)
	barX := float32(a.width) - barW
	barY := a.tabBarH
	termH := float32(a.height) - barY // full height below tab bar

	// Check if mouse is hovering the thumb
	mpos := imgui.MousePos()
	hovered := mpos.X >= barX && mpos.X <= barX+barW && mpos.Y >= barY && mpos.Y <= barY+termH

	thumbY, thumbH := a.renderer.DrawScrollbar(renderer.ScrollbarParams{
		X:              barX,
		Y:              barY,
		Width:          barW,
		Height:         termH,
		ScrollOffset:   scrollOff,
		TotalLines:     totalLines,
		VisibleLines:   rows,
		MinThumbHeight: float32(a.cfg.Scrollbar.MinThumbHeight),
		Hovered:        hovered,
	}, drawList)

	// Handle scrollbar click-drag
	if imgui.IsMouseClickedBool(0) && hovered {
		if mpos.Y >= thumbY && mpos.Y <= thumbY+thumbH {
			// Click ON the thumb — start drag
			a.sbDragging = true
		} else if mpos.Y < thumbY {
			// Click above thumb: page up
			if s, ok := a.scroll[tab.ID]; ok {
				s.PageUp(rows, sbLen)
			}
		} else if mpos.Y > thumbY+thumbH {
			// Click below thumb: page down
			if s, ok := a.scroll[tab.ID]; ok {
				s.PageDown(rows)
			}
		}
	}

	// End scrollbar drag on mouse release
	if !imgui.IsMouseDown(0) {
		a.sbDragging = false
	}

	// Drag thumb: map mouse Y to scroll offset.
	// Once dragging starts, track Y regardless of X (user may drift sideways).
	if a.sbDragging {
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
			if s, ok := a.scroll[tab.ID]; ok {
				s.Offset = newOff
			}
		}
	}
}

func (a *App) pasteText(text string) {
	tab := a.tabs.Active()
	if tab == nil {
		return
	}
	// Show confirmation for multi-line paste or text containing sudo
	if strings.Contains(text, "\n") || strings.Contains(text, "sudo") {
		a.pendingPaste = text
		imgui.OpenPopupStr("Unsafe Paste")
		return
	}
	tab.Terminal.Write([]byte(text))
}

func (a *App) renderPasteDialog() {
	if a.pendingPaste == "" {
		return
	}

	center := imgui.Vec2{X: float32(a.width) / 2, Y: float32(a.height) / 2}
	imgui.SetNextWindowPosV(center, imgui.CondAppearing, imgui.Vec2{X: 0.5, Y: 0.5})

	if imgui.BeginPopupModalV("Unsafe Paste", nil, imgui.WindowFlagsAlwaysAutoResize) {
		lines := strings.Count(a.pendingPaste, "\n") + 1
		imgui.Text(fmt.Sprintf("Paste %d lines into terminal?", lines))
		imgui.Text("Multi-line paste may execute commands.")
		imgui.Separator()

		if imgui.Button("Paste") {
			if tab := a.tabs.Active(); tab != nil {
				tab.Terminal.Write([]byte(a.pendingPaste))
			}
			a.pendingPaste = ""
			imgui.CloseCurrentPopup()
		}
		imgui.SameLineV(0, 8)
		if imgui.Button("Cancel") {
			a.pendingPaste = ""
			imgui.CloseCurrentPopup()
		}
		imgui.EndPopup()
	}
}

func (a *App) renderRenameDialog() {
	if !a.renamingTab {
		return
	}

	center := imgui.Vec2{X: float32(a.width) / 2, Y: float32(a.height) / 2}
	imgui.SetNextWindowPosV(center, imgui.CondAppearing, imgui.Vec2{X: 0.5, Y: 0.5})

	if imgui.BeginPopupModalV("Rename Tab", nil, imgui.WindowFlagsAlwaysAutoResize) {
		imgui.Text("Tab name:")
		imgui.InputTextWithHint("##rename", "tab name", &a.renameBuffer, 0, nil)

		if imgui.IsItemFocused() && imgui.IsKeyPressedBool(imgui.KeyEnter) {
			if tab := a.tabs.Active(); tab != nil {
				tab.Title = a.renameBuffer
			}
			a.renamingTab = false
			imgui.CloseCurrentPopup()
		}

		if imgui.Button("OK") {
			if tab := a.tabs.Active(); tab != nil {
				tab.Title = a.renameBuffer
			}
			a.renamingTab = false
			imgui.CloseCurrentPopup()
		}
		imgui.SameLineV(0, 8)
		if imgui.Button("Cancel") {
			a.renamingTab = false
			imgui.CloseCurrentPopup()
		}
		imgui.EndPopup()
	}
}

func (a *App) handleMouseSelection() {
	tab := a.tabs.Active()
	if tab == nil {
		return
	}

	mousePos := imgui.MousePos()
	col := int((mousePos.X - a.renderer.OffsetX) / a.cellW)
	row := int((mousePos.Y - a.renderer.OffsetY) / a.cellH)

	// Clamp to grid bounds
	cols, rows := a.gridSize()
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
	barW := float32(a.cfg.Scrollbar.Width)
	onScrollbar := mousePos.X >= float32(a.width)-barW
	onSearch := tab != nil && a.getScroll(tab.ID).Searching &&
		mousePos.X >= float32(a.width)-a.searchOverlayW &&
		mousePos.Y <= a.tabBarH+65
	inTerminal := mousePos.Y >= a.tabBarH && !onScrollbar && !onSearch && !a.sbDragging

	if imgui.IsMouseDoubleClicked(imgui.MouseButtonLeft) && inTerminal {
		scrollOff := 0
		if s, ok := a.scroll[tab.ID]; ok {
			scrollOff = s.Offset
		}
		cell := cellAtViewport(tab.Terminal.Emu, col, row, scrollOff)
		if cell != nil && isSelWordChar(cell.Content) {
			a.sel.selectWord(tab.Terminal.Emu, col, row, scrollOff)
		} else {
			a.sel.selectSpace(tab.Terminal.Emu, col, row, scrollOff)
		}
		if a.sel.active {
			text := a.sel.extractText(tab.Terminal.Emu, scrollOff)
			if text != "" {
				input.PrimaryWrite(text)
			}
		}
		a.lastDblClickTime = imgui.Time()
		a.lastDblClickRow = row
		a.lastDblClickCol = col
	} else if imgui.IsMouseClickedBool(imgui.MouseButtonLeft) {
		// Triple-click detection: click shortly after a double-click on the same row
		if inTerminal && imgui.Time()-a.lastDblClickTime < 0.4 && row == a.lastDblClickRow {
			scrollOff := 0
			if s, ok := a.scroll[tab.ID]; ok {
				scrollOff = s.Offset
			}
			a.sel.selectLine(tab.Terminal.Emu, row, scrollOff)
			if a.sel.active {
				text := a.sel.extractText(tab.Terminal.Emu, scrollOff)
				if text != "" {
					input.PrimaryWrite(text)
				}
			}
			a.lastDblClickTime = 0 // consumed
		} else if inTerminal {
			a.sel.clear()
			a.sel.startCol = col
			a.sel.startRow = row
			a.sel.endCol = col
			a.sel.endRow = row
			a.sel.dragging = true
		}
	}

	// Dragging extends selection
	if a.sel.dragging && imgui.IsMouseDown(imgui.MouseButtonLeft) {
		a.sel.endCol = col
		a.sel.endRow = row
		// Mark active as soon as we've moved at least one cell
		if a.sel.endCol != a.sel.startCol || a.sel.endRow != a.sel.startRow {
			a.sel.active = true
		}
	}

	// Release finalizes selection and copies to PRIMARY
	if a.sel.dragging && imgui.IsMouseReleased(imgui.MouseButtonLeft) {
		a.sel.dragging = false
		if a.sel.active {
			scrollOff := 0
			if s, ok := a.scroll[tab.ID]; ok {
				scrollOff = s.Offset
			}
			text := a.sel.extractText(tab.Terminal.Emu, scrollOff)
			if text != "" {
				input.PrimaryWrite(text)
			}
		}
	}

	// Middle-click pastes from PRIMARY selection
	if imgui.IsMouseClickedBool(imgui.MouseButtonMiddle) {
		if mousePos.Y >= a.tabBarH {
			text, err := input.PrimaryRead()
			if err == nil && text != "" {
				a.pasteText(text)
			}
		}
	}
}

func (a *App) selectedText() string {
	if !a.sel.active {
		return ""
	}
	tab := a.tabs.Active()
	if tab == nil {
		return ""
	}
	scrollOff := 0
	if s, ok := a.scroll[tab.ID]; ok {
		scrollOff = s.Offset
	}
	return a.sel.extractText(tab.Terminal.Emu, scrollOff)
}

func getCWD(term interface{}) string {
	// Would need the child PID to read /proc/<pid>/cwd
	// Placeholder for now
	return ""
}
