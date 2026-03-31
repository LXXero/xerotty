// Package config handles TOML configuration parsing, defaults, and validation.
package config

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration struct parsed from config.toml.
type Config struct {
	Shell      string     `toml:"shell"`
	Term       string     `toml:"term"`
	Appearance Appearance `toml:"appearance"`
	Font       FontConfig `toml:"font"`
	Keybinds   map[string]string `toml:"keybinds"`
	Keys       KeyConfig  `toml:"keys"`
	Menu       MenuConfig `toml:"menu"`
	Scrollback ScrollbackConfig `toml:"scrollback"`
	Scrollbar  ScrollbarConfig  `toml:"scrollbar"`
	Links      LinksConfig      `toml:"links"`
	Env        map[string]string `toml:"env"`
	Tabs       TabConfig  `toml:"tabs"`
}

// Appearance controls visual settings.
type Appearance struct {
	Theme         string  `toml:"theme"`
	Opacity       float32 `toml:"opacity"`
	Padding       int     `toml:"padding"`
	CursorStyle   string  `toml:"cursor_style"`
	CursorBlink   bool    `toml:"cursor_blink"`
	BlinkRate     int     `toml:"blink_rate_ms"`
	BoldIsBright  bool    `toml:"bold_is_bright"`
	TabColors     string  `toml:"tab_colors"`
	ScrollbarBg   string  `toml:"scrollbar_bg"`
	ScrollbarThumb string `toml:"scrollbar_thumb"`
}

// FontConfig controls font loading.
type FontConfig struct {
	Family string  `toml:"family"`
	Size   float32 `toml:"size"`
	Path   string  `toml:"path"`
}

// KeyConfig controls special key behavior.
type KeyConfig struct {
	Backspace  string `toml:"backspace"`
	Delete     string `toml:"delete"`
	ShiftEnter string `toml:"shift_enter"`
}

// MenuConfig holds the right-click context menu definition.
type MenuConfig struct {
	Items []MenuItem `toml:"items"`
}

// MenuItem is a single context menu entry.
type MenuItem struct {
	Label    string     `toml:"label"`
	Action   string     `toml:"action"`
	Shortcut string     `toml:"shortcut"`
	Enabled  string     `toml:"enabled"`
	Submenu  []MenuItem `toml:"submenu"`
}

// ScrollbackConfig controls scrollback buffer behavior.
type ScrollbackConfig struct {
	Lines      int    `toml:"lines"`
	Mode       string `toml:"mode"`        // "memory" | "disk" | "unlimited"
	DiskDir    string `toml:"disk_dir"`
	ScrollSpeed int   `toml:"scroll_speed"` // lines per mouse wheel tick
}

// ScrollbarConfig controls the scrollbar.
type ScrollbarConfig struct {
	Visible        string `toml:"visible"`          // "always" | "never" | "auto"
	Width          int    `toml:"width"`
	MinThumbHeight int    `toml:"min_thumb_height"`
}

// LinksConfig controls URL detection and interaction.
type LinksConfig struct {
	Enabled    bool   `toml:"enabled"`
	CtrlClick  bool   `toml:"ctrl_click"`
	Opener     string `toml:"opener"`
}

// TabConfig controls tab behavior.
type TabConfig struct {
	OnChildExit string `toml:"on_child_exit"` // "close" | "hold" | "hold_on_error"
}

// Default returns a Config with sensible defaults.
func Default() Config {
	return Config{
		Shell: "",
		Term:  "xterm-256color",
		Appearance: Appearance{
			Theme:       "dracula",
			Opacity:     1.0,
			Padding:     2,
			CursorStyle: "block",
			CursorBlink: true,
			BlinkRate:   530,
			BoldIsBright: true,
			TabColors:   "theme",
		},
		Font: FontConfig{
			Family: "monospace",
			Size:   14,
		},
		Keybinds: defaultKeybinds(),
		Keys: KeyConfig{
			Backspace:  "ascii_del",
			Delete:     "vt_sequence",
			ShiftEnter: "newline",
		},
		Menu: defaultMenu(),
		Scrollback: ScrollbackConfig{
			Lines:       10000,
			Mode:        "memory",
			ScrollSpeed: 3,
		},
		Scrollbar: ScrollbarConfig{
			Visible:        "always",
			Width:          12,
			MinThumbHeight: 20,
		},
		Links: LinksConfig{
			Enabled:   true,
			CtrlClick: true,
			Opener:    "xdg-open",
		},
		Tabs: TabConfig{
			OnChildExit: "close",
		},
	}
}

// Load reads config from the standard path, merging with defaults.
func Load() (Config, error) {
	cfg := Default()

	configDir, err := os.UserConfigDir()
	if err != nil {
		return cfg, nil
	}

	path := filepath.Join(configDir, "xerotty", "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}

	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// DetectShell returns the shell to use: config override > $SHELL > /bin/sh.
func (c *Config) DetectShell() string {
	if c.Shell != "" {
		return c.Shell
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

func defaultKeybinds() map[string]string {
	return map[string]string{
		"Ctrl+Shift+T":     "new_tab",
		"Ctrl+Shift+W":     "close_tab",
		"Ctrl+Shift+N":     "new_window",
		"Ctrl+Tab":         "next_tab",
		"Ctrl+Shift+Tab":   "prev_tab",
		"Alt+1":            "goto_tab:1",
		"Alt+2":            "goto_tab:2",
		"Alt+3":            "goto_tab:3",
		"Alt+4":            "goto_tab:4",
		"Alt+5":            "goto_tab:5",
		"Alt+6":            "goto_tab:6",
		"Alt+7":            "goto_tab:7",
		"Alt+8":            "goto_tab:8",
		"Alt+9":            "goto_tab:9",
		"Ctrl+Shift+C":     "copy",
		"Ctrl+Shift+V":     "paste",
		"Shift+Insert":     "paste_selection",
		"Shift+PageUp":     "scroll_page_up",
		"Shift+PageDown":   "scroll_page_down",
		"Ctrl+Shift+F":     "search",
		"F11":              "fullscreen",
		"Ctrl+Plus":        "font_size_up",
		"Ctrl+Minus":       "font_size_down",
		"Ctrl+0":           "font_size_reset",
		"Ctrl+Shift+Plus":  "font_size_up",
		"Ctrl+Shift+Minus": "font_size_down",
		"Ctrl+Shift+0":     "font_size_reset",
		"Ctrl+Shift+R":     "rename_tab",
		"Shift+Home":       "scroll_top",
		"Shift+End":        "scroll_bottom",
	}
}

func defaultMenu() MenuConfig {
	return MenuConfig{
		Items: []MenuItem{
			{Label: "New Tab", Action: "new_tab", Shortcut: "Ctrl+Shift+T"},
			{Label: "New Window", Action: "new_window", Shortcut: "Ctrl+Shift+N"},
			{Action: "separator"},
			{Label: "Copy", Action: "copy", Shortcut: "Ctrl+Shift+C", Enabled: "has_selection"},
			{Label: "Paste", Action: "paste", Shortcut: "Ctrl+Shift+V"},
			{Action: "separator"},
			{Label: "Open Link", Action: "open_link", Enabled: "has_link"},
			{Label: "Copy Link", Action: "copy_link", Enabled: "has_link"},
			{Action: "separator"},
			{Label: "Search...", Action: "search", Shortcut: "Ctrl+Shift+F"},
			{Label: "Fullscreen", Action: "fullscreen", Shortcut: "F11"},
			{Action: "separator"},
			{Label: "Rename Tab", Action: "rename_tab", Shortcut: "Ctrl+Shift+R"},
			{Label: "Close Tab", Action: "close_tab", Shortcut: "Ctrl+Shift+W"},
		},
	}
}
