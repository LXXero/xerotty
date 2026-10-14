// Package themes handles theme loading from TOML files.
package themes

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/renderer"
)

// ThemeFile represents the TOML structure of a theme file.
type ThemeFile struct {
	Theme struct {
		Name   string `toml:"name"`
		Colors struct {
			Foreground  string `toml:"foreground"`
			Background  string `toml:"background"`
			Bold        string `toml:"bold"`
			Cursor      string `toml:"cursor"`
			SelectionFg string `toml:"selection_fg"`
			SelectionBg string `toml:"selection_bg"`
			ANSI        struct {
				Black         string `toml:"black"`
				Red           string `toml:"red"`
				Green         string `toml:"green"`
				Yellow        string `toml:"yellow"`
				Blue          string `toml:"blue"`
				Magenta       string `toml:"magenta"`
				Cyan          string `toml:"cyan"`
				White         string `toml:"white"`
				BrightBlack   string `toml:"bright_black"`
				BrightRed     string `toml:"bright_red"`
				BrightGreen   string `toml:"bright_green"`
				BrightYellow  string `toml:"bright_yellow"`
				BrightBlue    string `toml:"bright_blue"`
				BrightMagenta string `toml:"bright_magenta"`
				BrightCyan    string `toml:"bright_cyan"`
				BrightWhite   string `toml:"bright_white"`
			} `toml:"ansi"`
		} `toml:"colors"`
		UI uiSection `toml:"ui"`
	} `toml:"theme"`
}

// uiSection is a theme's UI half ([theme.ui]): everything drawn
// around the terminal rather than in it. A UI-only theme file has
// just this half, so it can be paired with any terminal theme.
type uiSection struct {
	ScrollbarBg       string `toml:"scrollbar_bg"`
	ScrollbarThumb    string `toml:"scrollbar_thumb"`
	ScrollbarThumbHov string `toml:"scrollbar_thumb_hover"`

	// Flat tab keys from the original format, still read so older
	// theme files keep loading. [theme.ui.tabs] wins where both set.
	TabActivityGlow string `toml:"tab_activity_glow"`
	TabBarBg        string `toml:"tab_bar_bg"`
	TabActiveBg     string `toml:"tab_active_bg"`
	TabActiveFg     string `toml:"tab_active_fg"`
	TabInactiveBg   string `toml:"tab_inactive_bg"`
	TabInactiveFg   string `toml:"tab_inactive_fg"`

	Tabs TabSection `toml:"tabs"`
}

// TabSection is [theme.ui.tabs]. Each value is "#RRGGBB" or
// "terminal" (the terminal's background); unset values are derived
// from the terminal colors. config.toml's [appearance.tabs] uses the
// same keys.
type TabSection struct {
	BarBg        string `toml:"bar_bg"`
	InactiveBg   string `toml:"inactive_bg"`
	InactiveText string `toml:"inactive_text"`
	ActiveBg     string `toml:"active_bg"`
	ActiveText   string `toml:"active_text"`
	ActiveMarker string `toml:"active_marker"`
	HoverBg      string `toml:"hover_bg"`
	Activity     string `toml:"activity"`
	Bell         string `toml:"bell"`
}

// Apply sets every tab color s names onto tc, leaving the rest alone.
func (s TabSection) Apply(tc *renderer.TabColors) {
	set := func(dst *uint32, v string) {
		if c := renderer.ParseTabColor(v); c != 0 {
			*dst = c
		}
	}
	set(&tc.BarBg, s.BarBg)
	set(&tc.InactiveBg, s.InactiveBg)
	set(&tc.InactiveText, s.InactiveText)
	set(&tc.ActiveBg, s.ActiveBg)
	set(&tc.ActiveText, s.ActiveText)
	set(&tc.ActiveMarker, s.ActiveMarker)
	set(&tc.HoverBg, s.HoverBg)
	set(&tc.Activity, s.Activity)
	set(&tc.Bell, s.Bell)
}

// Load loads a theme by name, searching bundled and user directories.
// Both halves come from the one file; see LoadSet to mix them.
func Load(name string) (renderer.Theme, error) {
	if tf, _, ok := readFile(name); ok {
		return themeFromFile(tf), nil
	}

	// Fallback to default
	return renderer.DefaultTheme(), nil
}

// LoadSet loads the terminal half (text, ANSI, background) from the
// terminal theme and the UI half (tabs, scrollbar) from ui. ui = ""
// or "same" keeps the terminal theme's own UI half, so a full theme
// file is a matching set. A ui theme that can't be found also keeps
// the terminal theme's UI rather than dropping to defaults.
func LoadSet(terminal, ui string) (renderer.Theme, error) {
	t, err := Load(terminal)
	if ui == "" || ui == "same" || ui == terminal {
		return t, err
	}
	tf, _, ok := readFile(ui)
	if !ok {
		return t, err
	}
	// The UI theme owns the whole tab look: start its tabs from
	// unset so keys it leaves out derive from this terminal's colors
	// instead of leaking the terminal theme's tab colors through.
	t.Tabs = renderer.TabColors{}
	applyUI(&t, tf.Theme.UI)
	return t, err
}

// Halves reports which halves a theme file defines: a terminal half
// ([theme.colors]) and a UI half ([theme.ui]). The prefs dialog uses
// it to keep UI-only files out of the terminal theme picker.
func Halves(name string) (terminal, ui bool) {
	_, md, ok := readFile(name)
	if !ok {
		return false, false
	}
	return md.IsDefined("theme", "colors"), md.IsDefined("theme", "ui")
}

// readFile finds and parses the first theme file called name,
// user themes first, then bundled.
func readFile(name string) (ThemeFile, toml.MetaData, bool) {
	for _, p := range searchPaths(name) {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var tf ThemeFile
		md, err := toml.Decode(string(data), &tf)
		if err != nil {
			continue
		}
		return tf, md, true
	}
	return ThemeFile{}, toml.MetaData{}, false
}

// SearchDirs returns every directory theme files are looked up in,
// highest priority first. The prefs dialog's theme picker enumerates
// these same dirs — keep the list HERE so the picker can never drift
// from what Load actually resolves (it did once: the picker missed
// the .app Resources dir and showed no themes on macOS).
func SearchDirs() []string {
	var dirs []string

	// Via config.Dir so the XEROTTY_CONFIG_DIR override relocates
	// user themes together with config.toml.
	if dir := config.Dir(); dir != "" {
		dirs = append(dirs, filepath.Join(dir, "themes"))
	}

	// Bundled themes relative to executable. The third path covers the
	// macOS .app bundle layout: xerotty.app/Contents/Resources/themes/
	// is the conventional Mac place for bundled data, and the binary
	// lives at xerotty.app/Contents/MacOS/xerotty — so ../Resources/
	// from the exe directory hits it.
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		dirs = append(dirs, filepath.Join(dir, "themes"))
		dirs = append(dirs, filepath.Join(dir, "..", "themes"))
		dirs = append(dirs, filepath.Join(dir, "..", "Resources", "themes"))
	}

	return dirs
}

func searchPaths(name string) []string {
	if name == "" {
		return nil
	}
	var paths []string
	for _, dir := range SearchDirs() {
		paths = append(paths, filepath.Join(dir, name+".toml"))
	}
	return paths
}

func themeFromFile(tf ThemeFile) renderer.Theme {
	c := tf.Theme.Colors
	a := c.ANSI

	theme := renderer.DefaultTheme()
	// Tab colors a file doesn't set derive from ITS terminal colors
	// (renderer.Theme.ResolveTabs), not from Dracula's defaults.
	theme.Tabs = renderer.TabColors{}

	if c.Foreground != "" {
		theme.Foreground = renderer.HexToABGR(c.Foreground)
	}
	if c.Background != "" {
		theme.Background = renderer.HexToABGR(c.Background)
	}
	if c.Bold != "" {
		theme.Bold = renderer.HexToABGR(c.Bold)
	}
	if c.Cursor != "" {
		theme.Cursor = renderer.HexToABGR(c.Cursor)
	}
	if c.SelectionFg != "" {
		theme.SelectionFg = renderer.HexToABGR(c.SelectionFg)
	}
	if c.SelectionBg != "" {
		theme.SelectionBg = renderer.HexToABGR(c.SelectionBg)
	}

	ansiColors := [16]string{
		a.Black, a.Red, a.Green, a.Yellow, a.Blue, a.Magenta, a.Cyan, a.White,
		a.BrightBlack, a.BrightRed, a.BrightGreen, a.BrightYellow,
		a.BrightBlue, a.BrightMagenta, a.BrightCyan, a.BrightWhite,
	}
	for i, hex := range ansiColors {
		if hex != "" {
			theme.ANSI[i] = renderer.HexToABGR(hex)
		}
	}

	applyUI(&theme, tf.Theme.UI)

	return theme
}

func applyUI(theme *renderer.Theme, u uiSection) {
	if u.ScrollbarBg != "" {
		theme.ScrollbarBg = renderer.HexToABGR(u.ScrollbarBg)
	}
	if u.ScrollbarThumb != "" {
		theme.ScrollbarThumb = renderer.HexToABGR(u.ScrollbarThumb)
	}
	if u.ScrollbarThumbHov != "" {
		theme.ScrollbarHover = renderer.HexToABGR(u.ScrollbarThumbHov)
	}
	TabSection{
		BarBg:        u.TabBarBg,
		InactiveBg:   u.TabInactiveBg,
		InactiveText: u.TabInactiveFg,
		ActiveBg:     u.TabActiveBg,
		ActiveText:   u.TabActiveFg,
		Activity:     u.TabActivityGlow,
	}.Apply(&theme.Tabs)
	u.Tabs.Apply(&theme.Tabs)
}
