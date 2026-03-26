// Package app handles the SDL2/ImGui lifecycle and main render loop.
package app

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
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
	backend  *sdlbackend.SDLBackend
	tabs     *tabs.Manager
	renderer *renderer.Renderer
	scroll   map[int]*scrollback.State // per-tab scroll state
	width    int
	height   int
	cellW    float32
	cellH    float32
	tabBarH  float32

	// Text input buffer from SDL
	textInput string
	keyQueue  []keyPress

	fullscreen bool
	ready      bool // true after first frame measures fonts
	theme      renderer.Theme
}

type keyPress struct {
	key, mods int
	text      string
}

// New creates a new App with the given config.
func New(cfg config.Config) *App {
	return &App{
		cfg:    cfg,
		width:  800,
		height: 600,
		scroll: make(map[int]*scrollback.State),
		tabBarH: 25,
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

	a.backend = sdlbackend.NewSDLBackend()

	// Set background color from theme (ABGR → RGBA for SDL)
	bgR := float32((theme.Background >> 0) & 0xFF) / 255.0
	bgG := float32((theme.Background >> 8) & 0xFF) / 255.0
	bgB := float32((theme.Background >> 16) & 0xFF) / 255.0
	a.backend.SetBgColor(imgui.Vec4{X: bgR, Y: bgG, Z: bgB, W: 1.0})
	a.backend.CreateWindow("xerotty", a.width, a.height)
	a.backend.SetTargetFPS(120)

	// Load font into atlas (must be after CreateWindow, before first frame)
	font := renderer.LoadFont(&a.cfg)

	// Approximate metrics until first frame measures real ones
	fontSize := a.cfg.Font.Size
	if fontSize <= 0 {
		fontSize = 14
	}
	a.cellW = fontSize * 0.6
	a.cellH = fontSize

	// Create renderer (metrics will be updated on first frame)
	a.renderer = renderer.New(theme, renderer.CellMetrics{
		Width: a.cellW, Height: a.cellH,
	}, font)
	a.renderer.OffsetY = a.tabBarH

	// Set up key callback — queues presses for processing in frame()
	a.backend.SetKeyCallback(func(key, scancode, action, mods int) {
		if action == 1 || action == 2 { // pressed or repeat
			a.keyQueue = append(a.keyQueue, keyPress{key: key, mods: mods})
		}
	})

	// Handle window resize
	a.backend.SetSizeChangeCallback(func(w, h int) {
		a.width = w
		a.height = h
		a.resizeTerminals()
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
	availH := float32(a.height) - a.tabBarH
	cols = int(float32(a.width) / a.cellW)
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
	// Push our loaded font so all ImGui text ops use it
	fontSize := a.cfg.Font.Size
	if fontSize <= 0 {
		fontSize = 14
	}
	if a.renderer != nil && a.renderer.Font != nil {
		imgui.PushFont(a.renderer.Font, fontSize)
		defer imgui.PopFont()
	}

	// First frame: measure real font metrics and create initial terminal
	if !a.ready {
		a.ready = true
		metrics := renderer.MeasureCell()
		a.cellW = metrics.Width
		a.cellH = metrics.Height
		a.renderer.Metrics = metrics
		fmt.Fprintf(os.Stderr, "xerotty: font cell=%.1fx%.1f grid=%dx%d\n",
			a.cellW, a.cellH, int(float32(a.width)/a.cellW), int((float32(a.height)-a.tabBarH)/a.cellH))

		// Now create initial tab with accurate grid size
		cols, rows := a.gridSize()
		if _, err := a.tabs.NewTab(cols, rows); err != nil {
			a.backend.SetShouldClose(true)
			return
		}
		return // skip this frame, render starts next frame
	}

	// Drain data notifications
	a.tabs.DrainData()

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

	// Render tab bar
	a.renderTabBar()

	// Render active terminal
	if tab := a.tabs.Active(); tab != nil {
		drawList := imgui.BackgroundDrawList()
		if drawList != nil {
			a.renderer.Draw(tab.Terminal.Emu, drawList)

			// Cursor
			pos := tab.Terminal.Emu.CursorPosition()
			a.renderer.DrawCursor(struct{ X, Y int }{pos.X, pos.Y},
				a.cfg.Appearance.CursorStyle, drawList)
		}
	}

	// Search overlay
	a.renderSearchOverlay()

	// Context menu
	a.renderContextMenu()
}

func (a *App) isSearching() bool {
	if tab := a.tabs.Active(); tab != nil {
		if s, ok := a.scroll[tab.ID]; ok {
			return s.Searching
		}
	}
	return false
}

func (a *App) processKeys() {
	tab := a.tabs.Active()
	searching := a.isSearching()

	// Process special key events (arrows, function keys, ctrl combos)
	for _, kp := range a.keyQueue {
		// During search, handle Escape and Enter specially
		if searching {
			if tab != nil {
				s := a.getScroll(tab.ID)
				switch {
				case kp.key == 256: // Escape
					s.CloseSearch()
					searching = false
					continue
				case kp.key == 257: // Enter
					if kp.mods&1 != 0 { // Shift+Enter = prev
						s.PrevMatch()
					} else {
						s.NextMatch()
					}
					continue
				}
			}
		}

		ev := input.Translate(kp.key, kp.mods, kp.text, a.cfg.Keybinds, false)

		if ev.Action != "" {
			a.dispatchAction(ev.Action)
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
			tab.Terminal.Write(ev.Bytes)
		}
	}
	a.keyQueue = a.keyQueue[:0]

	// Don't forward text input to terminal during search (ImGui handles it)
	if searching {
		return
	}

	// Read text input from ImGui's character queue (SDL_TEXTINPUT events)
	if tab != nil {
		io := imgui.CurrentIO()
		chars := io.InputQueueCharacters()
		if chars.Size > 0 {
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
		// TODO: copy selected text
	case "paste":
		text, err := input.ClipboardRead()
		if err == nil && text != "" {
			if tab := a.tabs.Active(); tab != nil {
				tab.Terminal.Write([]byte(text))
			}
		}
	case "paste_selection":
		text, err := input.PrimaryRead()
		if err == nil && text != "" {
			if tab := a.tabs.Active(); tab != nil {
				tab.Terminal.Write([]byte(text))
			}
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
	a.cellW = fontSize * 0.6
	a.cellH = fontSize * 1.2
	a.renderer.Metrics = renderer.CellMetrics{Width: a.cellW, Height: a.cellH}
	a.resizeTerminals()
}

func (a *App) renderTabBar() {
	if a.tabs.Count() <= 1 {
		return // Don't show tab bar with single tab
	}

	imgui.SetNextWindowPosV(imgui.Vec2{X: 0, Y: 0}, imgui.CondAlways, imgui.Vec2{})
	imgui.SetNextWindowSizeV(imgui.Vec2{X: float32(a.width), Y: a.tabBarH}, imgui.CondAlways)
	flags := imgui.WindowFlagsNoTitleBar | imgui.WindowFlagsNoResize |
		imgui.WindowFlagsNoMove | imgui.WindowFlagsNoScrollbar |
		imgui.WindowFlagsNoBackground

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
				if imgui.BeginTabItemV(label, &open, 0) {
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
	ctx := &menu.Context{}
	if tab := a.tabs.Active(); tab != nil {
		ctx.TabTitle = tab.Title
		// CWD detection via /proc
		if tab.Terminal != nil {
			ctx.CWD = getCWD(tab.Terminal)
		}
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

	overlayH := float32(30)
	imgui.SetNextWindowPosV(imgui.Vec2{X: float32(a.width) - 320, Y: a.tabBarH}, imgui.CondAlways, imgui.Vec2{})
	imgui.SetNextWindowSizeV(imgui.Vec2{X: 310, Y: overlayH}, imgui.CondAlways)
	flags := imgui.WindowFlagsNoTitleBar | imgui.WindowFlagsNoResize |
		imgui.WindowFlagsNoMove | imgui.WindowFlagsNoScrollbar

	if imgui.BeginV("##search", nil, flags) {
		imgui.SetNextItemWidth(180)

		// Focus the input on first appearance
		if s.Query == "" {
			imgui.SetKeyboardFocusHere()
		}

		changed := imgui.InputTextWithHint("##searchinput", "Search...", &s.Query, 0, nil)
		if changed {
			s.Search(tab.Terminal.Emu)
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
		}
		imgui.SameLineV(0, 2)
		if imgui.ButtonV(">", imgui.Vec2{X: 20, Y: 0}) {
			s.NextMatch()
		}
		imgui.SameLineV(0, 2)
		if imgui.ButtonV("X", imgui.Vec2{X: 20, Y: 0}) {
			s.CloseSearch()
		}
	}
	imgui.End()

	// Draw match highlights on the terminal
	if len(s.Matches) > 0 {
		drawList := imgui.BackgroundDrawList()
		if drawList != nil {
			a.drawSearchHighlights(s, drawList)
		}
	}
}

func (a *App) drawSearchHighlights(s *scrollback.State, drawList *imgui.DrawList) {
	cellW := a.cellW
	cellH := a.cellH
	matchBg := uint32(0x4400FFFF)   // yellow, semi-transparent (ABGR)
	currentBg := uint32(0x8800AAFF) // orange, more opaque (ABGR)

	for i, m := range s.Matches {
		// Only draw matches on the visible screen (line >= 0)
		if m.Line < 0 {
			continue
		}

		x := a.renderer.OffsetX + float32(m.Col)*cellW
		y := a.renderer.OffsetY + float32(m.Line)*cellH
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

func getCWD(term interface{}) string {
	// Would need the child PID to read /proc/<pid>/cwd
	// Placeholder for now
	return ""
}
