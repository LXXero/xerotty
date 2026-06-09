// config_dialog.go — Preferences dialog for xerotty.
package app

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/fontsys"
	"github.com/LXXero/xerotty/internal/platform"
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
	prefSBModes      = []string{"memory", "unlimited"}
	prefSBVisible    = []string{"always", "never", "auto"}
	prefChildExits   = []string{"close", "hold", "hold_on_error"}
	prefCloseBtnPos  = []string{"right", "left"}
	// prefTabSourcesBase is the always-available subset. Remote
	// host entries get appended at prefs-open time via
	// buildTabSourceOptions; the resulting per-dialog slice lives
	// on configDialog.tabSourceOpts.
	prefTabSourcesBase = []string{"pty", "daemon"}
	prefColorModes     = []string{"theme", "custom"}
	prefBSModes        = []string{"ascii_del", "ascii_bs"}
	prefDelModes       = []string{"vt_sequence", "ascii_del"}
	prefShiftEnters    = []string{"newline", "escape_sequence"}
	prefHomeEnds       = []string{"ss3", "auto", "csi", "vt"}

	// Standard terminal font sizes. TTF/OTF fonts scale to any size, but
	// readable terminal sizes cluster in this range — exposing arbitrary
	// fractional sizes was the "weird" feedback. "Custom..." escapes for
	// users who want something specific.
	prefFontSizes = []float32{8, 9, 10, 11, 12, 13, 14, 15, 16, 18, 20, 22, 24, 28, 32, 36, 48, 72}
)

// Available actions for the menu editor. Keep alphabetical (the Add
// combo re-sorts by friendly label at runtime, but the source list
// stays greppable and additions have one obvious home).
var prefMenuActions = []string{
	// _remote_hosts is a magic action: at render time
	// app.expandMenu replaces it with a "Remote" submenu
	// listing per-host new/reattach items, one pair per
	// [[hosts]] entry. Listed here so the menu editor can
	// re-insert it after the user removes it.
	"_remote_hosts",
	"clear_scrollback",
	"close_tab",
	"connect_remote",
	"copy",
	"copy_link",
	"font_size_down",
	"font_size_reset",
	"font_size_up",
	"fullscreen",
	"new_tab",
	"new_window",
	"next_tab",
	"open_link",
	"paste",
	"paste_selection",
	"preferences",
	"prev_tab",
	"rename_tab",
	"reset_terminal",
	"scroll_bottom",
	"scroll_page_down",
	"scroll_page_up",
	"scroll_top",
	"search",
	"select_all",
	"separator",
	"toggle_opacity",
}

// menuKindSubmenu is an editor-only sentinel in the Add combo: picking
// it makes "Add Item" / a submenu's "+" create a new EMPTY submenu
// container (an explicitly-named parent) rather than an action item. It
// is never a real config action — newMenuEditorItem maps it to an
// isSubmenu entry.
const menuKindSubmenu = "_submenu"

// prefMenuAddOptions is the Add combo's option list: every action plus
// the "make a submenu" sentinel. d.addActionIdx indexes into THIS slice.
var prefMenuAddOptions = append(append([]string{}, prefMenuActions...), menuKindSubmenu)

// newMenuEditorItem builds the entry the Add combo's current selection
// describes — a named empty submenu for the sentinel, otherwise an
// action item with its default label.
func newMenuEditorItem(kind string) menuEditorItem {
	if kind == menuKindSubmenu {
		return menuEditorItem{label: "Submenu", isSubmenu: true}
	}
	it := menuEditorItem{label: prefMenuLabels[kind], action: kind}
	if kind == "toggle_opacity" {
		// Bind the live force-opaque state so the row renders its
		// checkmark — the same binding the default config entry ships
		// ({Action: "toggle_opacity", Checked: "force_opaque"}).
		it.checked = "force_opaque"
	}
	return it
}

// Keep alphabetical by key, same as prefMenuActions.
var prefMenuLabels = map[string]string{
	"_remote_hosts":    "Remote (expands per host)",
	"_submenu":         "Submenu",
	"clear_scrollback": "Clear Scrollback",
	"close_tab":        "Close Tab",
	"connect_remote":   "Connect to host...",
	"copy":             "Copy",
	"copy_link":        "Copy Link",
	"font_size_down":   "Font Size Down",
	"font_size_reset":  "Font Size Reset",
	"font_size_up":     "Font Size Up",
	"fullscreen":       "Fullscreen",
	"new_tab":          "New Tab",
	"new_window":       "New Window",
	"next_tab":         "Next Tab",
	"open_link":        "Open Link",
	"paste":            "Paste",
	"paste_selection":  "Paste Selection",
	"preferences":      "Preferences",
	"prev_tab":         "Previous Tab",
	"rename_tab":       "Rename Tab",
	"reset_terminal":   "Reset Terminal",
	"scroll_bottom":    "Scroll to Bottom",
	"scroll_page_down": "Scroll Page Down",
	"scroll_page_up":   "Scroll Page Up",
	"scroll_top":       "Scroll to Top",
	"search":           "Search...",
	"select_all":       "Select All",
	"separator":        "---",
	"toggle_opacity":   "Toggle Opacity",
}

// menuAddOption pairs a friendly display label with the action (or
// sentinel) it adds. The Add combo shows the labels alphabetically;
// d.addActionIdx indexes into prefMenuAddSorted, and .action maps the
// selected index back to what newMenuEditorItem consumes — so reordering
// the display never changes which item gets added.
type menuAddOption struct {
	label  string
	action string
}

func menuAddLabel(action string) string {
	if l, ok := prefMenuLabels[action]; ok && l != "" {
		return l
	}
	return action
}

func buildMenuAddSorted() ([]menuAddOption, []string) {
	opts := make([]menuAddOption, 0, len(prefMenuAddOptions))
	for _, a := range prefMenuAddOptions {
		opts = append(opts, menuAddOption{label: menuAddLabel(a), action: a})
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].label < opts[j].label })
	labels := make([]string, len(opts))
	for i, o := range opts {
		labels[i] = o.label
	}
	return opts, labels
}

// prefMenuAddSorted / prefMenuAddLabels are the Add combo's options
// sorted by friendly label, built once at init. Go orders package-var
// initialization by dependency, so this safely reads prefMenuAddOptions
// + prefMenuLabels.
var prefMenuAddSorted, prefMenuAddLabels = buildMenuAddSorted()

// configDialog holds state for the preferences window.
type configDialog struct {
	open bool
	// focused mirrors whether the prefs window (or its children/combo
	// popup) holds ImGui focus this frame. App.inputOwnedByDialog reads
	// it across ALL windows so the terminal input path can tell "a prefs
	// dialog is being typed into" from "prefs is open but the user
	// clicked back to a terminal" — even when the focused dialog belongs
	// to a window that isn't app.active.
	focused    bool
	themeNames []string

	// Appearance
	themeIdx           int32
	chooserClosedFrame map[string]int
	opacity            float32
	glowOn             bool
	glowIntensity      float32
	glowSpeed          float32
	glowScale          float32
	glowBlobs          int32
	padding            int32
	cursorIdx          int32
	cursorBlink        bool
	blinkRate          int32
	boldIsBright       bool
	terminalColorsIdx  int32
	tabColorsIdx       int32
	sbColorsIdx        int32
	resizeOverlay      bool
	resizeOverlayDur   float32
	foregroundHex      string
	backgroundHex      string
	tabBarBg           string
	tabActiveBg        string
	tabActiveFg        string
	tabInactiveBg      string
	tabInactiveFg      string
	scrollbarBgHex     string
	scrollbarThumbHex  string

	// Font
	fontFamily string
	fontSize   float32
	fontPath   string

	// Font picker state (populated lazily on first open)
	fontList       []renderer.FontEntry // discovered fonts for the picker
	fontShowAll    bool                 // include non-monospace
	fontResolved   string               // last resolved path (for status line)
	fontPickerInit bool
	fontSizeCustom bool // size combo set to "Custom..." → show numeric input

	// Shell & Tabs
	shell         string
	term          string
	childExitIdx  int32
	inheritCWD    bool
	closeBtnIdx   int32
	tabSourceIdx  int32
	tabSourceOpts []string // built on prefs-open: base + per-host
	daemonSocket  string

	// Scrollback
	sbLines   int32
	sbModeIdx int32
	dragSpd   int32
	scrollSpd int32
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
	bsIdx      int32
	delIdx     int32
	shEnIdx    int32
	homeEndIdx int32

	// Window
	winCols  int32
	winRows  int32
	winTitle string
	winFS    bool

	// Menu editor. addActionIdx indexes prefMenuAddSorted (the chooser is
	// a native SDL3 popup — see renderPrefMenuAddFooter).
	menuItems    []menuEditorItem
	addActionIdx int32

	// addActionPopupClosedFrame is the imgui FrameCount at which
	// RunImGuiPopup last returned. If the dismiss-click lands on the
	// trigger button itself, main_ctx sees a release-while-hovered
	// edge a few frames after popup-close and ButtonV would re-fire,
	// reopening the popup. renderPrefMenuAddFooter suppresses `open`
	// for a short window after this frame.
	addActionPopupClosedFrame int
}

// menuEditorItem is the editor's view of one menu entry. An entry is
// exactly ONE of three kinds: a separator (action == "separator"), a
// submenu (isSubmenu — a named container holding `submenu` children, no
// action of its own), or an action (everything else — fires `action`).
type menuEditorItem struct {
	label    string
	action   string
	shortcut string
	enabled  string
	checked  string
	// isSubmenu marks a named-container entry. It's tracked explicitly
	// (rather than inferred from len(submenu)>0) so a freshly-created
	// EMPTY submenu still shows its add-child button — config.MenuItem
	// can't express an empty submenu, so we can't round-trip through
	// len()>0 alone.
	isSubmenu bool
	submenu   []menuEditorItem
}

// menuItemsToEditor / editorToMenuItems convert between the config and
// editor representations recursively, so hand-authored [[menu.items.submenu]]
// trees survive a prefs open + save round-trip. config.MenuItem infers
// "is a submenu" from len(Submenu)>0; the editor tracks it explicitly.
func menuItemsToEditor(items []config.MenuItem) []menuEditorItem {
	var out []menuEditorItem
	for _, item := range items {
		ed := menuEditorItem{
			label: item.Label, shortcut: item.Shortcut,
			enabled: item.Enabled, checked: item.Checked,
		}
		if len(item.Submenu) > 0 {
			// Submenu container: a named parent, no action of its own
			// (the engine ignores Action when Submenu is non-empty).
			ed.isSubmenu = true
			ed.submenu = menuItemsToEditor(item.Submenu)
		} else {
			ed.action = item.Action
		}
		out = append(out, ed)
	}
	return out
}

func editorToMenuItems(items []menuEditorItem) []config.MenuItem {
	var out []config.MenuItem
	for _, item := range items {
		mi := config.MenuItem{
			Label: item.label, Shortcut: item.shortcut,
			Enabled: item.enabled, Checked: item.checked,
		}
		if item.isSubmenu {
			// A submenu serializes via its children. An EMPTY submenu
			// can't be expressed in config (len(Submenu)==0 reads back
			// as a leaf), so it degrades to a leaf-with-label, no action
			// — acceptable since an empty submenu is meaningless.
			mi.Submenu = editorToMenuItems(item.submenu)
		} else {
			mi.Action = item.action
		}
		out = append(out, mi)
	}
	return out
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

	// Enumerate the exact dirs themes.Load resolves against — keeping
	// a second hand-rolled path list here is how the picker once ended
	// up blind to the macOS .app's Resources/themes dir.
	for _, dir := range themes.SearchDirs() {
		scanThemeDir(dir, &names, seen)
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
	d.glowOn = cfg.Appearance.Glow.Enabled
	d.glowIntensity = float32(cfg.Appearance.Glow.Intensity)
	if d.glowIntensity <= 0 {
		d.glowIntensity = 0.35
	}
	d.glowSpeed = float32(cfg.Appearance.Glow.Speed)
	if d.glowSpeed <= 0 {
		d.glowSpeed = 1.0
	}
	d.glowScale = float32(cfg.Appearance.Glow.Scale)
	if d.glowScale <= 0 {
		d.glowScale = 0.7
	}
	d.glowBlobs = int32(cfg.Appearance.Glow.Blobs)
	if d.glowBlobs <= 0 {
		d.glowBlobs = 5
	}
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
	// Build the source-mode option list dynamically: base
	// options (pty, daemon) + one "daemon:<name>" per cfg.Hosts
	// entry. Without this, setting Tabs.Source = "daemon:kh"
	// would fall to index 0 (pty) on prefs-open and get
	// clobbered back to "pty" on save.
	d.tabSourceOpts = append([]string{}, prefTabSourcesBase...)
	for _, h := range cfg.Hosts {
		d.tabSourceOpts = append(d.tabSourceOpts, "daemon:"+h.Name)
	}
	// Membership check, NOT prefIndexOf — prefIndexOf returns 0
	// (= "pty") on miss, which would silently downgrade an unknown
	// source value. We want -1-on-miss semantics here so the
	// preservation branch below actually fires.
	d.tabSourceIdx = -1
	for i, opt := range d.tabSourceOpts {
		if opt == cfg.Tabs.Source {
			d.tabSourceIdx = int32(i)
			break
		}
	}
	if d.tabSourceIdx < 0 {
		// Source references a host not currently in cfg.Hosts —
		// add it temporarily so the user can see + keep their
		// configured value instead of having it silently
		// downgraded.
		if cfg.Tabs.Source != "" {
			d.tabSourceOpts = append(d.tabSourceOpts, cfg.Tabs.Source)
			d.tabSourceIdx = int32(len(d.tabSourceOpts) - 1)
		} else {
			d.tabSourceIdx = 0
		}
	}
	d.daemonSocket = cfg.Tabs.DaemonSocket

	d.sbLines = int32(cfg.Scrollback.Lines)
	d.sbModeIdx = prefIndexOf(prefSBModes, cfg.Scrollback.Mode)
	d.scrollSpd = int32(cfg.Scrollback.ScrollSpeed)
	d.dragSpd = int32(cfg.Scrollback.DragScrollSpeed)
	if d.dragSpd <= 0 {
		d.dragSpd = 25
	}
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
	d.homeEndIdx = prefIndexOf(prefHomeEnds, cfg.Keys.HomeEnd)

	d.winCols = int32(cfg.Window.Columns)
	d.winRows = int32(cfg.Window.Rows)
	d.winTitle = cfg.Window.Title
	d.winFS = cfg.Window.Fullscreen

	d.menuItems = menuItemsToEditor(cfg.Menu.Items)
	d.addActionIdx = 0
}

func (d *configDialog) applyTo(cfg *config.Config) {
	if int(d.themeIdx) < len(d.themeNames) {
		cfg.Appearance.Theme = d.themeNames[d.themeIdx]
	}
	cfg.Appearance.Opacity = d.opacity
	cfg.Appearance.Glow.Enabled = d.glowOn
	cfg.Appearance.Glow.Intensity = float64(d.glowIntensity)
	cfg.Appearance.Glow.Speed = float64(d.glowSpeed)
	cfg.Appearance.Glow.Scale = float64(d.glowScale)
	cfg.Appearance.Glow.Blobs = int(d.glowBlobs)
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
	if int(d.tabSourceIdx) < len(d.tabSourceOpts) {
		cfg.Tabs.Source = d.tabSourceOpts[d.tabSourceIdx]
	}
	cfg.Tabs.DaemonSocket = d.daemonSocket
	if int(d.closeBtnIdx) < len(prefCloseBtnPos) {
		cfg.Tabs.CloseButtonPosition = prefCloseBtnPos[d.closeBtnIdx]
	}

	cfg.Scrollback.Lines = int(d.sbLines)
	if int(d.sbModeIdx) < len(prefSBModes) {
		cfg.Scrollback.Mode = prefSBModes[d.sbModeIdx]
	}
	cfg.Scrollback.ScrollSpeed = int(d.scrollSpd)
	cfg.Scrollback.DragScrollSpeed = int(d.dragSpd)
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
	if int(d.homeEndIdx) < len(prefHomeEnds) {
		cfg.Keys.HomeEnd = prefHomeEnds[d.homeEndIdx]
	}

	cfg.Window.Columns = int(d.winCols)
	cfg.Window.Rows = int(d.winRows)
	cfg.Window.Title = d.winTitle
	cfg.Window.Fullscreen = d.winFS

	cfg.Menu.Items = editorToMenuItems(d.menuItems)
}

// openPreferences loads current config into dialog and shows it.
func (a *Window) openPreferences() {
	a.prefDialog.loadFrom(&a.app.cfg)
	a.prefDialog.open = true
	a.app.active = a
}

func (a *Window) restoreFocusAfterPreferencesClose() {
	if a.pendingClose {
		return
	}
	a.app.active = a
	// Reuse the normal post-frame focus path so native focus moves back
	// to the owning terminal window after the prefs viewport closes.
	a.pendingFocus = true
}

// applyPreferences writes dialog state to config, applies runtime changes, saves to disk.
func (a *Window) applyPreferences() {
	prevFamily := a.app.cfg.Font.Family
	prevPath := a.app.cfg.Font.Path
	prevSource := a.app.cfg.Tabs.Source
	// Snapshot the geometry-affecting inputs so we only reflow windows
	// when one of them actually changed — see the resize gate below.
	prevPx := renderer.PixelSize(&a.app.cfg)
	prevPad := a.app.cfg.Appearance.Padding

	a.prefDialog.applyTo(&a.app.cfg)
	a.app.ensureGlowTicker()

	// Tab source mode flip. Only honor pty → daemon switches that
	// previously had no hub (auto-spawn the daemon now). Going
	// daemon → pty just nulls out the factory; the running hub +
	// any daemon-backed tabs keep going (no point tearing them
	// down — the user can close them when they're done). Tabs
	// opened after the toggle land in the new source.
	if a.app.cfg.Tabs.Source != prevSource {
		if a.app.cfg.Tabs.Source == "daemon" && a.app.daemonHub == nil {
			if err := a.app.initDaemonSource(); err != nil {
				fmt.Fprintf(os.Stderr, "xerotty: prefs flipped to daemon mode but auto-spawn failed: %v\n", err)
			}
		}
		// Reapply the factory to every Window so the next NewTab
		// honors the new mode. installSourceFactory closes over
		// the Window's own daemonWindowID — important so multi-
		// window setups route correctly post-flip.
		for _, win := range a.app.windows {
			if win.tabs != nil {
				a.app.installSourceFactory(win)
			}
		}
	}

	// Apply theme change. The cimgui-go backend is shared across all
	// Windows (multi-viewport pop-out reuses the carrier's GL context),
	// so SetBgColor and updateEventLoopBg are process-wide and only run
	// once. The renderer is per-Window though — loop so theme switches
	// from prefs in Window A still re-color Windows B, C, ...
	if t, err := themes.Load(a.app.cfg.Appearance.Theme); err == nil {
		applyColorOverrides(&t, &a.app.cfg)
		a.app.theme = t
		for _, win := range a.app.windows {
			if win.renderer != nil {
				win.renderer.Theme = t
				win.renderer.InvalidateCellCache()
			}
		}
		bgR := float32((t.Background>>0)&0xFF) / 255.0
		bgG := float32((t.Background>>8)&0xFF) / 255.0
		bgB := float32((t.Background>>16)&0xFF) / 255.0
		platform.SetBgColor(imgui.NewVec4(bgR, bgG, bgB, 1.0))
	}
	// BoldIsBright also lives on each Window's renderer.
	for _, win := range a.app.windows {
		if win.renderer != nil {
			win.renderer.BoldIsBright = a.app.cfg.Appearance.BoldIsBright
			win.renderer.InvalidateCellCache()
		}
	}

	faceChanged := a.app.cfg.Font.Family != prevFamily || a.app.cfg.Font.Path != prevPath
	if faceChanged {
		// Defer the atlas rebuild to the start of the next frame. Clearing
		// fonts mid-frame leaves draw commands holding stale texture handles,
		// which manifests as the terminal going blank or input/selection
		// breaking until a resize forces a redraw.
		a.app.pendingFontFace = true
	} else if renderer.PixelSize(&a.app.cfg) != prevPx || a.app.cfg.Appearance.Padding != prevPad {
		// Font size or padding changed — reflow every window to the new
		// metrics. Treated as "reset all windows to the new default",
		// overriding any per-window Cmd+= / Cmd+- zoom that diverged.
		//
		// Gated on an ACTUAL geometry change: updateFontMetrics requests
		// a window resize, and running it on an Apply that changed
		// nothing geometric (a menu/keybind/theme edit) needlessly
		// resized windows — which on some WMs settled a couple grid rows
		// short, shrinking tabbed terminals after Apply.
		newSize := renderer.PixelSize(&a.app.cfg)
		for _, win := range a.app.windows {
			win.fontSize = newSize
			win.updateFontMetrics()
		}
	}

	// Re-apply scrollback config to every running terminal — affects
	// the vt.SafeEmulator's buffer size immediately, so switching to
	// "unlimited" or bumping Lines doesn't require reopening the
	// terminal. Walks every Window's every Tab.
	for _, win := range a.app.windows {
		if win.tabs == nil {
			continue
		}
		for _, tab := range win.tabs.Tabs {
			if tab.Terminal != nil {
				tab.Terminal.SetScrollbackFromConfig(&a.app.cfg)
			}
		}
	}

	// Persist to disk.
	_ = config.Save(a.app.cfg)
}

// renderPreferences draws the preferences window each frame.
func (a *Window) renderPreferences() {
	if !a.prefDialog.open {
		return
	}
	wasOpen := a.prefDialog.open

	// Center on the OWNING Window's viewport — not MainViewport, which
	// is the hidden cimgui-go carrier and would put prefs at the
	// carrier's (likely 0,0 or wherever the OS placed the invisible
	// window) position. Each Window has its own popped-out viewport
	// captured into imViewport during its frame's BeginV.
	center := imgui.Vec2{X: a.contentOriginX + float32(a.width)/2, Y: a.contentOriginY + float32(a.height)/2}
	if vp := a.viewport(); vp != nil {
		pos := vp.Pos()
		size := vp.Size()
		center = imgui.Vec2{X: pos.X + size.X*0.5, Y: pos.Y + size.Y*0.5}
	}

	multiViewport := (imgui.CurrentIO().ConfigFlags() & imgui.ConfigFlagsViewportsEnable) != 0
	if multiViewport {
		// Clear NoDecoration so the platform window gets normal OS
		// chrome (title bar, close/min/max, native drag-to-move),
		// and set NoAutoMerge so it pops out as its own window.
		// Pair with WindowFlagsNoTitleBar below — otherwise ImGui
		// also draws a title and we get double chrome. The same
		// trick is used for the main terminal wrapper window.
		windowClass := imgui.NewWindowClass()
		windowClass.SetViewportFlagsOverrideSet(imgui.ViewportFlagsNoAutoMerge)
		windowClass.SetViewportFlagsOverrideClear(imgui.ViewportFlagsNoDecoration)
		imgui.SetNextWindowClass(windowClass)
		windowClass.Destroy()
	}

	imgui.SetNextWindowPosV(
		center,
		imgui.CondAppearing, imgui.Vec2{X: 0.5, Y: 0.5},
	)
	imgui.SetNextWindowSizeV(imgui.Vec2{X: 720, Y: 720}, imgui.CondAppearing)

	// Title bar handling: when multi-viewport is on we want the OS
	// to draw the chrome (no ImGui title); when it's off the prefs
	// renders inside the parent viewport and ImGui's title is the
	// only thing the user has to drag/close with.
	prefFlags := imgui.WindowFlagsNoDocking
	if multiViewport {
		// OS chrome (the title bar) moves the window under multi-viewport,
		// so also disable ImGui's own move-by-dragging-the-body. Without
		// NoMove, clicking the inert background between widgets — e.g. the
		// menu editor's Text rows — and twitching starts an ImGui
		// window-move; on Wayland that move's mouse-up can be dropped
		// during the viewport position/focus shuffle, leaving
		// g.MovingWindow stuck so the whole dialog goes unclickable until
		// it's closed and reopened.
		prefFlags |= imgui.WindowFlagsNoTitleBar | imgui.WindowFlagsNoMove
	}
	// Reset each frame; set true below only while the prefs window
	// actually holds focus (used by App.inputOwnedByDialog to gate
	// terminal input across all windows).
	a.prefDialog.focused = false
	if imgui.BeginV("Preferences###prefs", &a.prefDialog.open, prefFlags) {
		// RootAndChildWindows so the tab content, the menu editor's
		// child, and the Add combo's popup all count as "prefs focused".
		a.prefDialog.focused = imgui.IsWindowFocusedV(imgui.FocusedFlagsRootAndChildWindows)
		// OS close-button: under multi-viewport ImGui's &open bool
		// doesn't propagate the WM's close — viewport.PlatformRequestClose
		// does. Mirror it back to our open state.
		if multiViewport {
			if vp := imgui.WindowViewport(); vp != nil && vp.PlatformRequestClose() {
				a.prefDialog.open = false
				vp.SetPlatformRequestClose(false)
			}
		}
		// Keep SDL text input asserted on the prefs window every frame.
		// The ImGui SDL3 backend SDL_StopTextInput's the window when an
		// InputText deactivates, so opening the Add combo (not an
		// InputText) would otherwise leave text input OFF — io.InputQueue
		// stays empty and combo type-ahead (jump-to-letter) never fires.
		// EnsureTextInput is a no-op when input is already on, so it
		// doesn't disturb the label InputText fields.
		if vp := imgui.WindowViewport(); vp != nil {
			platform.EnsureTextInput(vp.PlatformHandle())
		}
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
				// Shorten the scroll child by the footer's height so the
				// Add controls (rendered AFTER EndChild, outside the
				// scroll region — see renderPrefMenuAddFooter) have room.
				// The action chooser is a native SDL3 popup window, so it
				// floats OVER the dialog and needs no reserved space here.
				footerH := imgui.FrameHeightWithSpacing() + 12
				if imgui.BeginChildStrV("##menusc", imgui.Vec2{X: 0, Y: tabH - footerH}, 0, 0) {
					a.renderPrefMenu()
				}
				imgui.EndChild()
				a.renderPrefMenuAddFooter()
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
	if wasOpen && !a.prefDialog.open {
		a.restoreFocusAfterPreferencesClose()
	}
}

// --- Tab renderers ---

// prefPairRow lays two labeled controls side by side — the prefs
// window is much taller than it is wide, so paired numerics share a
// row instead of stacking into a scrollbar. Each draw func should
// emit exactly one widget; the item width is set for it already.
func prefPairRow(w float32, l1 string, draw1 func(), l2 string, draw2 func()) {
	_ = w
	if imgui.BeginTableV("##pair_"+l1, 2, 0, imgui.NewVec2(0, 0), 0) {
		imgui.TableNextColumn()
		imgui.Text(l1)
		draw1()
		imgui.TableNextColumn()
		imgui.Text(l2)
		draw2()
		imgui.EndTable()
	}
}

func (a *Window) renderPrefAppearance() {
	d := &a.prefDialog
	w := float32(200)

	prefPairRow(w, "Theme", func() {
		a.prefCombo("theme", &d.themeIdx, d.themeNames, w)
	}, "Terminal Colors", func() {
		a.prefCombo("termcolors", &d.terminalColorsIdx, prefColorModes, w)
	})

	if d.terminalColorsIdx == 1 {
		prefPairRow(w, "Foreground", func() {
			imgui.SetNextItemWidth(w)
			imgui.InputTextWithHint("##fg", "#RRGGBB", &d.foregroundHex, 0, nil)
		}, "Background", func() {
			imgui.SetNextItemWidth(w)
			imgui.InputTextWithHint("##bg", "#RRGGBB", &d.backgroundHex, 0, nil)
		})
	}

	prefPairRow(w, "Opacity", func() {
		imgui.SetNextItemWidth(w)
		imgui.SliderFloat("##opacity", &d.opacity, 0.1, 1.0)
	}, "Padding (px)", func() {
		imgui.SetNextItemWidth(w)
		imgui.SliderInt("##padding", &d.padding, 0, 20)
	})

	imgui.Checkbox("Lava Lamp Background", &d.glowOn)
	if d.glowOn {
		prefPairRow(w, "Glow Intensity", func() {
			imgui.SetNextItemWidth(w)
			imgui.SliderFloat("##glowintensity", &d.glowIntensity, 0.05, 1.0)
		}, "Glow Speed", func() {
			imgui.SetNextItemWidth(w)
			imgui.SliderFloat("##glowspeed", &d.glowSpeed, 0.1, 4.0)
		})
		prefPairRow(w, "Blob Size", func() {
			imgui.SetNextItemWidth(w)
			imgui.SliderFloat("##glowscale", &d.glowScale, 0.2, 1.5)
		}, "Blob Count", func() {
			imgui.SetNextItemWidth(w)
			imgui.SliderInt("##glowblobs", &d.glowBlobs, 1, 16)
		})
	}

	imgui.Separator()

	imgui.Text("Cursor Style")
	a.prefCombo("cursor", &d.cursorIdx, prefCursorStyles, w)

	if d.cursorBlink {
		// Same 2-col grid as prefPairRow so the slider's X lines up
		// with the right-column sliders above; checkbox + the
		// dependent control's label share the left cell.
		if imgui.BeginTableV("##pair_blink", 2, 0, imgui.NewVec2(0, 0), 0) {
			imgui.TableNextColumn()
			imgui.Checkbox("Cursor Blink", &d.cursorBlink)
			imgui.TableNextColumn()
			imgui.Text("Blink Rate (ms)")
			imgui.SetNextItemWidth(w)
			imgui.SliderInt("##blinkrate", &d.blinkRate, 100, 2000)
			imgui.EndTable()
		}
	} else {
		imgui.Checkbox("Cursor Blink", &d.cursorBlink)
	}

	imgui.Checkbox("Bold is Bright", &d.boldIsBright)

	imgui.Separator()

	if d.resizeOverlay {
		if imgui.BeginTableV("##pair_overlay", 2, 0, imgui.NewVec2(0, 0), 0) {
			imgui.TableNextColumn()
			imgui.Checkbox("Resize Overlay", &d.resizeOverlay)
			imgui.TableNextColumn()
			imgui.Text("Overlay Duration (s)")
			imgui.SetNextItemWidth(w)
			imgui.SliderFloat("##resizedur", &d.resizeOverlayDur, 0.1, 5.0)
			imgui.EndTable()
		}
	} else {
		imgui.Checkbox("Resize Overlay", &d.resizeOverlay)
	}

	imgui.Separator()

	imgui.Text("Tab Colors")
	a.prefCombo("tabcolors", &d.tabColorsIdx, prefColorModes, w)

	if d.tabColorsIdx == 1 {
		prefPairRow(w, "Tab Bar BG", func() {
			imgui.SetNextItemWidth(w)
			imgui.InputTextWithHint("##tabbarbg", "#RRGGBB", &d.tabBarBg, 0, nil)
		}, "Active Tab BG", func() {
			imgui.SetNextItemWidth(w)
			imgui.InputTextWithHint("##tabactbg", "#RRGGBB", &d.tabActiveBg, 0, nil)
		})
		prefPairRow(w, "Active Tab FG", func() {
			imgui.SetNextItemWidth(w)
			imgui.InputTextWithHint("##tabactfg", "#RRGGBB", &d.tabActiveFg, 0, nil)
		}, "Inactive Tab BG", func() {
			imgui.SetNextItemWidth(w)
			imgui.InputTextWithHint("##tabinbg", "#RRGGBB", &d.tabInactiveBg, 0, nil)
		})
		imgui.Text("Inactive Tab FG")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##tabinfg", "#RRGGBB", &d.tabInactiveFg, 0, nil)
	}

	imgui.Separator()

	imgui.Text("Scrollbar Colors")
	a.prefCombo("sbcolors", &d.sbColorsIdx, prefColorModes, w)

	if d.sbColorsIdx == 1 {
		prefPairRow(w, "Scrollbar BG", func() {
			imgui.SetNextItemWidth(w)
			imgui.InputTextWithHint("##sbbg", "#RRGGBB", &d.scrollbarBgHex, 0, nil)
		}, "Scrollbar Thumb", func() {
			imgui.SetNextItemWidth(w)
			imgui.InputTextWithHint("##sbthumb", "#RRGGBB", &d.scrollbarThumbHex, 0, nil)
		})
	}
}

func (a *Window) renderPrefFont() {
	d := &a.prefDialog
	w := float32(280)

	// Lazy-load the font list — directory walks aren't free, do it once per open.
	if !d.fontPickerInit {
		d.refreshFontList()
		d.fontPickerInit = true
	}

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

	// Two-column grid: Font | Size on row one, then the
	// non-monospace toggle under the font picker with the custom
	// file path beside it.
	if imgui.BeginTableV("##pair_font", 2, 0, imgui.NewVec2(0, 0), 0) {
		imgui.TableNextColumn()
		imgui.Text("Font")
		if len(labels) == 0 {
			imgui.TextDisabled("(no fonts found — check ~/.fonts or ~/Library/Fonts)")
		} else if a.prefCombo("fontpick", &selIdx, labels, w) {
			if selIdx >= 0 && int(selIdx) < len(d.fontList) {
				d.fontFamily = d.fontList[selIdx].Family
				d.fontPath = d.fontList[selIdx].Path
				d.fontResolved = d.fontPath
			}
		}
		imgui.TableNextColumn()
		imgui.Text("Size")
		a.renderPrefFontSize(w)

		imgui.TableNextColumn()
		if imgui.Checkbox("Include non-monospace", &d.fontShowAll) {
			d.refreshFontList()
		}
		imgui.TableNextColumn()
		// Custom path — for fonts not in the system database (e.g. a
		// .ttf in ~/Downloads). When set, this overrides the dropdown.
		imgui.TextDisabled("Or load a font file directly:")
		imgui.SetNextItemWidth(w)
		if imgui.InputTextWithHint("##fontpath", "/path/to/font.ttf (optional)", &d.fontPath, 0, nil) {
			d.refreshResolved()
		}
		imgui.EndTable()
	}

	// Status line — what will actually load.
	if d.fontResolved != "" {
		imgui.TextDisabled("→ " + d.fontResolved)
	} else {
		imgui.TextColored(imgui.Vec4{X: 1, Y: 0.5, Z: 0.5, W: 1}, "→ not found (will use ImGui default)")
	}
}

// renderPrefFontSize draws a combo of standard sizes with a "Custom..." escape
// hatch. TTF fonts scale continuously, but for terminals only a small handful
// of sizes are typically useful — surfacing them as discrete picks is friendlier
// than a freeform 6-72 slider.
func (a *Window) renderPrefFontSize(w float32) {
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
			if a.prefCombo("fontsize", &selIdx, labels, w) {
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

func (a *Window) renderPrefShellTabs() {
	d := &a.prefDialog
	w := float32(250)

	imgui.Text("Shell Override (empty = auto-detect)")
	imgui.SetNextItemWidth(w)
	imgui.InputTextWithHint("##shell", "/bin/bash", &d.shell, 0, nil)

	imgui.Text("TERM Variable")
	imgui.SetNextItemWidth(w)
	imgui.InputTextWithHint("##term", "xterm-256color", &d.term, 0, nil)

	imgui.Separator()

	prefPairRow(w, "On Child Exit", func() {
		a.prefCombo("childexit", &d.childExitIdx, prefChildExits, w)
	}, "Close Button Position", func() {
		a.prefCombo("closebtn", &d.closeBtnIdx, prefCloseBtnPos, w)
	})

	imgui.Checkbox("New Tab Inherits CWD", &d.inheritCWD)

	imgui.Separator()

	imgui.Text("Tab Source")
	a.prefCombo("tabsource", &d.tabSourceIdx, d.tabSourceOpts, w)
	imgui.TextDisabled("pty: in-process. daemon: routes through xerotty serve")
	imgui.TextDisabled("(auto-spawns one if no daemon is running). Takes effect on")
	imgui.TextDisabled("the next tab — existing tabs stay on their current source.")

	if d.tabSourceIdx == 1 {
		imgui.Text("Daemon Socket (empty = $XDG_RUNTIME_DIR/xerottyd.sock)")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##daemonsock", "/run/user/1000/xerottyd.sock", &d.daemonSocket, 0, nil)
	}
}

func (a *Window) renderPrefScrollback() {
	d := &a.prefDialog
	w := float32(200)

	prefPairRow(w, "Mode", func() {
		a.prefCombo("sbmode", &d.sbModeIdx, prefSBModes, w)
	}, "Scroll Speed (lines per tick)", func() {
		imgui.SetNextItemWidth(w)
		imgui.SliderInt("##scrollspd", &d.scrollSpd, 1, 20)
	})

	imgui.Text("Drag Scroll Speed (rows per second)")
	imgui.SetNextItemWidth(w)
	imgui.SliderInt("##dragspd", &d.dragSpd, 5, 150)

	// Lines is only meaningful in "memory" mode — under "unlimited"
	// the buffer grows without bound, so the number wouldn't do
	// anything. Skip it so the UI doesn't suggest otherwise.
	if int(d.sbModeIdx) < len(prefSBModes) && prefSBModes[d.sbModeIdx] == "memory" {
		imgui.Text("Lines")
		imgui.SetNextItemWidth(w)
		imgui.InputInt("##sblines", &d.sbLines)
		imgui.TextDisabled("Output past Lines is dropped (oldest first). For huge")
		imgui.TextDisabled("bursts (seq 80000, find / etc.) pick \"unlimited\" so")
		imgui.TextDisabled("history doesn't get truncated.")
	} else {
		imgui.TextDisabled("Unlimited mode spills to disk via /tmp. Daemon-mode")
		imgui.TextDisabled("tabs honor this too — full history reaches the GUI.")
	}

	imgui.Separator()

	if imgui.BeginTableV("##pair_scrollbottom", 2, 0, imgui.NewVec2(0, 0), 0) {
		imgui.TableNextColumn()
		imgui.Checkbox("Scroll to Bottom on Keystroke", &d.scrollKey)
		imgui.TableNextColumn()
		imgui.Checkbox("Scroll to Bottom on Output", &d.scrollOut)
		imgui.EndTable()
	}
}

func (a *Window) renderPrefScrollbar() {
	d := &a.prefDialog
	w := float32(200)

	imgui.Text("Visibility")
	a.prefCombo("sbvis", &d.sbVisIdx, prefSBVisible, w)

	prefPairRow(w, "Width (px)", func() {
		imgui.SetNextItemWidth(w)
		imgui.SliderInt("##sbwidth", &d.sbWidth, 4, 30)
	}, "Min Thumb Height (px)", func() {
		imgui.SetNextItemWidth(w)
		imgui.SliderInt("##sbminthumb", &d.sbMinThumb, 10, 100)
	})
}

func (a *Window) renderPrefClipboard() {
	d := &a.prefDialog

	imgui.Checkbox("Also copy selection to CLIPBOARD", &d.copyOnSel)
	if imgui.IsItemHovered() {
		imgui.SetTooltip("Selection always updates PRIMARY (middle-click target) on Linux.\nEnable to ALSO write CLIPBOARD (Ctrl/Cmd+V target) on every selection.")
	}
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

func (a *Window) renderPrefLinks() {
	d := &a.prefDialog
	w := float32(250)

	imgui.Checkbox("URL Detection", &d.linksOn)

	if d.linksOn {
		imgui.Checkbox("Ctrl+Click to Open", &d.ctrlClick)
		imgui.Checkbox("Double-Click to Open", &d.dblClick)

		imgui.Text("URL Opener Command")
		imgui.SetNextItemWidth(w)
		imgui.InputTextWithHint("##opener", config.DefaultOpener(), &d.opener, 0, nil)
	}
}

func (a *Window) renderPrefKeys() {
	d := &a.prefDialog
	w := float32(200)

	imgui.Text("Backspace Sends")
	a.prefCombo("bsmode", &d.bsIdx, prefBSModes, w)

	imgui.Text("Delete Sends")
	a.prefCombo("delmode", &d.delIdx, prefDelModes, w)

	imgui.Text("Shift+Enter Sends")
	a.prefCombo("shenter", &d.shEnIdx, prefShiftEnters, w)

	imgui.Text("Home / End Sends")
	a.prefCombo("homeend", &d.homeEndIdx, prefHomeEnds, w)
	switch d.homeEndIdx {
	case 0:
		imgui.TextDisabled("ss3 (ESC O H/F) = terminfo khome/kend — works in zsh/bash")
	case 1:
		imgui.TextDisabled("auto: SS3 in app-cursor mode, CSI otherwise (xterm)")
	case 2:
		imgui.TextDisabled("csi: always ESC [ H/F")
	default:
		imgui.TextDisabled("vt: ESC [ 1~ / ESC [ 4~")
	}
}

// renderPrefMenu draws the scrollable item list. The Add controls are
// NOT here — they render as a fixed footer (renderPrefMenuAddFooter)
// OUTSIDE the scrolling child. Inside the scroll region the combo's
// dropdown was positioned relative to clipped/scrolled content, so on
// Wayland it floated mid-window and couldn't be clicked.
func (a *Window) renderPrefMenu() {
	d := &a.prefDialog

	imgui.Text("Context Menu Items")
	imgui.TextDisabled("  submenu names are editable · ^/v reorder within a level · + (submenus only) adds a child · X removes")
	imgui.Separator()

	a.renderMenuLevel(&d.menuItems, 0, "m")
}

// prefCombo renders a combo-style chooser whose dropdown is a NATIVE
// SDL3 popup (platform.RunImGuiPopup) instead of ImGui's BeginCombo.
// ImGui's own combo popup embeds fine while it fits inside the prefs
// viewport, but the moment it would overflow (a combo near the
// window's bottom edge — e.g. after a custom-colors block expands
// above it) ImGui promotes it to its own OS window, and Wayland
// forbids positioning toplevels — the compositor drops it anywhere.
// Same disease that made menu.go and the add-action chooser go
// native. Returns true when the selection changed.
func (a *Window) prefCombo(id string, idx *int32, items []string, w float32) bool {
	d := &a.prefDialog
	preview := "(choose)"
	if int(*idx) >= 0 && int(*idx) < len(items) {
		preview = items[*idx]
	}
	open := imgui.ButtonV(preview+"##"+id, imgui.Vec2{X: w, Y: 0})
	// Same dismiss-click reopen race as the add-chooser: the popup's
	// context eats the DOWN, main gets the UP and reads it as a
	// click on this button. Swallow opens shortly after a close.
	if d.chooserClosedFrame == nil {
		d.chooserClosedFrame = map[string]int{}
	}
	if open {
		if cf, ok := d.chooserClosedFrame[id]; ok && int(imgui.FrameCount())-cf < 20 {
			open = false
		}
	}
	btnMin := imgui.ItemRectMin()
	btnMax := imgui.ItemRectMax()
	vp := imgui.WindowViewport()
	if !open || vp == nil {
		return false
	}
	sel, picked := a.runChooserPopup(vp, btnMin, btnMax, items, *idx)
	d.chooserClosedFrame[id] = int(imgui.FrameCount())
	if picked && sel != *idx {
		*idx = sel
		return true
	}
	return false
}

// runChooserPopup opens a native popup listing items anchored to the
// trigger button's CURRENT rect (captured this frame, so it can never
// go stale no matter how the layout above reflowed). Prefers opening
// BELOW like a normal combo, falling back above → right → left within
// the usable screen bounds. Blocks until dismissed; returns the
// picked index. Shares the type-to-jump machinery with the
// add-action chooser.
func (a *Window) runChooserPopup(vp *imgui.Viewport, btnMin, btnMax imgui.Vec2, items []string, cur int32) (int32, bool) {
	parentID := vp.PlatformHandle()
	vpPos := vp.Pos()

	rowH := imgui.TextLineHeightWithSpacing()
	if rowH < 18 {
		rowH = 18
	}
	popupW := int(btnMax.X - btnMin.X)
	if popupW < 160 {
		popupW = 160
	}
	popupH := int(rowH)*len(items) + 16
	if popupH > 420 {
		popupH = 420
	}

	const gap = 4
	belowY := int(btnMax.Y) + gap
	aboveY := int(btnMin.Y) - popupH - gap
	rightX := int(btnMax.X) + gap
	leftX := int(btnMin.X) - popupW - gap
	topY := int(btnMin.Y)

	usableX, usableY, usableW, usableH := 0, 0, 0, 0
	haveUsable := false
	if x, y, ww, hh, ok := platform.WindowUsableBounds(parentID); ok {
		usableX, usableY, usableW, usableH = x, y, ww, hh
		haveUsable = true
	}
	fits := func(absX, absY int) bool {
		if !haveUsable {
			return true
		}
		return absX >= usableX && absY >= usableY &&
			absX+popupW <= usableX+usableW && absY+popupH <= usableY+usableH
	}
	clampY := func(absY int) int {
		if !haveUsable {
			return absY
		}
		if absY+popupH > usableY+usableH {
			absY = usableY + usableH - popupH
		}
		if absY < usableY {
			absY = usableY
		}
		return absY
	}

	var absX, absY int
	placedAbove := false
	switch {
	case fits(int(btnMin.X), belowY):
		absX, absY = int(btnMin.X), belowY
	case fits(int(btnMin.X), aboveY):
		absX, absY = int(btnMin.X), aboveY
		placedAbove = true
	case fits(rightX, topY):
		absX, absY = rightX, topY
	case fits(leftX, topY):
		absX, absY = leftX, topY
	default:
		absX, absY = int(btnMin.X), clampY(belowY)
	}

	relX := absX - int(vpPos.X)
	relY := absY - int(vpPos.Y)
	if relX < 0 {
		relX = 0
	}
	if relY < 0 {
		relY = 0
	}

	picked := int32(-1)
	platform.RunImGuiPopup(parentID, relX, relY, popupW, popupH,
		func() platform.PopupMenuDrawResult {
			if placedAbove {
				imgui.SetNextWindowPosV(imgui.Vec2{X: 0, Y: float32(popupH)}, imgui.CondAlways, imgui.Vec2{X: 0, Y: 1})
			} else {
				imgui.SetNextWindowPos(imgui.Vec2{X: 0, Y: 0})
			}
			flags := imgui.WindowFlagsNoTitleBar |
				imgui.WindowFlagsNoResize |
				imgui.WindowFlagsNoMove |
				imgui.WindowFlagsNoSavedSettings |
				imgui.WindowFlagsNoCollapse |
				imgui.WindowFlagsAlwaysAutoResize
			var res platform.PopupMenuDrawResult
			if imgui.BeginV("##prefchooserpopup", nil, flags) {
				tmTarget := -1
				if imgui.IsWindowFocusedV(imgui.FocusedFlagsNone) {
					tmTarget = chooserTypematchTarget(items)
				}
				for i, label := range items {
					selected := int32(i) == cur
					if i == tmTarget {
						imgui.SetKeyboardFocusHereV(0)
						imgui.InternalSetNavCursorVisibleAfterMove()
						imgui.CurrentContext().SetNavInputSource(imgui.InputSourceKeyboard)
					}
					clicked := imgui.SelectableBoolV(label+fmt.Sprintf("##pc%d", i), selected, 0, imgui.Vec2{X: 0, Y: 0})
					if selected {
						imgui.SetItemDefaultFocus()
					}
					if clicked {
						picked = int32(i)
						res.Close = true
					}
				}
			}
			imgui.End()
			if imgui.IsMouseClickedBool(imgui.MouseButtonLeft) || imgui.IsMouseClickedBool(imgui.MouseButtonRight) {
				if !imgui.CurrentIO().WantCaptureMouse() {
					res.Close = true
				}
			}
			return res
		})
	platform.PostWake()
	return picked, picked >= 0
}

// chooserTypematchTarget is addChooserTypematchTarget for an arbitrary
// label list (same shared prefix state — one popup runs at a time).
func chooserTypematchTarget(labels []string) int {
	nowNano := time.Now().UnixNano()
	if nowNano-addChooserTMLastNano > int64(700*time.Millisecond) {
		addChooserTMPrefix = ""
	}
	var typed string
	for _, ch := range imgui.CurrentIO().InputQueueCharacters().Slice() {
		if ch >= 0x20 && ch < 0x7f {
			typed += string(rune(ch))
		}
	}
	if typed == "" {
		return -1
	}
	addChooserTMLastNano = nowNano
	addChooserTMPrefix += strings.ToLower(typed)
	for i, l := range labels {
		if strings.HasPrefix(strings.ToLower(l), addChooserTMPrefix) {
			return i
		}
	}
	for i, l := range labels {
		if strings.Contains(strings.ToLower(l), addChooserTMPrefix) {
			return i
		}
	}
	return -1
}

// renderPrefMenuAddFooter draws the "Add Item" button + a selection
// button as a fixed footer below the scrollable list. Clicking the
// selection button opens a REAL floating dropdown via
// platform.RunImGuiPopup — a native SDL3 xdg_popup parented to the prefs
// window, correctly positioned + clickable on Wayland (ImGui's own combo
// BeginPopup is broken on this multi-viewport setup; same reason menu.go
// hand-rolls its menu). Modeled on the working right-click context menu
// (Window.renderContextMenu). selectedAddAction maps d.addActionIdx (an
// index into prefMenuAddSorted) back to the action — unchanged.
func (a *Window) renderPrefMenuAddFooter() {
	d := &a.prefDialog

	imgui.Separator()
	if imgui.Button("Add Item") {
		d.menuItems = append(d.menuItems, newMenuEditorItem(d.selectedAddAction()))
	}
	imgui.SameLineV(0, 8)

	preview := "(choose)"
	if int(d.addActionIdx) >= 0 && int(d.addActionIdx) < len(prefMenuAddLabels) {
		preview = prefMenuAddLabels[d.addActionIdx]
	}
	open := imgui.ButtonV(preview+"##addchooser", imgui.Vec2{X: 200, Y: 0})
	// The popup loop's separate ImGui context consumes the
	// dismiss-click DOWN, but the UP arrives in main's drain_events a
	// few frames later and main's ButtonV interprets it as a
	// release-while-hovered click on the trigger — reopening the
	// popup the user just dismissed. Swallow `open` for a short
	// window after the popup closed.
	const addActionReopenCooldownFrames = 20
	if open && d.addActionPopupClosedFrame > 0 &&
		int(imgui.FrameCount())-d.addActionPopupClosedFrame < addActionReopenCooldownFrames {
		open = false
	}
	// Capture the button's screen rect + the prefs viewport NOW, before
	// RunImGuiPopup swaps the ImGui context.
	btnMin := imgui.ItemRectMin()
	btnMax := imgui.ItemRectMax()
	vp := imgui.WindowViewport()
	if open && vp != nil {
		a.openAddActionPopup(vp, btnMin, btnMax)
	}
}

// openAddActionPopup runs a floating SDL3 popup listing the add-action
// options (prefMenuAddSorted). Clicking a row sets d.addActionIdx and
// closes the popup. Anchored ABOVE the selection button (opening upward
// over the dialog) so it never covers the Add Item / Apply / OK row, with
// below/right/left fallbacks when there's no room above. Coords are
// relative to the parent viewport's OS-window top-left, the same scheme
// renderContextMenu uses. RunImGuiPopup blocks (on its own ImGui
// context) until dismissed.
func (a *Window) openAddActionPopup(vp *imgui.Viewport, btnMin, btnMax imgui.Vec2) {
	d := &a.prefDialog
	parentID := vp.PlatformHandle()
	vpPos := vp.Pos()

	// Surface size: wide enough for the labels, tall enough for the rows.
	// The inner list auto-sizes to its content (transparent slack is
	// invisible), so this only needs to be an upper bound on the content
	// height. rowH comes from the main context's font; floor it at 18 so
	// it never UNDER-counts the popup's default-font rows (which would
	// clip the auto-sized window).
	rowH := imgui.TextLineHeightWithSpacing()
	if rowH < 18 {
		rowH = 18
	}
	popupW := 240
	popupH := int(rowH)*len(prefMenuAddSorted) + 16
	if popupH > 420 {
		popupH = 420
	}

	// Pop the list UPWARD, above the button (like a menu that opens over
	// the control), so it never obscures the Add Item / Apply / OK row
	// directly underneath the chooser. Fall back through below → right →
	// left as each direction proves to overflow the usable screen bounds
	// (which exclude the macOS Dock and Linux taskbars).
	const gap = 4
	rightX := int(btnMax.X) + gap
	leftX := int(btnMin.X) - popupW - gap
	belowY := int(btnMax.Y) + gap
	aboveY := int(btnMin.Y) - popupH - gap
	topY := int(btnMin.Y)

	usableX, usableY, usableW, usableH := 0, 0, 0, 0
	haveUsable := false
	if x, y, w, h, ok := platform.WindowUsableBounds(parentID); ok {
		usableX, usableY, usableW, usableH = x, y, w, h
		haveUsable = true
	}

	// Returns absolute screen rect.
	fits := func(absX, absY int) bool {
		if !haveUsable {
			return true
		}
		return absX >= usableX &&
			absY >= usableY &&
			absX+popupW <= usableX+usableW &&
			absY+popupH <= usableY+usableH
	}
	clampY := func(absY int) int {
		if !haveUsable {
			return absY
		}
		if absY+popupH > usableY+usableH {
			absY = usableY + usableH - popupH
		}
		if absY < usableY {
			absY = usableY
		}
		return absY
	}

	var absX, absY int
	// placedAbove: the list opens upward, so anchor its content to the
	// BOTTOM of the (transparent, deliberately-oversized) surface — see
	// the draw callback. Otherwise content anchors at the surface top.
	placedAbove := false
	switch {
	case fits(int(btnMin.X), aboveY):
		absX, absY = int(btnMin.X), aboveY
		placedAbove = true
	case fits(int(btnMin.X), belowY):
		absX, absY = int(btnMin.X), belowY
	case fits(rightX, topY):
		absX, absY = rightX, topY
	case fits(leftX, topY):
		absX, absY = leftX, topY
	default:
		// No clean fit anywhere. Prefer upward (above the button) and
		// clamp vertically so as much of the list as possible stays on
		// screen — better than dropping behind the Dock/taskbar.
		absX, absY = int(btnMin.X), clampY(aboveY)
		placedAbove = true
	}

	relX := absX - int(vpPos.X)
	relY := absY - int(vpPos.Y)
	if relX < 0 {
		relX = 0
	}
	if relY < 0 {
		relY = 0
	}

	platform.RunImGuiPopup(parentID, relX, relY, popupW, popupH,
		func() platform.PopupMenuDrawResult {
			// Auto-size the list window to its content. The popup renders
			// in a separate ImGui context with the small default font, so
			// forcing popupH (computed from the main window's larger font)
			// painted a gray gap below the last row. The surface stays the
			// oversized popupH but is transparent, so the slack is
			// invisible. For an upward-opening list, anchor the window's
			// BOTTOM to the surface bottom (pivot 0,1) so its bottom edge
			// sits just above the button and it grows upward into the
			// transparent slack; otherwise anchor the top-left.
			if placedAbove {
				imgui.SetNextWindowPosV(imgui.Vec2{X: 0, Y: float32(popupH)}, imgui.CondAlways, imgui.Vec2{X: 0, Y: 1})
			} else {
				imgui.SetNextWindowPos(imgui.Vec2{X: 0, Y: 0})
			}
			flags := imgui.WindowFlagsNoTitleBar |
				imgui.WindowFlagsNoResize |
				imgui.WindowFlagsNoMove |
				imgui.WindowFlagsNoSavedSettings |
				imgui.WindowFlagsNoCollapse |
				imgui.WindowFlagsAlwaysAutoResize
			var res platform.PopupMenuDrawResult
			if imgui.BeginV("##addactionpopup", nil, flags) {
				// Type-to-jump: move keyboard focus to the first label
				// matching what the user typed this frame.
				tmTarget := -1
				if imgui.IsWindowFocusedV(imgui.FocusedFlagsNone) {
					tmTarget = addChooserTypematchTarget()
				}
				for i, opt := range prefMenuAddSorted {
					selected := int32(i) == d.addActionIdx
					if i == tmTarget {
						imgui.SetKeyboardFocusHereV(0)
						// If the mouse just moved over the popup, ImGui
						// flipped to mouse-nav and hid the keyboard-nav
						// cursor, suppressing this typed jump until an
						// arrow re-engages. Re-assert keyboard nav now.
						imgui.InternalSetNavCursorVisibleAfterMove()
						imgui.CurrentContext().SetNavInputSource(imgui.InputSourceKeyboard)
					}
					clicked := imgui.SelectableBoolV(opt.label+fmt.Sprintf("##ao%d", i), selected, 0, imgui.Vec2{X: 0, Y: 0})
					// Anchor initial keyboard focus on the current
					// selection so Up/Down start from there.
					if selected {
						imgui.SetItemDefaultFocus()
					}
					if clicked {
						d.addActionIdx = int32(i)
						res.Close = true
					}
				}
			}
			imgui.End()
			// Click in the transparent slack (outside the list) dismisses.
			if imgui.IsMouseClickedBool(imgui.MouseButtonLeft) || imgui.IsMouseClickedBool(imgui.MouseButtonRight) {
				if !imgui.CurrentIO().WantCaptureMouse() {
					res.Close = true
				}
			}
			return res
		})
	d.addActionPopupClosedFrame = int(imgui.FrameCount())
	platform.PostWake()
}

// Type-to-jump state for the add-action popup. One popup is open at a
// time (RunImGuiPopup blocks), so package-level state is safe; the
// prefix resets after a short idle.
var (
	addChooserTMPrefix   string
	addChooserTMLastNano int64
)

// addChooserTypematchTarget consumes characters typed this frame and
// returns the index into prefMenuAddSorted of the first label matching
// the accumulated prefix (prefix-match preferred, substring fallback),
// or -1 when nothing was typed this frame / nothing matches.
func addChooserTypematchTarget() int {
	nowNano := time.Now().UnixNano()
	if nowNano-addChooserTMLastNano > int64(700*time.Millisecond) {
		addChooserTMPrefix = ""
	}
	var typed string
	for _, ch := range imgui.CurrentIO().InputQueueCharacters().Slice() {
		if ch >= 0x20 && ch < 0x7f { // printable ASCII
			typed += string(rune(ch))
		}
	}
	if typed == "" {
		return -1
	}
	addChooserTMLastNano = nowNano
	addChooserTMPrefix += strings.ToLower(typed)
	for i, o := range prefMenuAddSorted {
		if strings.HasPrefix(strings.ToLower(o.label), addChooserTMPrefix) {
			return i
		}
	}
	for i, o := range prefMenuAddSorted {
		if strings.Contains(strings.ToLower(o.label), addChooserTMPrefix) {
			return i
		}
	}
	return -1
}

// selectedAddAction is the single source of truth for what the Add combo
// will create. The combo renders prefMenuAddSorted's labels, so the
// selection MUST be mapped back through the SAME sorted slice — indexing
// the unsorted prefMenuActions/prefMenuAddOptions with d.addActionIdx
// would add the wrong item (the bug always landed on "Reset Terminal").
// Both consumers — top-level "Add Item" and a submenu's "+" — go through
// here.
func (d *configDialog) selectedAddAction() string {
	return menuAddSelection(d.addActionIdx)
}

// menuAddSelection maps the Add combo's selected index (into the
// label-sorted prefMenuAddSorted) back to the action/sentinel it adds.
// Out-of-range falls back to the first option, never a wrong action.
func menuAddSelection(idx int32) string {
	if idx < 0 || int(idx) >= len(prefMenuAddSorted) {
		return prefMenuAddSorted[0].action
	}
	return prefMenuAddSorted[idx].action
}

// renderMenuLevel draws one level of the menu editor and recurses into
// each submenu, indented. Only submenu rows expose an editable name
// InputText (a submenu's name is its identity); action and separator
// rows are read-only Text so the action's identity stays visible and
// can't be blanked into a mystery row. Structural edits (reorder /
// remove / add-child) are recorded
// during the loop and applied after it so the slice isn't mutated
// mid-iteration. Widget IDs are derived from idp+index so they stay
// unique across the whole nested tree — colliding ImGui IDs would make
// sibling widgets share state. The + button (add a child, of the kind
// the Add combo currently selects) shows ONLY on submenu rows; an action
// or separator can't hold children.
func (a *Window) renderMenuLevel(items *[]menuEditorItem, depth int, idp string) {
	d := &a.prefDialog
	list := *items
	n := len(list)

	removeIdx := -1
	swapA, swapB := -1, -1
	addChildIdx := -1

	indentX := 8 + float32(depth)*18

	for i := range list {
		item := &list[i]
		id := fmt.Sprintf("%s_%d", idp, i)
		isSep := item.action == "separator"

		imgui.SetCursorPosX(indentX)

		switch {
		case isSep:
			imgui.AlignTextToFramePadding()
			imgui.Text("──────────  (separator)")
		case item.isSubmenu:
			// A submenu's name IS its identity, so it stays editable.
			imgui.AlignTextToFramePadding()
			imgui.Text("▸")
			imgui.SameLineV(0, 6)
			imgui.SetNextItemWidth(200)
			imgui.InputTextWithHint("##lbl"+id, "submenu name", &item.label, 0, nil)
		default:
			// Action rows are READ-ONLY: the action's identity must
			// always be visible. Renaming/blanking the label here would
			// leave an anonymous mystery row (the editor shows the label,
			// not the action id). Show the friendly label + shortcut hint.
			label := item.label
			if label == "" {
				label = menuAddLabel(item.action)
			}
			text := "  " + label
			if item.shortcut != "" {
				text += "  (" + item.shortcut + ")"
			}
			imgui.AlignTextToFramePadding()
			imgui.Text(text)
		}

		// Button column, right-aligned: ^ v [+|spacer] X. The + slot is
		// always reserved (real button on submenus, blank dummy elsewhere)
		// so the X column lines up across every row.
		imgui.SameLineV(imgui.WindowWidth()-104, 0)

		dis := i == 0
		if dis {
			imgui.BeginDisabled()
		}
		if imgui.ButtonV("^##up"+id, imgui.Vec2{X: 22, Y: 0}) {
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
		if imgui.ButtonV("v##dn"+id, imgui.Vec2{X: 22, Y: 0}) {
			swapA, swapB = i, i+1
		}
		if dis {
			imgui.EndDisabled()
		}

		imgui.SameLineV(0, 2)

		if item.isSubmenu {
			if imgui.ButtonV("+##add"+id, imgui.Vec2{X: 22, Y: 0}) {
				addChildIdx = i
			}
		} else {
			imgui.Dummy(imgui.Vec2{X: 22, Y: 0})
		}

		imgui.SameLineV(0, 2)

		if imgui.ButtonV("X##rm"+id, imgui.Vec2{X: 22, Y: 0}) {
			removeIdx = i
		}

		// Recurse into the submenu's children, indented one level deeper.
		// Edits inside mutate list[i].submenu in place via the pointer,
		// independent of this level's index shifts.
		if item.isSubmenu {
			a.renderMenuLevel(&item.submenu, depth+1, id)
		}
	}

	// Apply modifications after iteration.
	if swapA >= 0 && swapB >= 0 {
		list[swapA], list[swapB] = list[swapB], list[swapA]
	}
	if addChildIdx >= 0 {
		list[addChildIdx].submenu = append(list[addChildIdx].submenu,
			newMenuEditorItem(d.selectedAddAction()))
	}
	if removeIdx >= 0 {
		list = append(list[:removeIdx], list[removeIdx+1:]...)
	}
	*items = list
}

func (a *Window) renderPrefWindow() {
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
