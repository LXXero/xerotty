// Package config handles TOML configuration parsing, defaults, and validation.
package config

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration struct parsed from config.toml.
type Config struct {
	Shell string `toml:"shell"`
	// Renderer selects the GPU backend: "" / "gl" = OpenGL (default),
	// "gpu" = SDL_GPU (Metal on macOS, Vulkan on Linux). The
	// XEROTTY_GPU env var overrides when set — but env doesn't reach
	// Finder/app-menu launches, which is exactly what this option is
	// for.
	Renderer   string            `toml:"renderer"`
	Term       string            `toml:"term"`
	Appearance Appearance        `toml:"appearance"`
	Font       FontConfig        `toml:"font"`
	Keybinds   map[string]string `toml:"keybinds"`
	Keys       KeyConfig         `toml:"keys"`
	Menu       MenuConfig        `toml:"menu"`
	Scrollback ScrollbackConfig  `toml:"scrollback"`
	Scrollbar  ScrollbarConfig   `toml:"scrollbar"`
	Links      LinksConfig       `toml:"links"`
	Clipboard  ClipboardConfig   `toml:"clipboard"`
	Env        map[string]string `toml:"env"`
	Tabs       TabConfig         `toml:"tabs"`
	Hosts      []RemoteHost      `toml:"hosts"`
	Window     WindowConfig      `toml:"window"`
	MCP        MCPConfig         `toml:"mcp"`
}

// MCPConfig controls the AI-agent control socket's trust model.
//
// The default (observe, mode changes allowed) is fine for a local
// trusted setup where you run the agent yourself. To turn the
// propose queue into a real human-approval gate:
//
//	[mcp]
//	default_mode = "propose"   # agents land here
//	allow_mode_change = false  # ...and can't elevate themselves
//	approval_token = "secret"  # your review tool authenticates
//	                           # with this to gain auto authority
//
// With that, a propose-mode agent can queue writes but can't
// approve them; only a connection that presented approval_token
// (via the agent/authenticate method) can approve/drop.
type MCPConfig struct {
	// DefaultMode is the mode every new MCP connection starts in.
	// "observe" (default) | "propose" | "auto".
	DefaultMode string `toml:"default_mode"`
	// AllowModeChange lets connections call agent/mode to change
	// their own mode. Default true. Set false to pin connections
	// at DefaultMode (except token-authenticated ones).
	AllowModeChange bool `toml:"allow_mode_change"`
	// ApprovalToken, when non-empty, is the shared secret a
	// connection presents via agent/authenticate to gain
	// approval authority (auto mode) even when AllowModeChange
	// is false. Empty = no token auth.
	ApprovalToken string `toml:"approval_token"`
}

// GlowConfig is the animated background ("lava lamp") layer: soft
// color blobs drifting behind the terminal cells. Opt-in — it
// self-wakes the otherwise event-driven render loop at FPS while
// enabled. Colors empty = derive from the active theme's accents.
type GlowConfig struct {
	Enabled bool `toml:"enabled"`
	// BackgroundFPS caps the glow ANIMATION rate in windows that are
	// neither OS-focused nor under the mouse (default 10, 0 = full
	// FPS everywhere). Content updates are never throttled — a
	// background window's streaming text renders the moment it
	// changes; only the decorative lamp coalesces. The focused /
	// hovered window always animates at full FPS.
	BackgroundFPS int      `toml:"background_fps"`
	Style         string   `toml:"style"` // "lava" (reserved for future styles)
	Colors        []string `toml:"colors"`
	Blobs         int      `toml:"blobs"`
	Speed         float64  `toml:"speed"`
	Scale         float64  `toml:"scale"`
	Intensity     float64  `toml:"intensity"`
	FPS           int      `toml:"fps"`
}

// Appearance controls visual settings.
type Appearance struct {
	Theme                 string  `toml:"theme"`
	Opacity               float32 `toml:"opacity"`
	Padding               int     `toml:"padding"`
	CursorStyle           string  `toml:"cursor_style"`
	CursorBlink           bool    `toml:"cursor_blink"`
	BlinkRate             int     `toml:"blink_rate_ms"`
	BoldIsBright          bool    `toml:"bold_is_bright"`
	TerminalColors        string  `toml:"terminal_colors"`
	TabColors             string  `toml:"tab_colors"`
	ScrollbarColors       string  `toml:"scrollbar_colors"`
	ResizeOverlay         bool    `toml:"resize_overlay"`
	ResizeOverlayDuration float32 `toml:"resize_overlay_duration"`
	// Glow is the animated lava-lamp backdrop (see internal/app/glow.go).
	Glow GlowConfig `toml:"glow"`
	// Custom color overrides (hex strings, used when *_colors = "custom")
	Foreground     string `toml:"foreground"`
	Background     string `toml:"background"`
	TabBarBg       string `toml:"tab_bar_bg"`
	TabActiveBg    string `toml:"tab_active_bg"`
	TabActiveFg    string `toml:"tab_active_fg"`
	TabInactiveBg  string `toml:"tab_inactive_bg"`
	TabInactiveFg  string `toml:"tab_inactive_fg"`
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
	HomeEnd    string `toml:"home_end"`
}

// MenuConfig holds the right-click context menu definition.
type MenuConfig struct {
	Items []MenuItem `toml:"items"`
}

// MenuItem is a single context menu entry.
type MenuItem struct {
	Label    string `toml:"label"`
	Action   string `toml:"action"`
	Shortcut string `toml:"shortcut"`
	Enabled  string `toml:"enabled"`
	// Checked is an optional state predicate (like Enabled). When it
	// evaluates true the item renders with a "toggled on" highlight —
	// e.g. "force_opaque" for the opacity toggle. Empty = never checked.
	Checked string     `toml:"checked"`
	Submenu []MenuItem `toml:"submenu"`
}

// ScrollbackConfig controls scrollback buffer behavior.
type ScrollbackConfig struct {
	Lines             int    `toml:"lines"`
	Mode              string `toml:"mode"`                // "memory" | "unlimited"
	ScrollSpeed       int    `toml:"scroll_speed"`        // lines per mouse wheel tick
	DragScrollSpeed   int    `toml:"drag_scroll_speed"`   // rows/second when drag-selecting past an edge
	ScrollOnKeystroke bool   `toml:"scroll_on_keystroke"` // snap to bottom on keypress
	ScrollOnOutput    bool   `toml:"scroll_on_output"`    // snap to bottom on new output
}

// ScrollbarConfig controls the scrollbar.
type ScrollbarConfig struct {
	Visible        string `toml:"visible"` // "always" | "never" | "auto"
	Width          int    `toml:"width"`
	MinThumbHeight int    `toml:"min_thumb_height"`
}

// LinksConfig controls URL detection and interaction.
type LinksConfig struct {
	Enabled     bool   `toml:"enabled"`
	CtrlClick   bool   `toml:"ctrl_click"`
	DoubleClick bool   `toml:"double_click"`
	Opener      string `toml:"opener"`
}

// ClipboardConfig controls clipboard behavior.
type ClipboardConfig struct {
	CopyOnSelect           bool              `toml:"copy_on_select"`
	PasteOnMiddleClick     bool              `toml:"paste_on_middle_click"`
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
// RemoteHost is one entry in the user's host registry. Tabs can be
// opened against a named host via the menu action "new_tab_remote:<name>"
// or by setting cfg.Tabs.Source = "daemon:<name>". xerotty connects
// to that host via SSH and routes the tab's PTY through the remote
// daemon. The connection is persistent (auto-spawned remote daemon
// stays up across SSH disconnects) so tab survival works
// cross-machine.
type RemoteHost struct {
	Name      string   `toml:"name"`       // referenced by menu actions / cfg.Tabs.Source
	SSHDest   string   `toml:"ssh_dest"`   // anything ssh(1) accepts: "user@host", "host", an alias
	SSHArgs   []string `toml:"ssh_args"`   // optional extra args before <dest>: -i, -p, ...
	RemoteCmd string   `toml:"remote_cmd"` // override; default "xerotty serve --stdio"
}

type TabConfig struct {
	OnChildExit         string `toml:"on_child_exit"`         // "close" | "hold" | "hold_on_error"
	InheritCWD          bool   `toml:"inherit_cwd"`           // new tabs inherit parent CWD
	CloseButtonPosition string `toml:"close_button_position"` // "right" | "left"

	// Source picks where new tabs get their PTY:
	//   "pty"    (default) — in-process PTY, no daemon needed.
	//   "daemon" — talk to an xerotty daemon (xerotty serve). If
	//              one isn't running on the local socket, xerotty
	//              forks one in the background at GUI startup
	//              ("auto-spawn"). Lets tabs survive the GUI
	//              crashing and lets you reattach from another
	//              machine via xerotty connect --ssh.
	Source string `toml:"source"`

	// DaemonSocket overrides the unix-socket path xerotty connects
	// to when Source == "daemon". Empty = default
	// $XDG_RUNTIME_DIR/xerottyd.sock.
	DaemonSocket string `toml:"daemon_socket"`
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
			Theme:          "dracula",
			Opacity:        1.0,
			Padding:        2,
			CursorStyle:    "block",
			CursorBlink:    true,
			BlinkRate:      530,
			BoldIsBright:   true,
			TerminalColors: "theme",
			TabColors:      "theme",
			Glow: GlowConfig{
				Style:         "lava",
				BackgroundFPS: 10,
				Blobs:         5,
				Speed:         1.0,
				Scale:         0.7,
				Intensity:     0.35,
				FPS:           20,
			},
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
			Backspace: "ascii_del",
			// ss3 (ESC O H/F) is terminfo khome/kend for
			// xterm-256color, so the standard zsh key-binding
			// boilerplate (`bindkey "${terminfo[khome]}" ...`) and
			// readline both act on it WITHOUT the shell having to
			// enable application-cursor mode (smkx). "auto" only sent
			// SS3 in app mode, so Home/End silently did nothing in a
			// normal-mode interactive shell. ss3 "just works".
			HomeEnd:    "ss3",
			Delete:     "vt_sequence",
			ShiftEnter: "newline",
		},
		Menu: defaultMenu(),
		Scrollback: ScrollbackConfig{
			Lines:             10000,
			Mode:              "memory",
			ScrollSpeed:       3,
			DragScrollSpeed:   25,
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
			Opener:    DefaultOpener(),
		},
		Clipboard: ClipboardConfig{
			// Default off to match xterm / gnome-terminal / xfce4-terminal
			// / iTerm2 / kitty / alacritty: selection updates PRIMARY only,
			// CLIPBOARD only changes via explicit Copy. Users who want the
			// "auto-copy on select" behavior opt in via prefs.
			CopyOnSelect:           false,
			PasteOnMiddleClick:     true,
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
			InheritCWD:          false,
			CloseButtonPosition: "right",
		},
		Window: WindowConfig{
			Columns: 80,
			Rows:    24,
			Title:   "xerotty",
		},
		MCP: MCPConfig{
			DefaultMode:     "observe",
			AllowModeChange: true,
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

// DefaultOpener returns the platform's URL-opener command: macOS
// ships `open`; xdg-open is the freedesktop convention everywhere
// else.
func DefaultOpener() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}

// DetectShell returns the shell to use: config override > $SHELL > platform default.
func (c *Config) DetectShell() string {
	if c.Shell != "" {
		return c.Shell
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	if runtime.GOOS == "darwin" {
		return "/bin/zsh"
	}
	return "/bin/sh"
}

func defaultKeybinds() map[string]string {
	if runtime.GOOS == "darwin" {
		return defaultKeybindsDarwin()
	}
	return defaultKeybindsLinux()
}

func defaultKeybindsLinux() map[string]string {
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
		"Ctrl+Shift+O":     "toggle_opacity",
	}
}

// defaultKeybindsDarwin returns iTerm2-style defaults: drop the extra Shift
// modifier that Linux terminals need to disambiguate from Ctrl+letter control
// codes. ImGui's ConfigMacOSXBehaviors maps the physical Cmd key onto cimgui's
// ModCtrl flag, so "Ctrl+T" in this map fires on physical Cmd+T.
func defaultKeybindsDarwin() map[string]string {
	return map[string]string{
		"Ctrl+T":         "new_tab",
		"Ctrl+W":         "close_tab",
		"Ctrl+N":         "new_window",
		"Ctrl+Tab":       "next_tab",
		"Ctrl+Shift+Tab": "prev_tab",
		"Ctrl+1":         "goto_tab:1",
		"Ctrl+2":         "goto_tab:2",
		"Ctrl+3":         "goto_tab:3",
		"Ctrl+4":         "goto_tab:4",
		"Ctrl+5":         "goto_tab:5",
		"Ctrl+6":         "goto_tab:6",
		"Ctrl+7":         "goto_tab:7",
		"Ctrl+8":         "goto_tab:8",
		"Ctrl+9":         "goto_tab:9",
		"Ctrl+C":         "copy",
		"Ctrl+V":         "paste",
		"Shift+Insert":   "paste_selection",
		"Shift+PageUp":   "scroll_page_up",
		"Shift+PageDown": "scroll_page_down",
		"Ctrl+F":         "search",
		"F11":            "fullscreen",
		"Ctrl+Plus":      "font_size_up",
		"Ctrl+Minus":     "font_size_down",
		"Ctrl+0":         "font_size_reset",
		"Ctrl+R":         "rename_tab",
		"Shift+Home":     "scroll_top",
		"Shift+End":      "scroll_bottom",
		"Ctrl+Comma":     "preferences",
		"Ctrl+O":         "toggle_opacity",
	}
}

func defaultMenu() MenuConfig {
	if runtime.GOOS == "darwin" {
		return defaultMenuDarwin()
	}
	return defaultMenuLinux()
}

func defaultMenuLinux() MenuConfig {
	return MenuConfig{
		Items: []MenuItem{
			{Label: "New Tab", Action: "new_tab", Shortcut: "Ctrl+Shift+T"},
			{Label: "New Window", Action: "new_window", Shortcut: "Ctrl+Shift+N"},
			// "_remote_hosts" is a placeholder action expanded at
			// render time into a "Remote" submenu with per-host
			// new-tab / reattach items, one pair per [[hosts]]
			// entry. Collapses to nothing when no hosts are
			// configured.
			{Action: "_remote_hosts"},
			{Action: "separator"},
			{Label: "Copy", Action: "copy", Shortcut: "Ctrl+Shift+C", Enabled: "has_selection"},
			{Label: "Paste", Action: "paste", Shortcut: "Ctrl+Shift+V"},
			{Action: "separator"},
			{Label: "Open Link", Action: "open_link", Enabled: "has_link"},
			{Label: "Copy Link", Action: "copy_link", Enabled: "has_link"},
			{Action: "separator"},
			{Label: "Search...", Action: "search", Shortcut: "Ctrl+Shift+F"},
			{Label: "Fullscreen", Action: "fullscreen", Shortcut: "F11"},
			{Label: "Toggle Opacity", Action: "toggle_opacity", Shortcut: "Ctrl+Shift+O", Checked: "force_opaque"},
			{Action: "separator"},
			{Label: "Rename Tab", Action: "rename_tab", Shortcut: "Ctrl+Shift+R"},
			{Label: "Preferences", Action: "preferences", Shortcut: "Ctrl+,"},
			{Label: "Close Tab", Action: "close_tab", Shortcut: "Ctrl+Shift+W"},
		},
	}
}

func defaultMenuDarwin() MenuConfig {
	return MenuConfig{
		Items: []MenuItem{
			{Label: "New Tab", Action: "new_tab", Shortcut: "Cmd+T"},
			{Label: "New Window", Action: "new_window", Shortcut: "Cmd+N"},
			// _remote_hosts expands to per-host new/reattach
			// entries at render time (see app.expandMenu).
			// Collapses when cfg.Hosts is empty.
			{Action: "_remote_hosts"},
			{Action: "separator"},
			{Label: "Copy", Action: "copy", Shortcut: "Cmd+C", Enabled: "has_selection"},
			{Label: "Paste", Action: "paste", Shortcut: "Cmd+V"},
			{Action: "separator"},
			{Label: "Open Link", Action: "open_link", Enabled: "has_link"},
			{Label: "Copy Link", Action: "copy_link", Enabled: "has_link"},
			{Action: "separator"},
			{Label: "Search...", Action: "search", Shortcut: "Cmd+F"},
			{Label: "Fullscreen", Action: "fullscreen", Shortcut: "F11"},
			{Label: "Toggle Opacity", Action: "toggle_opacity", Shortcut: "Cmd+O", Checked: "force_opaque"},
			{Action: "separator"},
			{Label: "Rename Tab", Action: "rename_tab", Shortcut: "Cmd+R"},
			{Label: "Preferences", Action: "preferences", Shortcut: "Cmd+,"},
			{Label: "Close Tab", Action: "close_tab", Shortcut: "Cmd+W"},
		},
	}
}

// DefaultKeybindsForTest exposes the per-platform default keybind
// maps so tests can validate bindings for BOTH platforms regardless
// of the OS the test runs on. plat is "darwin" or anything else
// (Linux).
func DefaultKeybindsForTest(plat string) map[string]string {
	if plat == "darwin" {
		return defaultKeybindsDarwin()
	}
	return defaultKeybindsLinux()
}
