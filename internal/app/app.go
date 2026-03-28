// Package app handles the SDL2/ImGui lifecycle and main render loop.
package app

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
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

	fullscreen      bool
	tabBarHovered   bool    // true when mouse is over the tab bar
	tabSwitchReq    int     // tab ID to force-select, -1 = none
	ready           bool    // true after first frame measures fonts
	baseFontSize float32 // font size the atlas was built at
	baseCellW    float32 // cell width at base font size
	baseCellH    float32 // cell height at base font size
	theme        renderer.Theme
	sel          selection // text selection state
	pendingPaste   string  // text awaiting unsafe-paste confirmation
	resizeTime     float64 // imgui.Time() when last resize occurred
	resizeOverlay  bool    // whether to show overlay
	lastCols       int     // cols at last resize check
	lastRows       int     // rows at last resize check
	hoveredLink    *linkHit // URL under mouse cursor, nil if none
	renamingTab    bool     // whether rename popup is open
	renameBuffer   string   // text input for tab rename
}

// New creates a new App with the given config.
func New(cfg config.Config) *App {
	return &App{
		cfg:    cfg,
		width:  800,
		height: 600,
		scroll:      make(map[int]*scrollback.State),
		tabBarH:     0, // starts at 0, set to 25 when >1 tab
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
	bgR := float32((theme.Background >> 0) & 0xFF) / 255.0
	bgG := float32((theme.Background >> 8) & 0xFF) / 255.0
	bgB := float32((theme.Background >> 16) & 0xFF) / 255.0
	a.backend.SetBgColor(imgui.NewVec4(bgR, bgG, bgB, 1.0))
	a.backend.SetTargetFPS(120)

	// Disable viewports — keeps everything in one window
	io := imgui.CurrentIO()
	io.SetConfigFlags(io.ConfigFlags() &^ imgui.ConfigFlagsViewportsEnable)

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

	// Handle window resize
	a.backend.SetSizeChangeCallback(func(w, h int) {
		a.width = w
		a.height = h
		a.resizeTerminals()
		a.resizeTime = imgui.Time()
		a.resizeOverlay = true
	})

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
			a.backend.SetShouldClose(true)
			return
		}
		return
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

	// Scroll-to-bottom on new output (only if not manually scrolled back)
	for _, tab := range a.tabs.Tabs {
		if tab.Dirty {
			tab.Dirty = false
			s := a.getScroll(tab.ID)
			if !s.IsScrolled() {
				s.Reset()
			}
		}
	}

	// Check for closed tabs
	a.tabs.CheckClosed()
	// Auto-close dead tabs if configured
	if a.cfg.Tabs.OnChildExit == "close" {
		for i := len(a.tabs.Tabs) - 1; i >= 0; i-- {
			if a.tabs.Tabs[i].Closed {
				a.tabs.CloseTab(i)
			}
		}
	}

	// Exit if no tabs remain
	if a.tabs.Count() == 0 {
		a.backend.SetShouldClose(true)
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
	a.renderer.OffsetY = a.tabBarH + pad
	// Resize terminals if tab bar visibility changed (available space changed)
	if a.tabBarH != oldTabBarH {
		a.resizeTerminals()
	}

	// Render tab bar
	a.renderTabBar()

	// Render active terminal in a fullscreen ImGui window
	if tab := a.tabs.Active(); tab != nil {
		// Use a fullscreen borderless ImGui window for the terminal area
		imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, imgui.Vec2{})
		imgui.SetNextWindowPosV(imgui.Vec2{X: 0, Y: a.tabBarH}, imgui.CondAlways, imgui.Vec2{})
		imgui.SetNextWindowSizeV(imgui.Vec2{X: float32(a.width), Y: float32(a.height) - a.tabBarH}, imgui.CondAlways)
		wflags := imgui.WindowFlagsNoTitleBar | imgui.WindowFlagsNoResize |
			imgui.WindowFlagsNoMove | imgui.WindowFlagsNoScrollbar |
			imgui.WindowFlagsNoBackground | imgui.WindowFlagsNoInputs
		if imgui.BeginV("##terminal", nil, wflags) {
			drawList := imgui.WindowDrawList()
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
				// doesn't cause stale coordinates
				if s, ok := a.scroll[tab.ID]; ok && s.Searching && s.Query != "" {
					s.Search(tab.Terminal.Emu)
					if len(s.Matches) > 0 {
						a.drawSearchHighlights(s, drawList)
					}
				}

				// Scrollbar
				a.drawScrollbar(tab, scrollOff, drawList)
			}
		}
		imgui.End()
		imgui.PopStyleVar()
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
	return a.renamingTab || a.pendingPaste != ""
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
				case '\r': // Enter
					s.NextMatch()
					if _, rows := a.gridSize(); rows > 0 {
						s.ScrollToCurrentMatch(rows)
					}
					continue
				case '\n': // Shift+Enter
					s.PrevMatch()
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
		if a.fullscreen {
			a.backend.SetWindowFlags(sdlbackend.SDLWindowFlags(0x00000001), 1) // SDL_WINDOW_FULLSCREEN
		} else {
			a.backend.SetWindowFlags(sdlbackend.SDLWindowFlags(0x00000001), 0)
		}
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
	case "search":
		if tab := a.tabs.Active(); tab != nil {
			s := a.getScroll(tab.ID)
			s.OpenSearch()
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

	// Scale cell metrics proportionally (no atlas rebuild needed —
	// the renderer uses AddTextFontPtr with explicit font size)
	scale := fontSize / a.baseFontSize
	a.cellW = a.baseCellW * scale
	a.cellH = a.baseCellH * scale
	a.renderer.Metrics = renderer.CellMetrics{Width: a.cellW, Height: a.cellH}
	a.renderer.FontSize = fontSize

	// Keep window size fixed, just recalculate grid and resize terminals
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

	imgui.SetNextWindowPosV(imgui.Vec2{X: 0, Y: 0}, imgui.CondAlways, imgui.Vec2{})
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

	imgui.SetNextWindowPosV(imgui.Vec2{X: float32(a.width) - 320, Y: a.tabBarH}, imgui.CondAlways, imgui.Vec2{})
	flags := imgui.WindowFlagsNoTitleBar | imgui.WindowFlagsNoResize |
		imgui.WindowFlagsNoMove | imgui.WindowFlagsNoScrollbar | imgui.WindowFlagsAlwaysAutoResize

	if imgui.BeginV("##search", nil, flags) {
		imgui.SetNextItemWidth(180)

		// Focus the input once when search first opens
		if s.SearchFocusOnce {
			imgui.SetKeyboardFocusHere()
			s.SearchFocusOnce = false
		}

		_, rows := a.gridSize()
		changed := imgui.InputTextWithHint("##searchinput", "Search...", &s.Query, 0, nil)
		if changed {
			s.Search(tab.Terminal.Emu)
			s.ScrollToCurrentMatch(rows)
		}

		imgui.SameLineV(0, 4)
		if len(s.Matches) > 0 {
			imgui.Text(fmt.Sprintf("%d/%d", s.MatchIdx+1, len(s.Matches)))
		} else if s.Query != "" {
			imgui.Text("0")
		}

		imgui.SameLineV(0, 4)
		if imgui.ButtonV("<", imgui.Vec2{X: 20, Y: 0}) {
			s.PrevMatch()
			s.ScrollToCurrentMatch(rows)
		}
		imgui.SameLineV(0, 2)
		if imgui.ButtonV(">", imgui.Vec2{X: 20, Y: 0}) {
			s.NextMatch()
			s.ScrollToCurrentMatch(rows)
		}
		imgui.SameLineV(0, 2)
		if imgui.ButtonV("X", imgui.Vec2{X: 20, Y: 0}) {
			s.CloseSearch()
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
	duration := 1.5 // total display time in seconds
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
	termH := float32(rows) * a.cellH
	barX := float32(a.width) - barW
	barY := a.tabBarH

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
	if imgui.IsMouseDown(0) && hovered {
		if mpos.Y < thumbY {
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

	// Drag thumb: map mouse Y to scroll offset
	if imgui.IsMouseDragging(0) && mpos.X >= barX && mpos.X <= barX+barW {
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

	// Left click starts selection
	if imgui.IsMouseClickedBool(imgui.MouseButtonLeft) {
		// Only start selection in terminal area (below tab bar)
		if mousePos.Y >= a.tabBarH {
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
