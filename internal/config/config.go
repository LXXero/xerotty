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
	Clipboard  ClipboardConfig  `toml:"clipboard"`
	Env        map[string]string `toml:"env"`
	Tabs       TabConfig  `toml:"tabs"`
	Window     WindowConfig `toml:"window"`
}

// Appearance controls visual settings.
type Appearance struct {
	Theme                string  `toml:"theme"`
	Opacity              float32 `toml:"opacity"`
	Padding              int     `toml:"padding"`
	CursorStyle          string  `toml:"cursor_style"`
	CursorBlink          bool    `toml:"cursor_blink"`
	BlinkRate            int     `toml:"blink_rate_ms"`
	BoldIsBright         bool    `toml:"bold_is_bright"`
	TerminalColors       string  `toml:"terminal_colors"`
	TabColors            string  `toml:"tab_colors"`
	ScrollbarColors      string  `toml:"scrollbar_colors"`
	ResizeOverlay        bool    `toml:"resize_overlay"`
	ResizeOverlayDuration float32 `toml:"resize_overlay_duration"`
	// Custom color overrides (hex strings, used when *_colors = "custom")
	Foreground     string `toml:"foreground"`
	Background     string `toml:"background"`
	ScrollbarBg    string `toml:"scrollbar_bg"`
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
	Lines             int    `toml:"lines"`
	Mode              string `toml:"mode"`               // "memory" | "unlimited"
	ScrollSpeed       int    `toml:"scroll_speed"`        // lines per mouse wheel tick
	ScrollOnKeystroke bool   `toml:"scroll_on_keystroke"` // snap to bottom on keypress
	ScrollOnOutput    bool   `toml:"scroll_on_output"`    // snap to bottom on new output
}

// ScrollbarConfig controls the scrollbar.
type ScrollbarConfig struct {
	Visible        string `toml:"visible"`          // "always" | "never" | "auto"
	Width          int    `toml:"width"`
	MinThumbHeight int    `toml:"min_thumb_height"`
}

// LinksConfig controls URL detection and interaction.
type LinksConfig struct {
	Enabled   bool   `toml:"enabled"`
	CtrlClick bool   `toml:"ctrl_click"`
	Opener    string `toml:"opener"`
}

// ClipboardConfig controls clipboard behavior.
type ClipboardConfig struct {
	TrimTrailingWhitespace bool              `toml:"trim_trailing_whitespace"`
	UnsafePaste            UnsafePasteConfig `toml:"unsafe_paste"`
}

// UnsafePasteConfig controls the paste safety dialog.
type UnsafePasteConfig struct {
	Enabled          bool     `toml:"enabled"`
	MultilineWarning bool     `toml:"multiline_warning"`
	NewlineGuard     bool     `toml:"newline_guard"`
	Patterns         []string `toml:"patterns"`
}

// TabConfig controls tab behavior.
type TabConfig struct {
	OnChildExit        string `toml:"on_child_exit"`         // "close" | "hold" | "hold_on_error"
	CloseButtonPosition string `toml:"close_button_position"` // "right" | "left"
}

// WindowConfig controls initial window state.
type WindowConfig struct {
	Columns    int    `toml:"columns"`
	Rows       int    `toml:"rows"`
	Title      string `toml:"title"`
	Fullscreen bool   `toml:"fullscreen"`
}

// Default returns a Config with sensible defaults.
func Default() Config {
	return Config{
		Shell: "",
		Term:  "xterm-256color",
		Appearance: Appearance{
			Theme:                 "dracula",
			Opacity:               1.0,
			Padding:               2,
			CursorStyle:           "block",
			CursorBlink:           true,
			BlinkRate:             530,
			BoldIsBright:          true,
			TerminalColors:        "theme",
			TabColors:             "theme",
			ScrollbarColors:       "theme",
			ResizeOverlay:         true,
			ResizeOverlayDuration: 1.0,
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
			Lines:             10000,
			Mode:              "memory",
			ScrollSpeed:       3,
			ScrollOnKeystroke: true,
			ScrollOnOutput:    false,
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
		Clipboard: ClipboardConfig{
			TrimTrailingWhitespace: true,
			UnsafePaste: UnsafePasteConfig{
				Enabled:          true,
				MultilineWarning: true,
				NewlineGuard:     true,
				Patterns:         []string{`sudo\s`, `rm\s+(-rf?|--recursive)`},
			},
		},
		Tabs: TabConfig{
			OnChildExit:         "close",
			CloseButtonPosition: "right",
		},
		Window: WindowConfig{
			Columns: 80,
			Rows:    24,
			Title:   "xerotty",
		},
	}
}

// Path returns the config file path.
func Path() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(configDir, "xerotty", "config.toml")
}

// Load reads config from the standard path, merging with defaults.
func Load() (Config, error) {
	cfg := Default()

	path := Path()
	if path == "" {
		return cfg, nil
	}

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

// Save writes the config to the standard path as TOML.
func Save(cfg Config) error {
	path := Path()
	if path == "" {
		return os.ErrNotExist
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	encoder := toml.NewEncoder(f)
	return encoder.Encode(cfg)
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
		"Cmd+1":            "goto_tab:1",
		"Cmd+2":            "goto_tab:2",
		"Cmd+3":            "goto_tab:3",
		"Cmd+4":            "goto_tab:4",
		"Cmd+5":            "goto_tab:5",
		"Cmd+6":            "goto_tab:6",
		"Cmd+7":            "goto_tab:7",
		"Cmd+8":            "goto_tab:8",
		"Cmd+9":            "goto_tab:9",
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
		"Ctrl+Comma":       "preferences",
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
			{Label: "Preferences", Action: "preferences", Shortcut: "Ctrl+,"},
			{Label: "Close Tab", Action: "close_tab", Shortcut: "Ctrl+Shift+W"},
		},
	}
}
