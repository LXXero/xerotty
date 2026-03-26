// Package themes handles theme loading from TOML files.
package themes

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/LXXero/xerotty/internal/renderer"
)

// ThemeFile represents the TOML structure of a theme file.
type ThemeFile struct {
	Theme struct {
		Name   string `toml:"name"`
		Colors struct {
			Foreground  string `toml:"foreground"`
			Background  string `toml:"background"`
			Cursor      string `toml:"cursor"`
			SelectionFg string `toml:"selection_fg"`
			SelectionBg string `toml:"selection_bg"`
			Bold        string `toml:"bold"`
			ANSI        struct {
				Black        string `toml:"black"`
				Red          string `toml:"red"`
				Green        string `toml:"green"`
				Yellow       string `toml:"yellow"`
				Blue         string `toml:"blue"`
				Magenta      string `toml:"magenta"`
				Cyan         string `toml:"cyan"`
				White        string `toml:"white"`
				BrightBlack  string `toml:"bright_black"`
				BrightRed    string `toml:"bright_red"`
				BrightGreen  string `toml:"bright_green"`
				BrightYellow string `toml:"bright_yellow"`
				BrightBlue   string `toml:"bright_blue"`
				BrightMagenta string `toml:"bright_magenta"`
				BrightCyan   string `toml:"bright_cyan"`
				BrightWhite  string `toml:"bright_white"`
			} `toml:"ansi"`
		} `toml:"colors"`
		UI struct {
			TabBarBg          string `toml:"tab_bar_bg"`
			TabActiveBg       string `toml:"tab_active_bg"`
			TabActiveFg       string `toml:"tab_active_fg"`
			TabInactiveBg     string `toml:"tab_inactive_bg"`
			TabInactiveFg     string `toml:"tab_inactive_fg"`
			ScrollbarBg       string `toml:"scrollbar_bg"`
			ScrollbarThumb    string `toml:"scrollbar_thumb"`
			ScrollbarThumbHov string `toml:"scrollbar_thumb_hover"`
		} `toml:"ui"`
	} `toml:"theme"`
}

// Load loads a theme by name, searching bundled and user directories.
func Load(name string) (renderer.Theme, error) {
	// Search paths: user themes first, then bundled
	paths := searchPaths(name)

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}

		var tf ThemeFile
		if _, err := toml.Decode(string(data), &tf); err != nil {
			continue
		}

		return themeFromFile(tf), nil
	}

	// Fallback to default
	return renderer.DefaultTheme(), nil
}

func searchPaths(name string) []string {
	var paths []string

	configDir, err := os.UserConfigDir()
	if err == nil {
		paths = append(paths, filepath.Join(configDir, "xerotty", "themes", name+".toml"))
	}

	// Bundled themes relative to executable
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		paths = append(paths, filepath.Join(dir, "themes", name+".toml"))
		paths = append(paths, filepath.Join(dir, "..", "themes", name+".toml"))
	}

	return paths
}

func themeFromFile(tf ThemeFile) renderer.Theme {
	c := tf.Theme.Colors
	a := c.ANSI

	theme := renderer.DefaultTheme()

	if c.Foreground != "" {
		theme.Foreground = renderer.HexToABGR(c.Foreground)
	}
	if c.Background != "" {
		theme.Background = renderer.HexToABGR(c.Background)
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

	return theme
}
