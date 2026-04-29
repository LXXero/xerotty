// config_dialog.go — Preferences dialog for xerotty.
package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/fontsys"
	"github.com/LXXero/xerotty/internal/renderer"
	"github.com/LXXero/xerotty/internal/themes"
)

// applyColorOverrides applies user hex overrides from cfg onto a loaded theme.
// Only applies when the corresponding *_colors mode is "custom" and the hex is set.
func applyColorOverrides(t *renderer.Theme, cfg *config.Config) {
	if cfg.Appearance.TerminalColors == "custom" {
		if cfg.Appearance.Foreground != "" {
			t.Foreground = renderer.HexToABGR(cfg.Appearance.Foreground)
		}
		if cfg.Appearance.Background != "" {
			t.Background = renderer.HexToABGR(cfg.Appearance.Background)
		}
	}
}

// Combo option lists.
var (
	prefCursorStyles = []string{"block", "underline", "bar"}
	prefSBModes      = []string{"memory", "disk", "unlimited"}
	prefSBVisible    = []string{"always", "never", "auto"}
	prefChildExits   = []string{"close", "hold", "hold_on_error"}
	prefCloseBtnPos  = []string{"right", "left"}
	prefColorModes   = []string{"theme", "custom"}
	prefBSModes      = []string{"ascii_del", "ascii_bs"}
	prefDelModes     = []string{"vt_sequence", "ascii_del"}
	prefShiftEnters  = []string{"newline", "escape_sequence"}

	// Standard terminal font sizes. TTF/OTF fonts scale to any size, but
	// readable terminal sizes cluster in this range — exposing arbitrary
	// fractional sizes was the "weird" feedback. "Custom..." escapes for
	// users who want something specific.
	prefFontSizes = []float32{8, 9, 10, 11, 12, 13, 14, 15, 16, 18, 20, 22, 24, 28, 32, 36, 48, 72}
)

// Available actions for the menu editor.
var prefMenuActions = []string{
	"separator",
	"new_tab", "close_tab", "new_window",
	"copy", "paste", "paste_selection",
	"open_link", "copy_link",
	"search", "fullscreen",
	"select_all", "clear_scrollback", "reset_terminal",
	"rename_tab", "preferences",
	"font_size_up", "font_size_down", "font_size_reset",
}

var prefMenuLabels = map[string]string{
	"separator":        "---",
	"new_tab":          "New Tab",
	"close_tab":        "Close Tab",
	"new_window":       "New Window",
	"copy":             "Copy",
	"paste":            "Paste",
	"paste_selection":  "Paste Selection",
	"open_link":        "Open Link",
	"copy_link":        "Copy Link",
	"search":           "Search...",
	"fullscreen":       "Fullscreen",
	"select_all":       "Select All",
	"clear_scrollback": "Clear Scrollback",
	"reset_terminal":   "Reset Terminal",
	"rename_tab":       "Rename Tab",
	"preferences":      "Preferences",
	"font_size_up":     "Font Size Up",
	"font_size_down":   "Font Size Down",
	"font_size_reset":  "Font Size Reset",
}

// configDialog holds state for the preferences window.
type configDialog struct {
	open       bool
	themeNames []string

	// Appearance
	themeIdx          int32
	opacity           float32
	padding           int32
	cursorIdx         int32
	cursorBlink       bool
	blinkRate         int32
	boldIsBright      bool
	terminalColorsIdx int32
	tabColorsIdx      int32
	sbColorsIdx       int32
	resizeOverlay     bool
	resizeOverlayDur  float32
	foregroundHex     string
	backgroundHex     string
	tabBarBg          string
	tabActiveBg       string
	tabActiveFg       string
	tabInactiveBg     string
	tabInactiveFg     string
	scrollbarBgHex    string
	scrollbarThumbHex string

	// Font
	fontFamily  string
	fontSize    float32
	fontPath    string

	// Font picker state (populated lazily on first open)
	fontList       []renderer.FontEntry // discovered fonts for the picker
	fontShowAll    bool                 // include non-monospace
	fontResolved   string               // last resolved path (for status line)
	fontPickerInit bool
	fontSizeCustom bool // size combo set to "Custom..." → show numeric input

	// Shell & Tabs
	shell        string
	term         string
	childExitIdx int32
	inheritCWD   bool
	closeBtnIdx  int32

	// Scrollback
	sbLines   int32
	sbModeIdx int32
	scrollSpd int32
	diskDir   string
	scrollKey bool
	scrollOut bool

	// Scrollbar
	sbVisIdx   int32
	sbWidth    int32
	sbMinThumb int32

	// Clipboard
	copyOnSel      bool
	pasteMiddle    bool
	trimWS         bool
	unsafeEnabled  bool
	multilineWarn  bool
	nlGuard        bool
	unsafePatterns string

	// Links
	linksOn   bool
	ctrlClick bool
	dblClick  bool
	opener    string

	// Keys
	bsIdx   int32
	delIdx  int32
	shEnIdx int32

	// Window
	winCols  int32
	winRows  int32
	winTitle string
	winFS    bool

	// Menu editor
	menuItems    []menuEditorItem
	addActionIdx int32
}

type menuEditorItem struct {
	label    string
	action   string
	shortcut string
	enabled  string
}

func prefIndexOf(items []string, val string) int32 {
	for i, s := range items {
		if s == val {
			return int32(i)
		}
	}
	return 0
}

func discoverThemes() []string {
	seen := map[string]bool{}
	var names []string

	if dir, err := os.UserConfigDir(); err == nil {
		scanThemeDir(filepath.Join(dir, "xerotty", "themes"), &names, seen)
	}

	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		scanThemeDir(filepath.Join(base, "themes"), &names, seen)
		scanThemeDir(filepath.Join(base, "..", "themes"), &names, seen)
	}

	sort.Strings(names)
	return names
}

func scanThemeDir(dir string, names *[]string, seen map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".toml")
		if !seen[name] {
			seen[name] = true
			*names = append(*names, name)
		}
	}
}

func (d *configDialog) loadFrom(cfg *config.Config) {
	d.themeNames = discoverThemes()
	// Ensure current theme is in the list.
	if cfg.Appearance.Theme != "" {
		found := false
		for _, n := range d.themeNames {
			if n == cfg.Appearance.Theme {
				found = true
				break
			}
		}
		if !found {
			d.themeNames = append(d.themeNames, cfg.Appearance.Theme)
			sort.Strings(d.themeNames)
		}
	}
	if len(d.themeNames) == 0 {
		d.themeNames = []string{"default"}
	}

	d.themeIdx = prefIndexOf(d.themeNames, cfg.Appearance.Theme)
	d.opacity = cfg.Appearance.Opacity
	d.padding = int32(cfg.Appearance.Padding)
	d.cursorIdx = prefIndexOf(prefCursorStyles, cfg.Appearance.CursorStyle)
	d.cursorBlink = cfg.Appearance.CursorBlink
	d.blinkRate = int32(cfg.Appearance.BlinkRate)
	d.boldIsBright = cfg.Appearance.BoldIsBright
	d.terminalColorsIdx = prefIndexOf(prefColorModes, cfg.Appearance.TerminalColors)
	d.tabColorsIdx = prefIndexOf(prefColorModes, cfg.Appearance.TabColors)
	d.sbColorsIdx = prefIndexOf(prefColorModes, cfg.Appearance.ScrollbarColors)
	d.resizeOverlay = cfg.Appearance.ResizeOverlay
	d.resizeOverlayDur = cfg.Appearance.ResizeOverlayDuration
	d.foregroundHex = cfg.Appearance.Foreground
	d.backgroundHex = cfg.Appearance.Background
	d.tabBarBg = cfg.Appearance.TabBarBg
	d.tabActiveBg = cfg.Appearance.TabActiveBg
	d.tabActiveFg = cfg.Appearance.TabActiveFg
	d.tabInactiveBg = cfg.Appearance.TabInactiveBg
	d.tabInactiveFg = cfg.Appearance.TabInactiveFg
	d.scrollbarBgHex = cfg.Appearance.ScrollbarBg
	d.scrollbarThumbHex = cfg.Appearance.ScrollbarThumb

	d.fontFamily = cfg.Font.Family
	d.fontSize = cfg.Font.Size
	d.fontPath = cfg.Font.Path
	d.fontResolved = renderer.ResolveFontPath(cfg)
	d.fontPickerInit = false
	d.fontSizeCustom = !isStandardFontSize(d.fontSize)

	d.shell = cfg.Shell
	d.term = cfg.Term
	d.childExitIdx = prefIndexOf(prefChildExits, cfg.Tabs.OnChildExit)
	d.inheritCWD = cfg.Tabs.InheritCWD
	d.closeBtnIdx = prefIndexOf(prefCloseBtnPos, cfg.Tabs.CloseButtonPosition)

	d.sbLines = int32(cfg.Scrollback.Lines)
	d.sbModeIdx = prefIndexOf(prefSBModes, cfg.Scrollback.Mode)
	d.scrollSpd = int32(cfg.Scrollback.ScrollSpeed)
	d.diskDir = cfg.Scrollback.DiskDir
	d.scrollKey = cfg.Scrollback.ScrollOnKeystroke
	d.scrollOut = cfg.Scrollback.ScrollOnOutput

	d.sbVisIdx = prefIndexOf(prefSBVisible, cfg.Scrollbar.Visible)
	d.sbWidth = int32(cfg.Scrollbar.Width)
	d.sbMinThumb = int32(cfg.Scrollbar.MinThumbHeight)

	d.copyOnSel = cfg.Clipboard.CopyOnSelect
	d.pasteMiddle = cfg.Clipboard.PasteOnMiddleClick
	d.trimWS = cfg.Clipboard.TrimTrailingWhitespace
	d.unsafeEnabled = cfg.Clipboard.UnsafePaste.Enabled
	d.multilineWarn = cfg.Clipboard.UnsafePaste.MultilineWarning
	d.nlGuard = cfg.Clipboard.UnsafePaste.NewlineGuard
	d.unsafePatterns = strings.Join(cfg.Clipboard.UnsafePaste.Patterns, ", ")

	d.linksOn = cfg.Links.Enabled
	d.ctrlClick = cfg.Links.CtrlClick
	d.dblClick = cfg.Links.DoubleClick
	d.opener = cfg.Links.Opener

	d.bsIdx = prefIndexOf(prefBSModes, cfg.Keys.Backspace)
	d.delIdx = prefIndexOf(prefDelModes, cfg.Keys.Delete)
	d.shEnIdx = prefIndexOf(prefShiftEnters, cfg.Keys.ShiftEnter)

	d.winCols = int32(cfg.Window.Columns)
	d.winRows = int32(cfg.Window.Rows)
	d.winTitle = cfg.Window.Title
	d.winFS = cfg.Window.Fullscreen

	d.menuItems = nil
	for _, item := range cfg.Menu.Items {
		d.menuItems = append(d.menuItems, menuEditorItem{
			label: item.Label, action: item.Action,
			shortcut: item.Shortcut, enabled: item.Enabled,
		})
	}
	d.addActionIdx = 0
}

func (d *configDialog) applyTo(cfg *config.Config) {
	if int(d.themeIdx) < len(d.themeNames) {
		cfg.Appearance.Theme = d.themeNames[d.themeIdx]
	}
	cfg.Appearance.Opacity = d.opacity
	cfg.Appearance.Padding = int(d.padding)
	if int(d.cursorIdx) < len(prefCursorStyles) {
		cfg.Appearance.CursorStyle = prefCursorStyles[d.cursorIdx]
	}
	cfg.Appearance.CursorBlink = d.cursorBlink
	cfg.Appearance.BlinkRate = int(d.blinkRate)
	cfg.Appearance.BoldIsBright = d.boldIsBright
	if int(d.terminalColorsIdx) < len(prefColorModes) {
		cfg.Appearance.TerminalColors = prefColorModes[d.terminalColorsIdx]
	}
	if int(d.tabColorsIdx) < len(prefColorModes) {
		cfg.Appearance.TabColors = prefColorModes[d.tabColorsIdx]
	}
	if int(d.sbColorsIdx) < len(prefColorModes) {
		cfg.Appearance.ScrollbarColors = prefColorModes[d.sbColorsIdx]
	}
	cfg.Appearance.ResizeOverlay = d.resizeOverlay
	cfg.Appearance.ResizeOverlayDuration = d.resizeOverlayDur
	cfg.Appearance.Foreground = d.foregroundHex
	cfg.Appearance.Background = d.backgroundHex
	cfg.Appearance.TabBarBg = d.tabBarBg
	cfg.Appearance.TabActiveBg = d.tabActiveBg
	cfg.Appearance.TabActiveFg = d.tabActiveFg
	cfg.Appearance.TabInactiveBg = d.tabInactiveBg
	cfg.Appearance.TabInactiveFg = d.tabInactiveFg
	cfg.Appearance.ScrollbarBg = d.scrollbarBgHex
	cfg.Appearance.ScrollbarThumb = d.scrollbarThumbHex

	cfg.Font.Family = d.fontFamily
	cfg.Font.Size = d.fontSize
	cfg.Font.Path = d.fontPath

	cfg.Shell = d.shell
	cfg.Term = d.term
	if int(d.childExitIdx) < len(prefChildExits) {
		cfg.Tabs.OnChildExit = prefChildExits[d.childExitIdx]
	}
	cfg.Tabs.InheritCWD = d.inheritCWD
	if int(d.closeBtnIdx) < len(prefCloseBtnPos) {
		cfg.Tabs.CloseButtonPosition = prefCloseBtnPos[d.closeBtnIdx]
	}

	cfg.Scrollback.Lines = int(d.sbLines)
	if int(d.sbModeIdx) < len(prefSBModes) {
		cfg.Scrollback.Mode = prefSBModes[d.sbModeIdx]
	}
	cfg.Scrollback.ScrollSpeed = int(d.scrollSpd)
	cfg.Scrollback.DiskDir = d.diskDir
	cfg.Scrollback.ScrollOnKeystroke = d.scrollKey
	cfg.Scrollback.ScrollOnOutput = d.scrollOut

	if int(d.sbVisIdx) < len(prefSBVisible) {
		cfg.Scrollbar.Visible = prefSBVisible[d.sbVisIdx]
	}
	cfg.Scrollbar.Width = int(d.sbWidth)
	cfg.Scrollbar.MinThumbHeight = int(d.sbMinThumb)

	cfg.Clipboard.CopyOnSelect = d.copyOnSel
	cfg.Clipboard.PasteOnMiddleClick = d.pasteMiddle
	cfg.Clipboard.TrimTrailingWhitespace = d.trimWS
	cfg.Clipboard.UnsafePaste.Enabled = d.unsafeEnabled
	cfg.Clipboard.UnsafePaste.MultilineWarning = d.multilineWarn
	cfg.Clipboard.UnsafePaste.NewlineGuard = d.nlGuard
	cfg.Clipboard.UnsafePaste.Patterns = nil
	for _, p := range strings.Split(d.unsafePatterns, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			cfg.Clipboard.UnsafePaste.Patterns = append(cfg.Clipboard.UnsafePaste.Patterns, p)
		}
	}

	cfg.Links.Enabled = d.linksOn
	cfg.Links.CtrlClick = d.ctrlClick
	cfg.Links.DoubleClick = d.dblClick
	cfg.Links.Opener = d.opener

	if int(d.bsIdx) < len(prefBSModes) {
		cfg.Keys.Backspace = prefBSModes[d.bsIdx]
	}
	if int(d.delIdx) < len(prefDelModes) {
		cfg.Keys.Delete = prefDelModes[d.delIdx]
	}
	if int(d.shEnIdx) < len(prefShiftEnters) {
		cfg.Keys.ShiftEnter = prefShiftEnters[d.shEnIdx]
	}

	cfg.Window.Columns = int(d.winCols)
	cfg.Window.Rows = int(d.winRows)
	cfg.Window.Title = d.winTitle
	cfg.Window.Fullscreen = d.winFS

	cfg.Menu.Items = nil
	for _, item := range d.menuItems {
		cfg.Menu.Items = append(cfg.Menu.Items, config.MenuItem{
			Label: item.label, Action: item.action,
			Shortcut: item.shortcut, Enabled: item.enabled,
		})
	}
}

// openPreferences loads current config into dialog and shows it.
func (a *App) openPreferences() {
	a.prefDialog.loadFrom(&a.cfg)
	a.prefDialog.open = true
}

// applyPreferences writes dialog state to config, applies runtime changes, saves to disk.
func (a *App) applyPreferences() {
	prevFamily := a.cfg.Font.Family
	prevPath := a.cfg.Font.Path

	a.prefDialog.applyTo(&a.cfg)

	// Apply theme change.
	if t, err := themes.Load(a.cfg.Appearance.Theme); err == nil {
		applyColorOverrides(&t, &a.cfg)
		a.renderer.Theme = t
		a.theme = t
		bgR := float32((t.Background>>0)&0xFF) / 255.0
		bgG := float32((t.Background>>8)&0xFF) / 255.0
		bgB := float32((t.Background>>16)&0xFF) / 255.0
		a.backend.SetBgColor(imgui.NewVec4(bgR, bgG, bgB, 1.0))
	}
	a.renderer.BoldIsBright = a.cfg.Appearance.BoldIsBright

	faceChanged := a.cfg.Font.Family != prevFamily || a.cfg.Font.Path != prevPath
	if faceChanged {
		// Defer the atlas rebuild to the start of the next frame. Clearing
		// fonts mid-frame leaves draw commands holding stale texture handles,
		// which manifests as the terminal going blank or input/selection
		// breaking until a resize forces a redraw.
		a.pendingFontFace = true
	} else {
		// Size-only change: cheap scaling path, no atlas rebuild.
		a.updateFontMetrics()
	}

	// Persist to disk.
	_ = config.Save(a.cfg)
}

// renderPreferences draws the preferences window each frame.
func (a *App) renderPreferences() {
	if !a.prefDialog.open {
		return
	}

	center := imgui.Vec2{X: float32(a.width) / 2, Y: float32(a.height) / 2}
	if main := imgui.MainViewport(); main != nil {
		pos := main.Pos()
		size := main.Size()
		center = imgui.Vec2{X: pos.X + size.X*0.5, Y: pos.Y + size.Y*0.5}
	}

	if (imgui.CurrentIO().ConfigFlags() & imgui.ConfigFlagsViewportsEnable) != 0 {
		windowClass := imgui.NewWindowClass()
		windowClass.SetViewportFlagsOverrideSet(imgui.ViewportFlagsNoAutoMerge)
		imgui.SetNextWindowClass(windowClass)
		windowClass.Destroy()
	}

	imgui.SetNextWindowPosV(
		center,
		imgui.CondAppearing, imgui.Vec2{X: 0.5, Y: 0.5},
	)
	imgui.SetNextWindowSizeV(imgui.Vec2{X: 520, Y: 600}, imgui.CondAppearing)

	if imgui.BeginV("Preferences###prefs", &a.prefDialog.open, imgui.WindowFlagsNoDocking) {
		// Reserve space for bottom separator + button row.
		// Negative Y in BeginChildStrV means "fill, but leave -Y at the bottom".
		bottomReserve := imgui.FrameHeightWithSpacing() + 12
		tabH := -bottomReserve

		if imgui.BeginTabBar("##preftabs") {
			if imgui.BeginTabItem("Appearance") {
				if imgui.BeginChildStrV("##appsc", imgui.Vec2{X: 0, Y: tabH}, 0, 0) {
					a.renderPrefAppearance()
				}
				imgui.EndChild()
				imgui.EndTabItem()
			}
			if imgui.BeginTabItem("General") {
				if imgui.BeginChildStrV("##gensc", imgui.Vec2{X: 0, Y: tabH}, 0, 0) {
					imgui.Text("Font")
					imgui.Separator()
					a.renderPrefFont()
					imgui.Text("")
					imgui.Text("Shell & Tabs")
					imgui.Separator()
					a.renderPrefShellTabs()
				}
				imgui.EndChild()
				imgui.EndTabItem()
			}
			if imgui.BeginTabItem("Scrolling") {
				if imgui.BeginChildStrV("##scrollsc", imgui.Vec2{X: 0, Y: tabH}, 0, 0) {
					imgui.Text("Scrollback")
					imgui.Separator()
					a.renderPrefScrollback()
					imgui.Text("")
					imgui.Text("Scrollbar")
					imgui.Separator()
					a.renderPrefScrollbar()
				}
				imgui.EndChild()
				imgui.EndTabItem()
			}
			if imgui.BeginTabItem("Clipboard & Links") {
				if imgui.BeginChildStrV("##clipsc", imgui.Vec2{X: 0, Y: tabH}, 0, 0) {
					imgui.Text("Clipboard")
					imgui.Separator()
					a.renderPrefClipboard()
					imgui.Text("")
					imgui.Text("Links")
					imgui.Separator()
					a.renderPrefLinks()
				}
				imgui.EndChild()
				imgui.EndTabItem()
			}
			if imgui.BeginTabItem("Keys") {
				if imgui.BeginChildStrV("##keysc", imgui.Vec2{X: 0, Y: tabH}, 0, 0) {
					a.renderPrefKeys()
				}
				imgui.EndChild()
				imgui.EndTabItem()
			}
			if imgui.BeginTabItem("Menu") {
				if imgui.BeginChildStrV("##menusc", imgui.Vec2{X: 0, Y: tabH}, 0, 0) {
					a.renderPrefMenu()
				}
				imgui.EndChild()
				imgui.EndTabItem()
			}
			if imgui.BeginTabItem("Window") {
				if imgui.BeginChildStrV("##winsc", imgui.Vec2{X: 0, Y: tabH}, 0, 0) {
					a.renderPrefWindow()
				}
				imgui.EndChild()
				imgui.EndTabItem()
			}
			imgui.EndTabBar()
		}

		imgui.Separator()
		if imgui.Button("Apply") {
			a.applyPreferences()
		}
		imgui.SameLineV(0, 8)
		if imgui.Button("OK") {
			a.applyPreferences()
			a.prefDialog.open = false
		}
		imgui.SameLineV(0, 8)
		if imgui.Button("Cancel") {
			a.prefDialog.open = false
		}
	}
	imgui.End()
}

// --- Tab renderers ---

func (a *App) renderPrefAppearance() {
	d := &a.prefDialog
	w := float32(200)

	imgui.Text("Theme")
	imgui.SetNextItemWidth(w)
	imgui.ComboStrarr("##theme", &d.themeIdx, d.themeNames, int32(len(d.themeNames)))

	imgui.Text("Terminal Colors")
	imgui.SetNextItemWidth(w)
	imgui.ComboStrarr("##termcolors", &d.terminalColorsIdx, prefColorModes, int32(len(prefColorModes)))

	if d.terminalColorsIdx == 1 {
		imgui.Text("Foreground")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##fg", "#RRGGBB", &d.foregroundHex, 0, nil)
		imgui.Text("Background")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##bg", "#RRGGBB", &d.backgroundHex, 0, nil)
	}

	imgui.Text("Opacity")
	imgui.SetNextItemWidth(w)
	imgui.SliderFloat("##opacity", &d.opacity, 0.1, 1.0)

	imgui.Text("Padding (px)")
	imgui.SetNextItemWidth(w)
	imgui.SliderInt("##padding", &d.padding, 0, 20)

	imgui.Separator()

	imgui.Text("Cursor Style")
	imgui.SetNextItemWidth(w)
	imgui.ComboStrarr("##cursor", &d.cursorIdx, prefCursorStyles, int32(len(prefCursorStyles)))

	imgui.Checkbox("Cursor Blink", &d.cursorBlink)
	if d.cursorBlink {
		imgui.Text("Blink Rate (ms)")
		imgui.SetNextItemWidth(w)
		imgui.SliderInt("##blinkrate", &d.blinkRate, 100, 2000)
	}

	imgui.Checkbox("Bold is Bright", &d.boldIsBright)

	imgui.Separator()

	imgui.Checkbox("Resize Overlay", &d.resizeOverlay)
	if d.resizeOverlay {
		imgui.Text("Overlay Duration (s)")
		imgui.SetNextItemWidth(w)
		imgui.SliderFloat("##resizedur", &d.resizeOverlayDur, 0.1, 5.0)
	}

	imgui.Separator()

	imgui.Text("Tab Colors")
	imgui.SetNextItemWidth(w)
	imgui.ComboStrarr("##tabcolors", &d.tabColorsIdx, prefColorModes, int32(len(prefColorModes)))

	if d.tabColorsIdx == 1 {
		imgui.Text("Tab Bar BG")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##tabbarbg", "#RRGGBB", &d.tabBarBg, 0, nil)
		imgui.Text("Active Tab BG")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##tabactbg", "#RRGGBB", &d.tabActiveBg, 0, nil)
		imgui.Text("Active Tab FG")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##tabactfg", "#RRGGBB", &d.tabActiveFg, 0, nil)
		imgui.Text("Inactive Tab BG")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##tabinbg", "#RRGGBB", &d.tabInactiveBg, 0, nil)
		imgui.Text("Inactive Tab FG")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##tabinfg", "#RRGGBB", &d.tabInactiveFg, 0, nil)
	}

	imgui.Separator()

	imgui.Text("Scrollbar Colors")
	imgui.SetNextItemWidth(w)
	imgui.ComboStrarr("##sbcolors", &d.sbColorsIdx, prefColorModes, int32(len(prefColorModes)))

	if d.sbColorsIdx == 1 {
		imgui.Text("Scrollbar BG")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##sbbg", "#RRGGBB", &d.scrollbarBgHex, 0, nil)
		imgui.Text("Scrollbar Thumb")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##sbthumb", "#RRGGBB", &d.scrollbarThumbHex, 0, nil)
	}
}

func (a *App) renderPrefFont() {
	d := &a.prefDialog
	w := float32(280)

	// Lazy-load the font list — directory walks aren't free, do it once per open.
	if !d.fontPickerInit {
		d.refreshFontList()
		d.fontPickerInit = true
	}

	imgui.Text("Font")

	// Build display labels for the combo. Each entry shows family name
	// only; the path is in fontList[i].Path.
	labels := make([]string, len(d.fontList))
	for i, e := range d.fontList {
		labels[i] = e.Family
	}

	// Resolve current selection by matching either family or path —
	// path takes precedence so a user-entered custom file still
	// reflects the right family in the dropdown when its family
	// matches a known one.
	selIdx := int32(-1)
	for i, e := range d.fontList {
		if d.fontPath != "" && e.Path == d.fontPath {
			selIdx = int32(i)
			break
		}
		if d.fontPath == "" && strings.EqualFold(e.Family, d.fontFamily) {
			selIdx = int32(i)
			break
		}
	}

	imgui.SetNextItemWidth(w)
	if len(labels) == 0 {
		imgui.TextDisabled("(no fonts found — check ~/.fonts or ~/Library/Fonts)")
	} else if imgui.ComboStrarr("##fontpick", &selIdx, labels, int32(len(labels))) {
		if selIdx >= 0 && int(selIdx) < len(d.fontList) {
			d.fontFamily = d.fontList[selIdx].Family
			d.fontPath = d.fontList[selIdx].Path
			d.fontResolved = d.fontPath
		}
	}
	imgui.SameLineV(0, 8)
	if imgui.Checkbox("Include non-monospace", &d.fontShowAll) {
		d.refreshFontList()
	}

	// Custom path — for fonts not in the system database (e.g. a .ttf
	// in ~/Downloads). When set, this overrides the family dropdown.
	imgui.TextDisabled("Or load a font file directly:")
	imgui.SetNextItemWidth(w)
	if imgui.InputTextWithHint("##fontpath", "/path/to/font.ttf (optional)", &d.fontPath, 0, nil) {
		d.refreshResolved()
	}

	// Status line — what will actually load.
	if d.fontResolved != "" {
		imgui.TextDisabled("→ " + d.fontResolved)
	} else {
		imgui.TextColored(imgui.Vec4{X: 1, Y: 0.5, Z: 0.5, W: 1}, "→ not found (will use ImGui default)")
	}

	imgui.Separator()

	imgui.Text("Size")
	a.renderPrefFontSize(w)
}

// renderPrefFontSize draws a combo of standard sizes with a "Custom..." escape
// hatch. TTF fonts scale continuously, but for terminals only a small handful
// of sizes are typically useful — surfacing them as discrete picks is friendlier
// than a freeform 6-72 slider.
func (a *App) renderPrefFontSize(w float32) {
	d := &a.prefDialog

	if !d.fontSizeCustom {
		labels := make([]string, len(prefFontSizes)+1)
		for i, s := range prefFontSizes {
			labels[i] = fmt.Sprintf("%g pt", s)
		}
		labels[len(prefFontSizes)] = "Custom..."

		// Find current size in the preset list. If absent (e.g. a user typed
		// 13.5 in the config file), surface a synthetic entry so the combo
		// reflects reality.
		selIdx := int32(-1)
		for i, s := range prefFontSizes {
			if s == d.fontSize {
				selIdx = int32(i)
				break
			}
		}
		if selIdx < 0 {
			d.fontSizeCustom = true
		} else {
			imgui.SetNextItemWidth(w)
			if imgui.ComboStrarr("##fontsize", &selIdx, labels, int32(len(labels))) {
				if int(selIdx) == len(prefFontSizes) {
					d.fontSizeCustom = true
				} else {
					d.fontSize = prefFontSizes[selIdx]
				}
			}
			return
		}
	}

	// Custom-size input: integer points are the common case but allow
	// fractional values for HiDPI users who want a half-pixel size.
	imgui.SetNextItemWidth(w)
	imgui.InputFloat("##fontsizecustom", &d.fontSize)
	if d.fontSize < 6 {
		d.fontSize = 6
	}
	if d.fontSize > 96 {
		d.fontSize = 96
	}
	imgui.SameLineV(0, 8)
	if imgui.Button("Presets") {
		d.fontSizeCustom = false
	}
}

// isStandardFontSize reports whether s exactly matches one of the preset sizes.
func isStandardFontSize(s float32) bool {
	for _, ps := range prefFontSizes {
		if ps == s {
			return true
		}
	}
	return false
}

// refreshFontList rescans system font dirs and updates the combo source.
// When fontsys is available (CoreText on macOS, fontconfig on Linux once
// implemented), we ask the OS — that gives a proper monospace flag from
// the font's metadata instead of guessing from filename. Falls back to
// the filename-heuristic discovery if no fontsys impl exists for the
// platform.
func (d *configDialog) refreshFontList() {
	if fontsys.Default != nil {
		if entries, ok := enumerateViaFontsys(d.fontShowAll); ok {
			d.fontList = entries
			return
		}
	}
	if d.fontShowAll {
		d.fontList = renderer.DiscoverAllFonts()
	} else {
		d.fontList = renderer.DiscoverMonospaceFonts()
	}
}

// enumerateViaFontsys queries the OS font database via fontsys and
// returns FontEntry list filtered to one entry per family, regular
// weight only. Returns ok=false if enumeration failed (caller falls
// back to filename heuristics).
func enumerateViaFontsys(showAll bool) ([]renderer.FontEntry, bool) {
	infos, err := fontsys.Default.Enumerate()
	if err != nil {
		return nil, false
	}
	seen := map[string]bool{}
	var out []renderer.FontEntry
	for _, f := range infos {
		if !showAll && !f.Monospace {
			continue
		}
		if !isRegularStyle(f.Style) {
			continue
		}
		if f.Family == "" {
			continue
		}
		key := strings.ToLower(f.Family)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, renderer.FontEntry{Family: f.Family, Path: f.Path})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Family) < strings.ToLower(out[j].Family)
	})
	return out, true
}

// isRegularStyle filters out bold/italic/etc faces so the picker shows
// one row per family. CoreText's style names include "Regular", "Bold",
// "Italic", "Bold Italic", "Light", etc.
func isRegularStyle(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "regular", "roman", "book", "normal", "plain":
		return true
	}
	return false
}

// refreshResolved recomputes the status-line preview after a custom edit.
func (d *configDialog) refreshResolved() {
	tmp := config.Config{Font: config.FontConfig{
		Family: d.fontFamily,
		Path:   d.fontPath,
	}}
	d.fontResolved = renderer.ResolveFontPath(&tmp)
}

func (a *App) renderPrefShellTabs() {
	d := &a.prefDialog
	w := float32(250)

	imgui.Text("Shell Override (empty = auto-detect)")
	imgui.SetNextItemWidth(w)
	imgui.InputTextWithHint("##shell", "/bin/bash", &d.shell, 0, nil)

	imgui.Text("TERM Variable")
	imgui.SetNextItemWidth(w)
	imgui.InputTextWithHint("##term", "xterm-256color", &d.term, 0, nil)

	imgui.Separator()

	imgui.Text("On Child Exit")
	imgui.SetNextItemWidth(w)
	imgui.ComboStrarr("##childexit", &d.childExitIdx, prefChildExits, int32(len(prefChildExits)))

	imgui.Checkbox("New Tab Inherits CWD", &d.inheritCWD)

	imgui.Text("Close Button Position")
	imgui.SetNextItemWidth(w)
	imgui.ComboStrarr("##closebtn", &d.closeBtnIdx, prefCloseBtnPos, int32(len(prefCloseBtnPos)))
}

func (a *App) renderPrefScrollback() {
	d := &a.prefDialog
	w := float32(200)

	imgui.Text("Scrollback Mode")
	imgui.SetNextItemWidth(w)
	imgui.ComboStrarr("##sbmode", &d.sbModeIdx, prefSBModes, int32(len(prefSBModes)))

	if d.sbModeIdx != 2 { // not unlimited
		imgui.Text("Lines")
		imgui.SetNextItemWidth(w)
		imgui.InputInt("##sblines", &d.sbLines)
	}

	if d.sbModeIdx == 1 { // disk
		imgui.Text("Disk Directory")
		imgui.SetNextItemWidth(300)
		imgui.InputTextWithHint("##diskdir", "/tmp/xerotty", &d.diskDir, 0, nil)
	}

	imgui.Separator()

	imgui.Text("Scroll Speed (lines per tick)")
	imgui.SetNextItemWidth(w)
	imgui.SliderInt("##scrollspd", &d.scrollSpd, 1, 20)

	imgui.Checkbox("Scroll to Bottom on Keystroke", &d.scrollKey)
	imgui.Checkbox("Scroll to Bottom on Output", &d.scrollOut)
}

func (a *App) renderPrefScrollbar() {
	d := &a.prefDialog
	w := float32(200)

	imgui.Text("Visibility")
	imgui.SetNextItemWidth(w)
	imgui.ComboStrarr("##sbvis", &d.sbVisIdx, prefSBVisible, int32(len(prefSBVisible)))

	imgui.Text("Width (px)")
	imgui.SetNextItemWidth(w)
	imgui.SliderInt("##sbwidth", &d.sbWidth, 4, 30)

	imgui.Text("Min Thumb Height (px)")
	imgui.SetNextItemWidth(w)
	imgui.SliderInt("##sbminthumb", &d.sbMinThumb, 10, 100)
}

func (a *App) renderPrefClipboard() {
	d := &a.prefDialog

	imgui.Checkbox("Copy on Select", &d.copyOnSel)
	imgui.Checkbox("Paste on Middle Click", &d.pasteMiddle)
	imgui.Checkbox("Trim Trailing Whitespace", &d.trimWS)

	imgui.Separator()
	imgui.Text("Unsafe Paste Protection")

	imgui.Checkbox("Enabled##unsafe", &d.unsafeEnabled)
	if d.unsafeEnabled {
		imgui.Checkbox("Multiline Warning", &d.multilineWarn)
		imgui.Checkbox("Newline Guard", &d.nlGuard)

		imgui.Text("Patterns (comma-separated regex)")
		imgui.SetNextItemWidth(400)
		imgui.InputTextWithHint("##patterns", `sudo\s, rm\s+(-rf|--recursive)`, &d.unsafePatterns, 0, nil)
	}
}

func (a *App) renderPrefLinks() {
	d := &a.prefDialog
	w := float32(250)

	imgui.Checkbox("URL Detection", &d.linksOn)

	if d.linksOn {
		imgui.Checkbox("Ctrl+Click to Open", &d.ctrlClick)
		imgui.Checkbox("Double-Click to Open", &d.dblClick)

		imgui.Text("URL Opener Command")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##opener", "xdg-open", &d.opener, 0, nil)
	}
}

func (a *App) renderPrefKeys() {
	d := &a.prefDialog
	w := float32(200)

	imgui.Text("Backspace Sends")
	imgui.SetNextItemWidth(w)
	imgui.ComboStrarr("##bsmode", &d.bsIdx, prefBSModes, int32(len(prefBSModes)))

	imgui.Text("Delete Sends")
	imgui.SetNextItemWidth(w)
	imgui.ComboStrarr("##delmode", &d.delIdx, prefDelModes, int32(len(prefDelModes)))

	imgui.Text("Shift+Enter Sends")
	imgui.SetNextItemWidth(w)
	imgui.ComboStrarr("##shenter", &d.shEnIdx, prefShiftEnters, int32(len(prefShiftEnters)))
}

func (a *App) renderPrefMenu() {
	d := &a.prefDialog

	imgui.Text("Context Menu Items")
	imgui.Separator()

	removeIdx := -1
	swapA, swapB := -1, -1
	n := len(d.menuItems)

	for i := range d.menuItems {
		item := &d.menuItems[i]

		// Label column.
		if item.action == "separator" {
			imgui.Text("  ----------------")
		} else {
			label := item.label
			if label == "" {
				label = item.action
			}
			text := label
			if item.shortcut != "" {
				text += "  (" + item.shortcut + ")"
			}
			imgui.Text("  " + text)
		}

		// Buttons aligned to right side.
		imgui.SameLineV(imgui.WindowWidth()-80, 0)

		dis := i == 0
		if dis {
			imgui.BeginDisabled()
		}
		if imgui.ButtonV(fmt.Sprintf("^##mu%d", i), imgui.Vec2{X: 22, Y: 0}) {
			swapA, swapB = i, i-1
		}
		if dis {
			imgui.EndDisabled()
		}

		imgui.SameLineV(0, 2)

		dis = i == n-1
		if dis {
			imgui.BeginDisabled()
		}
		if imgui.ButtonV(fmt.Sprintf("v##md%d", i), imgui.Vec2{X: 22, Y: 0}) {
			swapA, swapB = i, i+1
		}
		if dis {
			imgui.EndDisabled()
		}

		imgui.SameLineV(0, 2)

		if imgui.ButtonV(fmt.Sprintf("X##mx%d", i), imgui.Vec2{X: 22, Y: 0}) {
			removeIdx = i
		}
	}

	// Apply modifications after iteration.
	if swapA >= 0 && swapB >= 0 {
		d.menuItems[swapA], d.menuItems[swapB] = d.menuItems[swapB], d.menuItems[swapA]
	}
	if removeIdx >= 0 {
		d.menuItems = append(d.menuItems[:removeIdx], d.menuItems[removeIdx+1:]...)
	}

	imgui.Separator()
	if imgui.Button("Add Item") {
		action := prefMenuActions[d.addActionIdx]
		label := prefMenuLabels[action]
		d.menuItems = append(d.menuItems, menuEditorItem{
			label: label, action: action,
		})
	}
	imgui.SameLineV(0, 8)
	imgui.SetNextItemWidth(200)
	imgui.ComboStrarr("##addaction", &d.addActionIdx, prefMenuActions, int32(len(prefMenuActions)))
}

func (a *App) renderPrefWindow() {
	d := &a.prefDialog
	w := float32(200)

	imgui.Text("Initial Columns")
	imgui.SetNextItemWidth(w)
	imgui.InputInt("##wincols", &d.winCols)

	imgui.Text("Initial Rows")
	imgui.SetNextItemWidth(w)
	imgui.InputInt("##winrows", &d.winRows)

	imgui.Text("Window Title")
	imgui.SetNextItemWidth(w)
	imgui.InputTextWithHint("##wintitle", "xerotty", &d.winTitle, 0, nil)

	imgui.Checkbox("Start Fullscreen", &d.winFS)
}
