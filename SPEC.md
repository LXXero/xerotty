# xerotty — Terminal Emulator Specification

Version 1.0 — 2026-03-25

## 1. Project Overview

**xerotty** is a customizable terminal emulator for Linux, built in Go with SDL2 and Dear ImGui (via cimgui-go). The core motivation is a fully configurable right-click context menu and a UI that respects mouse users — something most terminal emulators get wrong by either not having context menus or making them unconfigurable.

xerotty is a companion to [SevenTTY](https://github.com/LXXero/SevenTTY), an SSH client and terminal emulator for classic Mac OS 7/8/9. Where SevenTTY brings modern SSH to vintage Macs, xerotty brings the user-centric design philosophy of classic Mac OS to modern Linux terminals.

### Design Principles

1. **Mouse users are first-class citizens.** Right-click menus, scrollbars, and tab interactions should all be intuitive and configurable.
2. **Everything is configurable via TOML.** No recompiling, no hidden defaults you can't override.
3. **Worse is better.** Simplicity over feature bloat. Get the core right before adding frills.
4. **No Electron. No web tech.** SDL2 + Dear ImGui for a native, fast, low-overhead UI.

---

## 2. Technology Stack

| Component | Library | Purpose |
|-----------|---------|---------|
| Language | Go 1.22+ | Main application language |
| PTY | [creack/pty](https://github.com/creack/pty) | Pseudoterminal allocation and management |
| Terminal emulation | [charmbracelet/x/vt](https://github.com/charmbracelet/x) | VT100/VT220/xterm escape sequence parsing via `SafeEmulator` |
| GUI framework | [AllenDang/cimgui-go](https://github.com/AllenDang/cimgui-go) | Go bindings for Dear ImGui |
| GUI backend | SDL2 + OpenGL 3.x | Window management, input, GPU rendering |
| Config format | [BurntSushi/toml](https://github.com/BurntSushi/toml) | Configuration file parsing |
| Config path | `~/.config/xerotty/config.toml` | User configuration |
| Theme path | `~/.config/xerotty/themes/` | User theme files |

### Why These Choices

- **charmbracelet/x/vt `SafeEmulator`**: Thread-safe wrapper around the terminal emulator. The PTY reader goroutine writes data into the emulator concurrently with the main thread reading cells for rendering. Using the non-safe `Emulator` would cause data races. SafeEmulator serializes access internally.
- **cimgui-go with SDL2**: Dear ImGui provides immediate-mode rendering with minimal boilerplate. SDL2 handles cross-platform windowing. The SDL2+OpenGL backend gives us hardware-accelerated rendering while ImGui handles all the UI widgets (tabs, menus, popups, overlays).
- **creack/pty**: The standard Go library for PTY allocation. Handles the `forkpty`/`openpty` dance and provides a clean `io.ReadWriteCloser` interface.

---

## 3. Package Layout

```
xerotty/
├── cmd/xerotty/
│   └── main.go               # Entry point: parse flags, load config, create App, run
├── internal/
│   ├── app/
│   │   └── app.go            # App struct, SDL2/ImGui backend init, main loop, keybind dispatch
│   ├── config/
│   │   └── config.go         # Config types, TOML parsing, defaults, validation
│   ├── terminal/
│   │   ├── terminal.go       # Terminal struct: SafeEmulator + PTY + goroutines
│   │   └── pty.go            # Shell detection, PTY spawn, env setup
│   ├── renderer/
│   │   ├── renderer.go       # Cell grid → ImDrawList rendering (bg runs, fg glyphs, cursor)
│   │   ├── font.go           # Font loading, cell metrics, glyph range configuration
│   │   └── colors.go         # ansi.Color → ImU32 conversion, theme color resolution
│   ├── tabs/
│   │   └── tabs.go           # TabManager: create/close/switch/reorder tabs
│   ├── menu/
│   │   └── menu.go           # Config-driven context menu builder, action registry
│   ├── input/
│   │   ├── input.go          # SDL key → VT key mapping, modifier translation
│   │   └── clipboard.go      # PRIMARY + CLIPBOARD management, unsafe paste detection
│   ├── themes/
│   │   └── themes.go         # Theme loading/parsing, iTerm2 import, color resolution
│   └── scrollback/
│       └── scrollback.go     # Scrollback buffer, search, disk swap for unlimited mode
├── tools/
│   └── iterm2-import.go      # CLI tool: .itermcolors → xerotty TOML theme converter
├── themes/                    # Bundled theme TOML files
│   ├── dracula.toml
│   ├── solarized-dark.toml
│   ├── solarized-light.toml
│   ├── gruvbox-dark.toml
│   └── monokai.toml
├── testdata/                  # ANSI art test files, VT escape sequence test scripts
│   ├── ansi/                  # .ans files for visual testing
│   └── scripts/               # Shell scripts that exercise escape sequences
├── go.mod
├── go.sum
├── SPEC.md
├── CLAUDE.md
└── README.md
```

### Package Responsibilities

| Package | Owns | Depends On |
|---------|------|------------|
| `cmd/xerotty` | CLI flags, startup | `app`, `config` |
| `app` | Main loop, SDL2/ImGui lifecycle, keybind dispatch | `config`, `terminal`, `renderer`, `tabs`, `menu`, `input`, `themes`, `scrollback` |
| `config` | TOML parsing, default values, validation | (none — leaf package) |
| `terminal` | SafeEmulator, PTY lifecycle, goroutines | `config` |
| `renderer` | Cell-to-pixel rendering, font atlas | `config`, `themes` |
| `tabs` | Tab state, tab bar rendering | `terminal`, `config` |
| `menu` | Context menu rendering, action dispatch | `config` |
| `input` | Key translation, clipboard ops | `config` |
| `themes` | Theme file parsing, color resolution | `config` |
| `scrollback` | Buffer management, search, disk swap | `config` |

---

## 4. Architecture

### 4.1 Threading Model

```
┌─────────────────────────────────────────────────────────┐
│                    Main Thread (OS thread)               │
│  runtime.LockOSThread() — required for SDL2/OpenGL      │
│                                                          │
│  ┌─────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐ │
│  │ SDL2    │  │ ImGui    │  │ Render   │  │ Context  │ │
│  │ Events  │→ │ Frame    │→ │ Cells    │→ │ Menu     │ │
│  └─────────┘  └──────────┘  └──────────┘  └──────────┘ │
│       ↕                           ↑                      │
│  Input dispatch            SafeEmulator.Read()           │
│       ↓                                                  │
│  SafeEmulator / PTY write                                │
└─────────────────────────────────────────────────────────┘

Per Terminal (one per tab):
┌──────────────────────────┐    ┌──────────────────────────┐
│   PTY Reader Goroutine   │    │  Emulator Reader Gortn   │
│                          │    │                          │
│  for {                   │    │  for {                   │
│    n = pty.Read(buf)     │    │    n = emu.Read(buf)     │
│    emu.Write(buf[:n])    │    │    pty.Write(buf[:n])    │
│    notify main thread    │    │  }                       │
│  }                       │    │  (device responses:      │
│                          │    │   DA, DSR, cursor pos)   │
└──────────────────────────┘    └──────────────────────────┘
```

**Per-terminal goroutines (2 per tab):**

1. **PTY Reader**: Reads bytes from the PTY file descriptor. Writes them into `SafeEmulator.Write()`. Sends a notification to the main thread (via channel) that new data is available for rendering. This goroutine is the producer of terminal content.

2. **Emulator Reader**: Reads bytes from `SafeEmulator.Read()` — these are device responses generated by the terminal emulator (e.g., cursor position reports, device attributes). Writes them back to the PTY so the shell/application receives the responses. This goroutine handles the "backwards" data flow that most people forget about.

**Main thread responsibilities:**
- `runtime.LockOSThread()` at startup (SDL2 and OpenGL require main thread)
- Poll SDL2 events (keyboard, mouse, window resize, quit)
- Dispatch keybinds to actions
- Drive ImGui frame: begin frame → render tab bar → render active terminal cell grid → render context menu → end frame → SDL2 swap
- Read cell data from `SafeEmulator` for rendering (safe because SafeEmulator serializes)

**Data flow for a keystroke:**
1. SDL2 key event → `input.go` translates to VT escape sequence
2. Sequence written to PTY fd (e.g., `\x1b[B` for down arrow)
3. Shell/program processes it, writes output back to PTY
4. PTY Reader goroutine reads output, feeds to `SafeEmulator.Write()`
5. SafeEmulator parses escape sequences, updates internal cell grid
6. Next render frame reads cells from SafeEmulator, draws to screen

### 4.2 Window Model

Each OS window is a separate process. When the user requests "New Window" (via menu or keybind):

```go
exe, _ := os.Executable()
cmd := exec.Command(exe)
cmd.Start()
// No shared state — each process has its own config, tabs, terminals
```

This keeps the architecture simple: no IPC, no shared memory, no window manager within the application. Each process owns exactly one SDL2 window with one ImGui context.

### 4.3 Terminal Lifecycle

```
NewTerminal(config)
  │
  ├── Detect shell (config override > $SHELL > /bin/sh)
  ├── pty.Start(shell) → PTY fd + child process
  ├── Set TERM=xterm-256color (or config override)
  ├── Apply env overrides from config
  ├── Create SafeEmulator(cols, rows)
  ├── Start PTY Reader goroutine
  ├── Start Emulator Reader goroutine
  └── Return Terminal struct
        │
        ├── Write([]byte) → PTY fd (for keyboard input)
        ├── Resize(cols, rows) → pty.Setsize() + SafeEmulator.Resize()
        ├── Cells() → read cell grid from SafeEmulator
        └── Close() → kill child, close PTY, stop goroutines
```

### 4.4 Notification Channel

The PTY Reader goroutine must notify the main thread that new data arrived (so it can re-render). This uses a non-blocking channel send:

```go
// In Terminal struct
dataCh chan struct{} // buffered, capacity 1

// PTY Reader goroutine, after emu.Write():
select {
case t.dataCh <- struct{}{}:
default: // already notified, don't block
}

// Main loop, before rendering:
// Drain all pending notifications
for _, tab := range tabs {
    select {
    case <-tab.Terminal.dataCh:
        tab.dirty = true
    default:
    }
}
```

---

## 5. Rendering Pipeline

### 5.1 Cell Grid Rendering

The renderer converts the SafeEmulator's cell grid into ImGui draw commands using `ImDrawList`. This is NOT ImGui widgets — it is direct GPU-accelerated drawing for maximum performance.

```
For each visible row (top to bottom):
  1. BACKGROUNDS: Run-length encode consecutive cells with same bg color.
     For each run: AddRectFilled(x, y, x+run_len*cell_w, y+cell_h, bg_color)

  2. FOREGROUND TEXT: For each cell with content:
     AddText(font, font_size, pos, fg_color, glyph_string)
     (ImGui internally batches these into one GPU draw call per font texture)

  3. DECORATIONS: For cells with underline, strikethrough, etc.:
     AddLine() for underline/strikethrough
```

**Why run-length encoding for backgrounds?** A typical terminal row has long runs of the same background color (e.g., an entire line of text on the default background). Instead of one `AddRectFilled()` per cell (80+ calls per row), RLE reduces it to 1-5 calls per row. This is a significant performance win.

### 5.2 Cell Metrics

```go
type CellMetrics struct {
    Width  float32 // single cell width in pixels (from font advance of 'M')
    Height float32 // cell height in pixels (font ascent + descent + line gap)
}
```

Cell metrics are computed once when the font is loaded. All rendering math uses these values. The terminal grid dimensions (cols × rows) are derived from `(window_width - scrollbar_width) / cell_width` and `(window_height - tab_bar_height) / cell_height`.

### 5.3 Wide Characters (CJK)

Characters with `Width == 2` (CJK ideographs, some emoji) span two cell widths:
- Render the glyph at the first cell position with width = `2 * cell_width`
- Skip the continuation cell (second cell of the pair) during rendering
- The SafeEmulator handles the internal bookkeeping of which cells are continuations

### 5.4 Cursor Rendering

```go
type CursorStyle int
const (
    CursorBlock     CursorStyle = iota // filled rectangle
    CursorUnderline                     // horizontal line at cell bottom
    CursorBar                           // vertical line at cell left
)
```

- **Block**: `AddRectFilled()` with cursor color, then render the character underneath in the background color (inverted)
- **Underline**: `AddRectFilled()` with cursor color, height = 2px, at bottom of cell
- **Bar**: `AddRectFilled()` with cursor color, width = 2px, at left of cell

**Blink**: Toggle visibility on a timer (default 530ms on, 530ms off). Configurable rate. Can be disabled.

### 5.5 Font Loading

```go
// Font configuration
type FontConfig struct {
    Family string  // e.g., "JetBrains Mono", "Fira Code"
    Size   float32 // in points
    Path   string  // explicit path override (e.g., "/usr/share/fonts/TTF/JetBrainsMono-Regular.ttf")
}
```

Font loading sequence:
1. If `font.path` is set in config, load that TTF/OTF file directly
2. Otherwise, search system font directories for `font.family`
3. Fallback: embedded default font (built into the binary, e.g., a liberation mono or similar)

**Glyph ranges loaded into ImGui font atlas:**
- ASCII (U+0020-U+007E)
- Latin-1 Supplement (U+00A0-U+00FF)
- Box Drawing (U+2500-U+257F)
- Block Elements (U+2580-U+259F)
- Braille Patterns (U+2800-U+28FF)
- Geometric Shapes (U+25A0-U+25FF)
- Powerline symbols (U+E0A0-U+E0D4) — optional, for shell prompt glyphs
- CJK ranges — optional, loaded only if configured (large atlas)

### 5.6 Color Conversion

The terminal emulator produces colors as ANSI indices (0-255) or 24-bit RGB. These must be converted to ImGui's `ImU32` (packed ABGR).

```go
// Convert terminal color to ImU32 for rendering
func (c *ColorConverter) Resolve(color vt.Color, isDefault bool, isFg bool) uint32 {
    if isDefault {
        if isFg { return c.theme.Foreground }
        return c.theme.Background
    }
    if color.IsIndexed() {
        idx := color.Index()
        if idx < 16 {
            return c.theme.ANSI[idx] // theme palette
        }
        return xterm256ToImU32(idx)   // 6x6x6 cube + grayscale ramp
    }
    // 24-bit RGB
    r, g, b := color.RGB()
    return imgui.ColorU32(r, g, b, 255)
}
```

---

## 6. Feature Specifications

### 6.1 Tab Support

#### Overview
Tabs are rendered as an ImGui tab bar at the top of the window. Each tab owns a complete, independent Terminal instance (SafeEmulator + PTY + 2 goroutines).

#### Data Model

```go
type Tab struct {
    ID       int
    Title    string       // from OSC 0/2, or user override
    Terminal *Terminal     // owns SafeEmulator + PTY + goroutines
    Dirty    bool         // new data since last render
    Closed   bool         // child process exited
}

type TabManager struct {
    Tabs      []*Tab
    ActiveIdx int
    NextID    int          // monotonically increasing tab ID
}
```

#### Tab Title
- Default: shell name (e.g., "bash", "zsh")
- Updated by OSC escape sequences:
  - `ESC ] 0 ; <title> BEL` — set window title (used as tab title)
  - `ESC ] 2 ; <title> BEL` — set window title
- User can override via context menu action `rename_tab` (shows an ImGui text input popup)
- Truncated with ellipsis if longer than tab width

#### Tab Bar Rendering
- Uses `imgui.BeginTabBar()` / `imgui.BeginTabItem()` / `imgui.EndTabItem()` / `imgui.EndTabBar()`
- Tab bar flags: `ImGuiTabBarFlags_Reorderable | ImGuiTabBarFlags_AutoSelectNewTabs | ImGuiTabBarFlags_TabListPopupButton`
- Close button on each tab (the small X): `ImGuiTabItemFlags_None` with `p_open` parameter
- Tab bar colors follow the active theme (see Section 6.4)

#### Tab Close Behavior
- Clicking the X button on a tab → close that tab
- If only one tab remains and it's closed → close the window (exit the process)
- When a tab's child process exits (detected by PTY read returning EOF), mark the tab as closed. Behavior is configurable:
  - `on_child_exit = "close"` — automatically close the tab (default)
  - `on_child_exit = "hold"` — keep the tab open with a "[Process exited]" message
  - `on_child_exit = "hold_on_error"` — hold only if exit code != 0

#### Default Keybinds

| Action | Default Keybind | Configurable |
|--------|----------------|--------------|
| New tab | `Ctrl+Shift+T` | Yes |
| Close tab | `Ctrl+Shift+W` | Yes |
| Next tab | `Ctrl+Tab` | Yes |
| Previous tab | `Ctrl+Shift+Tab` | Yes |
| Go to tab N | `Alt+1` through `Alt+9` | Yes |
| Rename tab | (none) | Yes |

---

### 6.2 Configurable Right-Click Context Menu

#### Overview
This is THE core feature. The right-click context menu is 100% defined in `config.toml`. Every item, its order, its action, and its enabled/disabled conditions are user-configurable.

#### Config Schema

```toml
[[menu.items]]
label = "New Tab"
action = "new_tab"
shortcut = "Ctrl+Shift+T"    # display-only hint, not a keybind definition

[[menu.items]]
label = "New Window"
action = "new_window"
shortcut = "Ctrl+Shift+N"

[[menu.items]]
action = "separator"

[[menu.items]]
label = "Copy"
action = "copy"
shortcut = "Ctrl+Shift+C"
enabled = "has_selection"     # only shown when text is selected

[[menu.items]]
label = "Paste"
action = "paste"
shortcut = "Ctrl+Shift+V"

[[menu.items]]
label = "Paste Selection"
action = "paste_selection"
shortcut = "Shift+Insert"

[[menu.items]]
action = "separator"

[[menu.items]]
label = "Open Link"
action = "open_link"
enabled = "has_link"          # only shown when right-clicking a URL

[[menu.items]]
label = "Copy Link"
action = "copy_link"
enabled = "has_link"

[[menu.items]]
action = "separator"

[[menu.items]]
label = "Search..."
action = "search"
shortcut = "Ctrl+Shift+F"

[[menu.items]]
label = "Fullscreen"
action = "fullscreen"
shortcut = "F11"

[[menu.items]]
action = "separator"

# Submenu example
[[menu.items]]
label = "Themes"
[[menu.items.submenu]]
label = "Dracula"
action = "set_theme:dracula"
[[menu.items.submenu]]
label = "Solarized Dark"
action = "set_theme:solarized-dark"
[[menu.items.submenu]]
label = "Gruvbox"
action = "set_theme:gruvbox-dark"

[[menu.items]]
action = "separator"

# Shell hook: run arbitrary command
[[menu.items]]
label = "Open File Manager Here"
action = "exec:xdg-open ."

[[menu.items]]
label = "Tmux Cheatsheet"
action = "exec:less ~/.config/xerotty/tmux-cheatsheet.txt"

[[menu.items]]
action = "separator"

[[menu.items]]
label = "Close Tab"
action = "close_tab"
shortcut = "Ctrl+Shift+W"
```

#### Built-in Actions

| Action | Description |
|--------|-------------|
| `new_tab` | Open a new tab with the default shell |
| `new_window` | Spawn a new xerotty process |
| `copy` | Copy selected text to CLIPBOARD |
| `paste` | Paste from CLIPBOARD |
| `paste_selection` | Paste from PRIMARY selection |
| `close_tab` | Close the active tab |
| `rename_tab` | Show rename popup for active tab |
| `search` | Open search overlay |
| `fullscreen` | Toggle fullscreen |
| `open_link` | Open the URL under the cursor with xdg-open |
| `copy_link` | Copy the URL under the cursor to CLIPBOARD |
| `select_all` | Select all visible text |
| `clear_scrollback` | Clear the scrollback buffer |
| `reset_terminal` | Send reset sequence to the terminal |
| `set_theme:<name>` | Switch to a named theme at runtime |
| `separator` | Render a visual separator line |
| `exec:<command>` | Run an arbitrary shell command (see below) |

#### Shell Hook Actions (`exec:`)

The `exec:` prefix runs a shell command. The command is executed via `/bin/sh -c "<command>"`. Available environment variable substitutions:

| Variable | Value |
|----------|-------|
| `$XEROTTY_SELECTION` | Currently selected text |
| `$XEROTTY_LINK` | URL under cursor (if any) |
| `$XEROTTY_CWD` | Working directory of the shell in the active tab (via `/proc/<pid>/cwd`) |
| `$XEROTTY_TAB_TITLE` | Title of the active tab |

Example: a "Search Google" menu item:
```toml
[[menu.items]]
label = "Search Google"
action = "exec:xdg-open 'https://www.google.com/search?q=$XEROTTY_SELECTION'"
enabled = "has_selection"
```

#### Enabled Conditions

| Condition | True When |
|-----------|-----------|
| `has_selection` | Text is currently selected |
| `has_link` | Right-click position is over a detected URL |
| `always` | Always enabled (default if omitted) |
| `in_tmux` | Running inside a tmux session (`$TMUX` is set) |

Items with a false `enabled` condition are **hidden** (not grayed out) from the menu.

#### Rendering
- Use `imgui.BeginPopupContextWindow()` for the right-click popup
- Iterate config menu items in order
- For each item: check `enabled` condition → if false, skip
- If `action == "separator"`: `imgui.Separator()`
- If has `submenu`: `imgui.BeginMenu(label)` → render submenu items → `imgui.EndMenu()`
- Otherwise: `imgui.MenuItemEx(label, shortcut, false, true)` → if clicked, dispatch action

---

### 6.3 Configurable Keymap, Keybinds, and Shortcuts

#### Overview
All keybinds are defined in `[keybinds]` in config.toml. They use the same action system as menus.

#### Config Schema

```toml
[keybinds]
# Tab management
"Ctrl+Shift+T"       = "new_tab"
"Ctrl+Shift+W"       = "close_tab"
"Ctrl+Shift+N"       = "new_window"
"Ctrl+Tab"           = "next_tab"
"Ctrl+Shift+Tab"     = "prev_tab"
"Alt+1"              = "goto_tab:1"
"Alt+2"              = "goto_tab:2"
"Alt+3"              = "goto_tab:3"
"Alt+4"              = "goto_tab:4"
"Alt+5"              = "goto_tab:5"
"Alt+6"              = "goto_tab:6"
"Alt+7"              = "goto_tab:7"
"Alt+8"              = "goto_tab:8"
"Alt+9"              = "goto_tab:9"

# Clipboard
"Ctrl+Shift+C"       = "copy"
"Ctrl+Shift+V"       = "paste"
"Shift+Insert"        = "paste_selection"

# Scrollback
"Shift+PageUp"        = "scroll_page_up"
"Shift+PageDown"      = "scroll_page_down"
"Ctrl+Shift+F"        = "search"

# Window
"F11"                 = "fullscreen"
"Ctrl+Shift+Plus"     = "font_size_up"
"Ctrl+Shift+Minus"    = "font_size_down"
"Ctrl+Shift+0"        = "font_size_reset"
```

#### Key Notation

Keys are described as modifier combos using `+` as separator:
- Modifiers: `Ctrl`, `Shift`, `Alt`, `Super`
- Named keys: `Tab`, `Enter`, `Escape`, `Space`, `Backspace`, `Delete`, `Home`, `End`, `PageUp`, `PageDown`, `Insert`, `F1`-`F12`, `Up`, `Down`, `Left`, `Right`, `Plus`, `Minus`
- Letter keys: `A`-`Z` (case-insensitive)
- Number keys: `0`-`9`

#### Key Translation Pipeline

```
SDL_Event (SDL_KEYDOWN)
    │
    ├── Check keybind map → if match, execute action (consume event)
    │
    └── Not a keybind → translate to VT escape sequence:
        ├── Regular printable char → UTF-8 bytes
        ├── Enter → \r (or \n if configured)
        ├── Shift+Enter → \n (configurable, see Section 6.12)
        ├── Backspace → \x7f (or \x08, configurable)
        ├── Delete → \x1b[3~ (or configurable)
        ├── Arrow keys → \x1b[A/B/C/D (normal) or \x1bOA/B/C/D (app mode)
        ├── Home/End → \x1b[H/\x1b[F (normal) or \x1bOH/\x1bOF (app mode)
        ├── F1-F12 → standard xterm sequences
        ├── Ctrl+letter → ASCII 1-26
        └── Alt+key → \x1b + key sequence (meta-sends-escape)
```

#### Special Key Configuration

```toml
[keys]
backspace = "ascii_del"    # \x7f (default) | "ascii_bs" for \x08
delete = "vt_sequence"     # \x1b[3~ (default) | custom string
shift_enter = "newline"    # \n (default) | custom string
```

---

### 6.4 Theme System

#### Overview
Themes define all visual colors for the terminal and its UI chrome. Themes are TOML files that can be bundled, user-installed, or imported from iTerm2 format.

#### Theme File Format

Each theme is a TOML file in `~/.config/xerotty/themes/` or the bundled `themes/` directory.

```toml
# ~/.config/xerotty/themes/dracula.toml
[theme]
name = "Dracula"

[theme.colors]
foreground = "#F8F8F2"
background = "#282A36"
cursor = "#F8F8F2"
selection_fg = "#F8F8F2"
selection_bg = "#44475A"

# Bold text color (optional — if unset, bold uses bright ANSI variant)
bold = "#F8F8F2"

# ANSI 16-color palette
[theme.colors.ansi]
black          = "#21222C"
red            = "#FF5555"
green          = "#50FA7B"
yellow         = "#F1FA8C"
blue           = "#BD93F9"
magenta        = "#FF79C6"
cyan           = "#8BE9FD"
white          = "#F8F8F2"
bright_black   = "#6272A4"
bright_red     = "#FF6E6E"
bright_green   = "#69FF94"
bright_yellow  = "#FFFFA5"
bright_blue    = "#D6ACFF"
bright_magenta = "#FF92DF"
bright_cyan    = "#A4FFFF"
bright_white   = "#FFFFFF"

# UI chrome colors (optional — falls back to sensible defaults from palette)
[theme.ui]
tab_bar_bg = "#1E1F29"
tab_active_bg = "#282A36"
tab_active_fg = "#F8F8F2"
tab_inactive_bg = "#21222C"
tab_inactive_fg = "#6272A4"
scrollbar_bg = "#282A36"
scrollbar_thumb = "#44475A"
scrollbar_thumb_hover = "#6272A4"
```

#### Theme Resolution Order

When determining the color for any element:

1. **Explicit override in `config.toml`** (highest priority)
2. **Theme file value** (if a theme is active)
3. **System theme detection** (GTK/Qt — for UI chrome colors only)
4. **Built-in defaults** (lowest priority)

```toml
# config.toml — override specific theme values
[appearance]
theme = "dracula"

# Override just the scrollbar colors from the theme
scrollbar_bg = "#000000"
scrollbar_thumb = "#333333"

# UI chrome can follow system theme instead of xerotty theme
tab_colors = "system"  # "theme" | "system" | "custom"
# If "custom", use:
# tab_bar_bg = "#..."
# tab_active_bg = "#..."
# etc.
```

#### Bold as Bright Colors

```toml
[appearance]
bold_is_bright = true   # bold text uses bright ANSI variant (default: true)
                        # false = bold text uses the same color, just bold weight
```

When `bold_is_bright = true`:
- ANSI color 0 (black) + bold → ANSI color 8 (bright black)
- ANSI color 1 (red) + bold → ANSI color 9 (bright red)
- And so on for 0-7 → 8-15

#### iTerm2 Import Tool

`tools/iterm2-import.go` is a CLI tool that converts `.itermcolors` files (Apple plist format) to xerotty TOML theme format.

```bash
go run tools/iterm2-import.go Dracula.itermcolors > ~/.config/xerotty/themes/dracula.toml
```

Implementation:
- Parse plist XML (use `howett.net/plist` or `encoding/xml`)
- Map iTerm2 keys to xerotty theme keys:
  - `Background Color` → `theme.colors.background`
  - `Foreground Color` → `theme.colors.foreground`
  - `Cursor Color` → `theme.colors.cursor`
  - `Selection Color` → `theme.colors.selection_bg`
  - `Selected Text Color` → `theme.colors.selection_fg`
  - `Ansi 0 Color` through `Ansi 15 Color` → `theme.colors.ansi.*`
- Convert float RGB components (0.0-1.0) to hex strings

#### Bundled Themes

Ship with at least 5 themes:
- Dracula
- Solarized Dark
- Solarized Light
- Gruvbox Dark
- Monokai

---

### 6.5 Scrollback

#### Overview
The scrollback buffer stores lines that have scrolled off the top of the visible terminal area. It supports configurable length, unlimited mode with disk swap, search, and smooth interaction.

#### Config

```toml
[scrollback]
lines = 10000           # number of lines to keep (default: 10000)
                         # -1 = unlimited (disk-backed when memory exceeds threshold)
scroll_on_keystroke = true   # snap to bottom when user types (default: true)
scroll_on_output = false     # snap to bottom on new output (default: false)
```

#### Data Model

```go
type ScrollbackBuffer struct {
    lines     []ScrollbackLine  // ring buffer for bounded mode
    head      int               // write position
    count     int               // number of valid lines
    maxLines  int               // capacity (-1 for unlimited)

    // Unlimited mode: spill to disk
    diskFile  *os.File          // temp file for overflow
    diskLines int               // number of lines on disk
}

type ScrollbackLine struct {
    Cells []ScrollbackCell
}

type ScrollbackCell struct {
    Rune rune
    FG   uint32 // packed color (ANSI index or RGB)
    BG   uint32
    Attrs uint8 // bold, italic, underline, strikethrough, etc.
}
```

#### Unlimited Mode

When `lines = -1`:
- Start with an in-memory ring buffer (default 10,000 lines)
- When memory usage exceeds a threshold (configurable, default 50MB), start spilling oldest lines to a temporary file on disk
- The temp file is created in `$XDG_RUNTIME_DIR` or `/tmp`
- Lines are serialized as gob-encoded structs (simple, fast)
- Deleted on terminal close or process exit

#### Scrolling Behavior

| Input | Action |
|-------|--------|
| Mouse wheel up | Scroll up 3 lines (configurable) |
| Mouse wheel down | Scroll down 3 lines |
| Shift+PageUp | Scroll up one page (visible rows - 1) |
| Shift+PageDown | Scroll down one page |
| Shift+Home | Scroll to top of scrollback |
| Shift+End | Scroll to bottom (live terminal) |
| Any keystroke (if `scroll_on_keystroke`) | Snap to bottom |
| New output (if `scroll_on_output`) | Snap to bottom |

#### Scroll Position Tracking

```go
type ScrollState struct {
    Offset    int  // 0 = live terminal (bottom), positive = scrolled up N lines
    MaxOffset int  // total scrollback lines available
}
```

When `Offset > 0`, the renderer shows scrollback lines above the live terminal. The terminal continues to receive output in the background — new lines push into scrollback and increment `MaxOffset`.

#### Scrollback Search

Activated by `Ctrl+Shift+F` (configurable). Renders as an ImGui overlay at the top of the terminal area.

```
┌─ Search ──────────────────────────────────────────────┐
│ [search text input____] [↑ Prev] [↓ Next] [X Close]  │
│ 3 of 47 matches                                       │
└───────────────────────────────────────────────────────┘
```

- Real-time incremental search as user types
- Highlights ALL matches in the visible area with a distinct background color
- The "current" match has a brighter/different highlight
- Up/Down arrows (or Prev/Next buttons) cycle through matches
- Escape or the X button closes the search overlay
- Search wraps around

---

### 6.6 Scrollbar

#### Config

```toml
[scrollbar]
visible = "auto"         # "always" | "never" | "auto"
                          # auto: visible when scrollback has content AND user has scrolled
width = 12                # scrollbar width in pixels (default: 12)
min_thumb_height = 20     # minimum thumb size in pixels
```

#### Behavior

- **always**: Scrollbar is always rendered on the right side of the terminal area
- **never**: No scrollbar (scrolling still works via keyboard/mouse wheel)
- **auto**: Scrollbar appears when the user scrolls up from the bottom, disappears when they return to the bottom (with a short fade-out animation, ~300ms)

#### Rendering

The scrollbar is drawn directly via ImDrawList (not an ImGui widget) to maintain full color control:

```
┌─────────────────────────┬──┐
│                         │▲ │  ← optional arrow (auto-hidden in auto mode)
│  Terminal content       │░░│  ← track (scrollbar_bg color)
│                         │██│  ← thumb (scrollbar_thumb color)
│                         │░░│
│                         │▼ │
└─────────────────────────┴──┘
```

- **Track**: Full height of terminal area, width from config
- **Thumb**: Proportional size based on `visible_rows / total_rows`. Position based on scroll offset
- **Interaction**: Click-drag thumb to scroll. Click track above/below thumb to page up/down
- **Hover**: Thumb color changes to `scrollbar_thumb_hover` on mouse hover
- Colors come from theme (see Section 6.4)

---

### 6.7 Resize Overlay

#### Overview
When the terminal window is being resized, display a temporary centered overlay showing the current terminal dimensions.

#### Config

```toml
[appearance]
resize_overlay = true          # enable/disable (default: true)
resize_overlay_duration = 1.0  # seconds to show after resize stops (default: 1.0)
```

#### Behavior

1. On `SDL_WINDOWEVENT_RESIZED`, calculate new cols × rows
2. Show overlay: centered text displaying `"80 × 24"` (cols × rows)
3. Overlay is a semi-transparent rounded rectangle with white text
4. Start/reset a fade timer
5. After `resize_overlay_duration` seconds with no further resize events, fade out the overlay over 300ms
6. During the fade, alpha decreases linearly from 1.0 to 0.0

#### Rendering

```go
// Overlay rectangle
drawList.AddRectFilled(overlayRect, imgui.ColorU32(0, 0, 0, 180), 8.0) // rounded corners
// Text
text := fmt.Sprintf("%d × %d", cols, rows)
textSize := imgui.CalcTextSize(text)
pos := imgui.Vec2{
    X: (windowWidth - textSize.X) / 2,
    Y: (windowHeight - textSize.Y) / 2,
}
drawList.AddText(pos, imgui.ColorU32(255, 255, 255, 255), text)
```

---

### 6.8 Clipboard

#### Overview
Linux has two separate clipboard mechanisms. xerotty supports both and makes them fully configurable.

| Clipboard | How It Works | Default Behavior in xerotty |
|-----------|-------------|---------------------------|
| PRIMARY | Highlight text → automatically copied. Middle-click → paste. | Copy on select |
| CLIPBOARD | Explicit Ctrl+C → copy. Ctrl+V → paste. | Ctrl+Shift+C / Ctrl+Shift+V |

#### Config

```toml
[clipboard]
copy_on_select = true          # auto-copy selection to PRIMARY (default: true)
paste_on_middle_click = true   # middle-click pastes PRIMARY (default: true)
trim_trailing_whitespace = true # trim trailing spaces from copied lines (default: true)

[clipboard.unsafe_paste]
enabled = true                 # show warning dialog for suspicious paste content (default: true)
multiline_warning = true       # warn on multi-line paste (default: true)
patterns = [                   # additional regex patterns that trigger a warning
    "sudo\\s",
    "rm\\s+(-rf?|--recursive)",
    "chmod\\s+777",
    "curl.*\\|.*sh",
    "wget.*\\|.*sh",
    "dd\\s+if=",
    "> /dev/sd",
]
newline_guard = true           # warn if paste content ends with a newline (auto-executes) (default: true)
```

#### Unsafe Paste Dialog

When triggered, show an ImGui modal dialog:

```
┌─ Unsafe Paste Warning ──────────────────────────────────┐
│                                                          │
│  ⚠ The clipboard content may be dangerous:              │
│                                                          │
│  ┌────────────────────────────────────────────────────┐  │
│  │ sudo rm -rf /important/data                        │  │
│  │ echo "done"                                        │  │
│  └────────────────────────────────────────────────────┘  │
│                                                          │
│  Reason: Contains 'sudo' and multiple lines             │
│                                                          │
│  [Paste Anyway]                          [Cancel]        │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

- Shows a preview of the paste content (truncated if very long)
- Lists the reason(s) the warning was triggered
- "Paste Anyway" sends the content to the PTY
- "Cancel" discards
- Keyboard: Enter = Cancel (safe default), Ctrl+Enter = Paste Anyway

#### SDL2 Clipboard API

- `SDL_GetClipboardText()` for CLIPBOARD
- For PRIMARY: Use X11 selection via `SDL_X11_GetPrimarySelectionText()` (SDL2 ≥ 2.26) or fall back to xclip/xsel subprocess

---

### 6.9 Shell Configuration

#### Config

```toml
[shell]
command = ""                  # override $SHELL (default: "" = use $SHELL or /bin/sh)
login_shell = true            # prepend "-" to argv[0] (default: true)
initial_command = ""          # command to run on tab open (e.g., "tmux attach || tmux")
args = []                     # additional shell arguments

[shell.env]
TERM = "xterm-256color"       # default terminal type
COLORTERM = "truecolor"       # advertise true color support
# Additional env vars:
# FOO = "bar"
```

#### Shell Detection

```go
func detectShell(config *Config) string {
    if config.Shell.Command != "" {
        return config.Shell.Command
    }
    if shell := os.Getenv("SHELL"); shell != "" {
        return shell
    }
    return "/bin/sh"
}
```

#### Login Shell

When `login_shell = true`, the shell's `argv[0]` is prefixed with `-`:
```go
// For /bin/bash with login_shell = true:
// argv[0] = "-bash" (not "/bin/bash")
// This causes bash to read /etc/profile, ~/.bash_profile, etc.
```

#### Initial Command

If `initial_command` is set, it is written to the PTY after the shell starts (with a short delay to let the shell initialize):

```go
if config.Shell.InitialCommand != "" {
    time.AfterFunc(100*time.Millisecond, func() {
        terminal.Write([]byte(config.Shell.InitialCommand + "\n"))
    })
}
```

#### Environment Variables

The PTY child process inherits the parent's environment, with overrides from `[shell.env]`:

```go
env := os.Environ()
for k, v := range config.Shell.Env {
    env = append(env, fmt.Sprintf("%s=%s", k, v))
}
cmd.Env = env
```

---

### 6.10 Link Detection and Interaction

#### Overview
URLs in terminal output are detected, visually highlighted on hover, and can be opened via right-click menu or keyboard/mouse shortcuts.

#### URL Detection

Regex pattern for URL matching (applied per-line when content changes):

```go
var urlRegex = regexp.MustCompile(
    `https?://[^\s<>"{}|\\^` + "`" + `\[\]]+` +
    `|` +
    `www\.[^\s<>"{}|\\^` + "`" + `\[\]]+`)
```

Detected URLs are stored per-line as `{startCol, endCol, url}` tuples. These are recalculated when a line changes (dirty tracking).

#### Visual Feedback

- When mouse hovers over a detected URL, the URL text is underlined (via `AddLine()` calls below each character)
- Cursor changes to a pointing hand (via `SDL_SetCursor(SDL_CreateSystemCursor(SDL_SYSTEM_CURSOR_HAND))`)
- On mouse-out, underline disappears and cursor reverts

#### Interaction

| Input | Action | Config Key |
|-------|--------|------------|
| Right-click on URL | "Open Link" / "Copy Link" appear in context menu | Always on (context-aware) |
| Ctrl+Click on URL | Open URL | `link.ctrl_click = true` (default) |
| Double-click on URL | Open URL | `link.double_click = false` (default: disabled, double-click is word select) |

#### Config

```toml
[links]
enabled = true                # enable URL detection (default: true)
ctrl_click = true             # Ctrl+click opens links (default: true)
double_click = false          # double-click opens links (default: false)
opener = "xdg-open"           # command to open URLs (default: "xdg-open")
```

#### Opening Links

```go
func openLink(url string, opener string) error {
    cmd := exec.Command(opener, url)
    cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from terminal
    return cmd.Start()
}
```

---

### 6.11 Image Paste Support

#### Overview
Support pasting images from the clipboard into the terminal, primarily for applications that support inline images (e.g., Claude Code, kitty-protocol-aware tools).

#### Protocol

Use the **Kitty graphics protocol** for inline image display:
- Images are base64-encoded and transmitted via escape sequences
- Format: `ESC_G<payload>ESC\\` where payload contains image data and metadata

#### Behavior

1. When paste is triggered and clipboard contains image data:
   - Check if the terminal application advertises kitty graphics protocol support (via device attributes response)
   - If supported: transmit via kitty graphics protocol
   - If not supported: base64-encode the image and paste as text (with a confirmation dialog)
2. Image data is retrieved via SDL2 clipboard API or `xclip -selection clipboard -target image/png -o`

#### Config

```toml
[images]
enabled = true                # enable image paste support (default: true)
max_size_mb = 10              # maximum image size to paste in MB (default: 10)
fallback = "base64"           # "base64" | "ignore" — what to do if app doesn't support images
```

---

### 6.12 Shift+Enter Support

#### Overview
Shift+Enter sends a literal newline character (`\n`, 0x0A) to the terminal without triggering command submission. This is critical for multi-line input in tools like Claude Code, Python REPL, and other applications that distinguish between Enter (submit) and Shift+Enter (newline).

#### Implementation

In the key translation pipeline (`input.go`):

```go
if key == SDL_SCANCODE_RETURN && (mods & KMOD_SHIFT) != 0 {
    // Shift+Enter: send configurable sequence (default: \n)
    terminal.Write([]byte(config.Keys.ShiftEnter))
    return // consume event
}
```

#### Config

```toml
[keys]
shift_enter = "\n"    # what Shift+Enter sends (default: "\n")
                       # Some apps may want "\x1b[13;2u" (CSI u encoding)
```

---

### 6.13 Tmux Helpers (Lower Priority)

#### Overview
Optional quality-of-life features for tmux users. These are not core functionality and should be implemented last.

#### Features

1. **Tmux detection**: Check for `$TMUX` environment variable. When running inside tmux, adjust behavior:
   - Don't intercept the tmux prefix key (default `Ctrl+B`)
   - Optionally pass through certain keybinds to tmux instead of handling locally

2. **Tmux cheatsheet overlay**: A built-in action (`tmux_cheatsheet`) that shows an ImGui overlay with common tmux keybinds. This can be bound to a menu item or keybind.

3. **Tmux menu items**: The right-click menu can include tmux-specific items via `exec:` actions:

```toml
[[menu.items]]
label = "Tmux"
enabled = "in_tmux"
[[menu.items.submenu]]
label = "New Tmux Window"
action = "exec:tmux new-window"
[[menu.items.submenu]]
label = "Split Horizontal"
action = "exec:tmux split-window -h"
[[menu.items.submenu]]
label = "Split Vertical"
action = "exec:tmux split-window -v"
[[menu.items.submenu]]
label = "Cheatsheet"
action = "tmux_cheatsheet"
```

#### Config

```toml
[tmux]
passthrough_prefix = true    # don't intercept tmux prefix key (default: true)
prefix_key = "Ctrl+B"        # tmux prefix key to avoid intercepting (default: "Ctrl+B")
```

---

### 6.14 ANSI Art / VT Compatibility

#### Overview
Full compatibility with VT100/VT220/xterm escape sequences, powered by `charmbracelet/x/vt`. xerotty must correctly render ANSI art, TUI applications, and all standard terminal features.

#### Supported Features (via charmbracelet/x/vt)

| Category | Features |
|----------|----------|
| Text attributes | Bold, dim, italic, underline, blink, inverse, hidden, strikethrough |
| Colors | 16 ANSI, 256-color (6x6x6 cube + grayscale), 24-bit true color (SGR) |
| Cursor | Movement (CUU/CUD/CUF/CUB), positioning (CUP), save/restore (DECSC/DECRC) |
| Erasing | Clear line (EL), clear screen (ED), selective erase |
| Scrolling | Scroll up (SU), scroll down (SD), scroll regions (DECSTBM) |
| Modes | Application cursor keys (DECCKM), origin mode, wraparound, bracketed paste |
| OSC | Set title (0, 2), set colors (4, 10, 11, 12), hyperlinks (8) |
| Mouse | X10, normal, SGR, button tracking (for tmux, vim, etc.) |
| Alternate screen | smcup/rmcup switching (for vim, less, etc.) |

#### Unicode Support

The renderer must handle these glyph ranges correctly:

| Range | Name | Used By |
|-------|------|---------|
| U+2500-U+257F | Box Drawing | TUI borders (mc, htop, etc.) |
| U+2580-U+259F | Block Elements | Bar charts, progress bars |
| U+2800-U+28FF | Braille Patterns | Graphical plots (gnuplot, spark) |
| U+25A0-U+25FF | Geometric Shapes | Bullets, markers |
| U+E0A0-U+E0D4 | Powerline Private Use | Shell prompts (oh-my-zsh, starship) |

These ranges must be included in the ImGui font atlas glyph range configuration (see Section 5.5).

#### Bracketed Paste Mode

When the application enables bracketed paste mode (`\x1b[?2004h`), all pasted text is wrapped:
- Start: `\x1b[200~`
- End: `\x1b[201~`

This prevents pasted text from being interpreted as commands. xerotty must track whether bracketed paste mode is enabled (the SafeEmulator tracks this) and wrap paste content accordingly.

---

## 7. Complete Config File Schema

The following is the complete default `config.toml` with all options, their defaults, and comments.

```toml
# xerotty configuration
# ~/.config/xerotty/config.toml
#
# All values shown are the defaults.
# Uncomment and modify to customize.

# ── Window ─────────────────────────────────────────────────────────

[window]
# Initial window dimensions in columns and rows (not pixels).
# The actual pixel size is computed from font metrics.
columns = 80
rows = 24

# Window title (overridden by OSC escape sequences from the shell)
title = "xerotty"

# Padding between the terminal content and the window edge, in pixels.
padding = 2

# Start in fullscreen mode
fullscreen = false

# Window opacity (1.0 = fully opaque, requires compositor)
opacity = 1.0

# ── Appearance ─────────────────────────────────────────────────────

[appearance]
# Theme name. Must match a .toml file in ~/.config/xerotty/themes/ or
# the bundled themes/ directory (without extension).
theme = ""   # empty = built-in default dark theme

# Whether bold text uses the bright ANSI color variant (colors 8-15)
bold_is_bright = true

# Cursor style: "block", "underline", or "bar"
cursor_style = "block"

# Cursor blinking
cursor_blink = true
cursor_blink_interval = 0.53   # seconds for each on/off phase

# Show resize overlay with column×row dimensions during window resize
resize_overlay = true
resize_overlay_duration = 1.0  # seconds to display after resize stops

# Tab bar / scrollbar color source: "theme", "system", or "custom"
# "theme"  = use colors from the active theme
# "system" = detect GTK/Qt system theme colors
# "custom" = use the explicit color values below
tab_colors = "theme"
scrollbar_colors = "theme"

# Custom color overrides (only used when *_colors = "custom")
# tab_bar_bg = "#1E1F29"
# tab_active_bg = "#282A36"
# tab_active_fg = "#F8F8F2"
# tab_inactive_bg = "#21222C"
# tab_inactive_fg = "#6272A4"
# scrollbar_bg = "#282A36"
# scrollbar_thumb = "#44475A"
# scrollbar_thumb_hover = "#6272A4"

# ── Font ───────────────────────────────────────────────────────────

[font]
# Font family name (searched in system font directories)
family = "monospace"

# Explicit font file path (overrides family if set)
# path = "/usr/share/fonts/TTF/JetBrainsMono-Regular.ttf"

# Font size in points
size = 14.0

# Additional line spacing in pixels (added to font-derived cell height)
line_spacing = 0

# Glyph ranges to load (in addition to ASCII and Latin-1)
# Available: "box_drawing", "block_elements", "braille", "geometric",
#            "powerline", "cjk_unified"
extra_glyphs = ["box_drawing", "block_elements", "braille", "geometric", "powerline"]

# ── Shell ──────────────────────────────────────────────────────────

[shell]
# Override $SHELL (empty = use $SHELL environment variable, fallback /bin/sh)
command = ""

# Additional arguments passed to the shell
args = []

# Run as login shell (prepend "-" to argv[0])
login_shell = true

# Command to execute in the shell on tab creation
# (written to PTY after shell starts)
initial_command = ""

# Child process exit behavior: "close", "hold", "hold_on_error"
on_child_exit = "close"

# Environment variable overrides for the shell process
[shell.env]
TERM = "xterm-256color"
COLORTERM = "truecolor"

# ── Keys ───────────────────────────────────────────────────────────

[keys]
# What the Backspace key sends: "ascii_del" (\x7f) or "ascii_bs" (\x08)
backspace = "ascii_del"

# What the Delete key sends (VT escape sequence)
delete = "\x1b[3~"

# What Shift+Enter sends
shift_enter = "\n"

# ── Keybinds ───────────────────────────────────────────────────────
# Format: "Modifier+Key" = "action"
# Modifiers: Ctrl, Shift, Alt, Super
# See Section 6.3 for the full action list.

[keybinds]
"Ctrl+Shift+T" = "new_tab"
"Ctrl+Shift+W" = "close_tab"
"Ctrl+Shift+N" = "new_window"
"Ctrl+Tab" = "next_tab"
"Ctrl+Shift+Tab" = "prev_tab"
"Alt+1" = "goto_tab:1"
"Alt+2" = "goto_tab:2"
"Alt+3" = "goto_tab:3"
"Alt+4" = "goto_tab:4"
"Alt+5" = "goto_tab:5"
"Alt+6" = "goto_tab:6"
"Alt+7" = "goto_tab:7"
"Alt+8" = "goto_tab:8"
"Alt+9" = "goto_tab:9"
"Ctrl+Shift+C" = "copy"
"Ctrl+Shift+V" = "paste"
"Shift+Insert" = "paste_selection"
"Shift+PageUp" = "scroll_page_up"
"Shift+PageDown" = "scroll_page_down"
"Shift+Home" = "scroll_to_top"
"Shift+End" = "scroll_to_bottom"
"Ctrl+Shift+F" = "search"
"F11" = "fullscreen"
"Ctrl+Shift+Plus" = "font_size_up"
"Ctrl+Shift+Minus" = "font_size_down"
"Ctrl+Shift+0" = "font_size_reset"

# ── Right-Click Context Menu ───────────────────────────────────────
# Menu items are rendered in the order they appear here.
# Each item has: label, action, shortcut (display hint), enabled condition.
# See Section 6.2 for the full action and condition reference.

[[menu.items]]
label = "New Tab"
action = "new_tab"
shortcut = "Ctrl+Shift+T"

[[menu.items]]
label = "New Window"
action = "new_window"
shortcut = "Ctrl+Shift+N"

[[menu.items]]
action = "separator"

[[menu.items]]
label = "Copy"
action = "copy"
shortcut = "Ctrl+Shift+C"
enabled = "has_selection"

[[menu.items]]
label = "Paste"
action = "paste"
shortcut = "Ctrl+Shift+V"

[[menu.items]]
label = "Paste Selection"
action = "paste_selection"
shortcut = "Shift+Insert"

[[menu.items]]
action = "separator"

[[menu.items]]
label = "Open Link"
action = "open_link"
enabled = "has_link"

[[menu.items]]
label = "Copy Link"
action = "copy_link"
enabled = "has_link"

[[menu.items]]
action = "separator"

[[menu.items]]
label = "Search..."
action = "search"
shortcut = "Ctrl+Shift+F"

[[menu.items]]
label = "Fullscreen"
action = "fullscreen"
shortcut = "F11"

[[menu.items]]
action = "separator"

[[menu.items]]
label = "Close Tab"
action = "close_tab"
shortcut = "Ctrl+Shift+W"

# ── Clipboard ──────────────────────────────────────────────────────

[clipboard]
# Automatically copy selected text to PRIMARY clipboard
copy_on_select = true

# Middle-click pastes from PRIMARY clipboard
paste_on_middle_click = true

# Trim trailing whitespace from each line when copying
trim_trailing_whitespace = true

[clipboard.unsafe_paste]
# Show a confirmation dialog before pasting suspicious content
enabled = true

# Warn when paste contains multiple lines
multiline_warning = true

# Warn when paste content ends with a newline (would auto-execute)
newline_guard = true

# Regex patterns that trigger a paste warning
patterns = [
    "sudo\\s",
    "rm\\s+(-rf?|--recursive)",
    "chmod\\s+777",
    "curl.*\\|.*sh",
    "wget.*\\|.*sh",
    "dd\\s+if=",
    "> /dev/sd",
]

# ── Scrollback ─────────────────────────────────────────────────────

[scrollback]
# Maximum number of lines to keep in scrollback.
# -1 = unlimited (disk-backed when memory is large)
lines = 10000

# Snap to bottom of terminal when user presses a key
scroll_on_keystroke = true

# Snap to bottom of terminal when new output is received
scroll_on_output = false

# Number of lines to scroll per mouse wheel tick
scroll_speed = 3

# ── Scrollbar ──────────────────────────────────────────────────────

[scrollbar]
# Visibility: "always", "never", or "auto"
# auto = shows when scrollback has content and user has scrolled up
visible = "auto"

# Scrollbar width in pixels
width = 12

# Minimum thumb height in pixels (prevents tiny thumb on huge scrollback)
min_thumb_height = 20

# ── Links ──────────────────────────────────────────────────────────

[links]
# Enable URL detection in terminal output
enabled = true

# Ctrl+click on a URL to open it
ctrl_click = true

# Double-click on a URL to open it (conflicts with word selection)
double_click = false

# Command used to open URLs
opener = "xdg-open"

# ── Images ─────────────────────────────────────────────────────────

[images]
# Enable image paste support (kitty graphics protocol)
enabled = true

# Maximum image file size to paste, in megabytes
max_size_mb = 10

# Fallback when terminal app doesn't support kitty graphics:
# "base64" = paste as base64 text, "ignore" = silently skip
fallback = "base64"

# ── Tmux ───────────────────────────────────────────────────────────

[tmux]
# Don't intercept the tmux prefix key when running inside tmux
passthrough_prefix = true

# The tmux prefix key to avoid intercepting
prefix_key = "Ctrl+B"
```

---

## 8. Implementation Phases

### Phase 1 — Core Terminal (Foundation)

**Goal**: A working terminal emulator that can run a shell in a single window. No tabs, no menus, no themes — just a functioning terminal.

**Packages**: `cmd/xerotty`, `internal/app`, `internal/config` (minimal), `internal/terminal`, `internal/renderer`, `internal/input`

**Deliverables**:
1. SDL2 + ImGui window initialization with OpenGL backend
2. `runtime.LockOSThread()` on main thread
3. Font loading (system monospace font) and cell metric calculation
4. ImGui font atlas with ASCII + box drawing + block elements glyph ranges
5. PTY spawn with shell detection (`$SHELL` → `/bin/sh` fallback)
6. `TERM=xterm-256color` environment
7. SafeEmulator creation and sizing
8. PTY Reader goroutine (PTY fd → SafeEmulator.Write)
9. Emulator Reader goroutine (SafeEmulator.Read → PTY fd)
10. Notification channel (PTY reader → main thread)
11. Cell grid rendering via ImDrawList:
    - Background RLE → AddRectFilled
    - Foreground per-glyph → AddText
    - Cursor (block only for now, no blink)
12. SDL key event → VT escape sequence translation (printable chars, arrows, Enter, Backspace, Delete, F-keys, Ctrl+letter, Alt+key)
13. Window resize → PTY resize + SafeEmulator resize + cell recalculation
14. Graceful shutdown (kill child, close PTY, clean up goroutines)

**Acceptance Criteria**:
- `ls`, `vim`, `htop`, `top` all render correctly
- Arrow keys, Home/End, Page Up/Down work in vim and less
- Window resize correctly updates the terminal dimensions
- Shell exits → window closes
- Colors render (16 ANSI at minimum)

---

### Phase 2 — Config, Themes, and Colors

**Goal**: Full color support (256 + true color), theme loading, and a config file that controls appearance.

**Packages**: `internal/config` (full), `internal/themes`, `internal/renderer/colors.go`

**Deliverables**:
1. TOML config file loading from `~/.config/xerotty/config.toml`
2. Default config generation (if file doesn't exist, create with defaults)
3. Config validation (unknown keys → warning, invalid values → fallback to default)
4. Full color pipeline:
   - ANSI 16 colors from theme palette
   - 256-color (6×6×6 cube + 24 grayscale)
   - 24-bit true color (SGR `38;2;r;g;b` / `48;2;r;g;b`)
5. Theme TOML parsing
6. Theme loading from `~/.config/xerotty/themes/` and bundled `themes/`
7. iTerm2 import tool (`tools/iterm2-import.go`)
8. Bold-as-bright-colors option
9. Selection rendering (selection_fg/selection_bg colors)
10. Cursor style (block/underline/bar) and blink
11. Font configuration (family, size, path, extra glyph ranges)
12. Window configuration (columns, rows, padding, opacity)
13. 5 bundled themes (Dracula, Solarized Dark, Solarized Light, Gruvbox Dark, Monokai)

**Acceptance Criteria**:
- `tools/iterm2-import.go` correctly converts iTerm2 themes
- `cat` a true-color test script renders correctly
- Theme switching works (change config, restart)
- Font size and family from config are honored
- Bold text renders with bright colors when enabled

---

### Phase 3 — Right-Click Context Menu and Actions

**Goal**: The fully configurable right-click context menu is working, with all built-in actions.

**Packages**: `internal/menu`, `internal/input/clipboard.go`

**Deliverables**:
1. Menu config parsing (label, action, shortcut, enabled, submenu)
2. Action registry (map of action string → handler function)
3. All built-in actions:
   - `copy`, `paste`, `paste_selection`
   - `new_tab` (stub — opens in same window for now, tabs come in Phase 4)
   - `new_window` (spawn new process)
   - `close_tab` (exit for now)
   - `search` (stub — search comes in Phase 5)
   - `fullscreen`
   - `open_link`, `copy_link` (stubs — links come in Phase 6)
   - `select_all`, `clear_scrollback`, `reset_terminal`
   - `set_theme:<name>` (runtime theme switching)
   - `separator`
   - `exec:<command>` (shell hooks)
4. Context menu rendering:
   - `imgui.BeginPopupContextWindow()` on right-click
   - Iterate menu items in config order
   - Enabled condition evaluation
   - Submenu support via `imgui.BeginMenu()`
5. Clipboard implementation:
   - Text selection via mouse click-drag
   - PRIMARY clipboard (copy on select)
   - CLIPBOARD (explicit copy action)
   - Paste from both clipboards
6. Unsafe paste warning dialog
7. Keybind dispatch:
   - Parse keybind config into lookup table
   - Match SDL key events against table
   - Dispatch matching action

**Acceptance Criteria**:
- Right-click shows config-defined menu
- Reorder items in config → reorder in menu
- `exec:xdg-open .` works
- Copy/paste work with both clipboards
- Unsafe paste warning triggers on multi-line paste
- Ctrl+Shift+C/V keybinds work
- Submenus render and function

---

### Phase 4 — Tabs

**Goal**: Multiple terminal sessions in a tabbed interface.

**Packages**: `internal/tabs`

**Deliverables**:
1. TabManager with create/close/switch operations
2. ImGui tab bar rendering at top of window
3. Each tab owns an independent Terminal (SafeEmulator + PTY + goroutines)
4. Tab title from OSC 0/2 escape sequences
5. Tab close button (X on each tab)
6. Tab reordering (drag-and-drop via ImGui)
7. Keybinds: new tab, close tab, next/prev tab, go-to-tab-N
8. Tab close detection when child process exits (configurable behavior)
9. Last tab closed → window closes
10. Tab bar colors from theme/system/custom config
11. Connect `new_tab` and `close_tab` actions (Phase 3 stubs) to real tab operations
12. Rename tab action (ImGui text input popup)

**Acceptance Criteria**:
- Ctrl+Shift+T opens new tab with fresh shell
- Each tab operates independently (type in one, other is unaffected)
- Tab title updates when running commands (if shell sets title via OSC)
- Closing last tab exits the window
- Alt+N switches to tab N
- Tab reordering by drag works

---

### Phase 5 — Scrollback and Search

**Goal**: Configurable scrollback buffer with search functionality.

**Packages**: `internal/scrollback`

**Deliverables**:
1. ScrollbackBuffer implementation (ring buffer)
2. Integration with SafeEmulator (capture lines scrolling off-screen)
3. Mouse wheel scrolling
4. Shift+PageUp/PageDown/Home/End scrolling
5. Scroll-to-bottom on keystroke (configurable)
6. Scroll-to-bottom on output (configurable)
7. Configurable scroll speed (lines per wheel tick)
8. Unlimited mode with disk swap
9. Scrollback search:
   - Search overlay UI (ImGui input text + prev/next buttons)
   - Incremental search (search as you type)
   - Match highlighting in terminal (all matches highlighted, current match distinct)
   - Prev/Next navigation through matches
   - Escape to close search

**Acceptance Criteria**:
- `seq 1 50000` → can scroll back through all output
- Scroll position is maintained per-tab
- Ctrl+Shift+F opens search, finds text in scrollback
- Search highlights visible matches
- Unlimited mode works without excessive memory use

---

### Phase 6 — Scrollbar and Resize Overlay

**Goal**: Visual scrollbar and resize dimension overlay.

**Packages**: `internal/scrollback` (scrollbar integration), `internal/renderer` (overlay)

**Deliverables**:
1. Scrollbar rendering via ImDrawList (track, thumb)
2. Scrollbar interaction (click-drag thumb, click track to page)
3. Scrollbar visibility modes (always, never, auto)
4. Scrollbar colors from theme/system/custom
5. Hover effect on scrollbar thumb
6. Resize overlay (centered cols×rows text with rounded rect background)
7. Resize overlay fade-out animation
8. Terminal area width accounting for scrollbar width

**Acceptance Criteria**:
- Scrollbar shows correct position in scrollback
- Dragging scrollbar thumb scrolls the terminal
- `visible = "auto"` shows scrollbar only when scrolled up
- Resize overlay appears during window resize with correct dimensions
- Overlay fades out after configured duration

---

### Phase 7 — Links and Images

**Goal**: URL detection/interaction and image paste support.

**Packages**: `internal/renderer` (link highlighting), `internal/input` (image paste)

**Deliverables**:
1. URL regex detection on terminal lines
2. Underline on hover
3. Cursor change to hand on hover
4. Ctrl+click to open link
5. Context-aware "Open Link" / "Copy Link" menu items (connect Phase 3 stubs)
6. `xdg-open` link opening
7. Image paste detection (clipboard contains image data)
8. Kitty graphics protocol image transmission
9. Base64 fallback for non-graphics-capable apps
10. Image size limit check

**Acceptance Criteria**:
- URLs in `echo "https://example.com"` output are detected
- Hovering shows underline
- Ctrl+click opens in browser
- Right-click on URL shows "Open Link" in context menu
- Right-click elsewhere does NOT show "Open Link"
- Pasting an image in a kitty-protocol-supporting app works

---

### Phase 8 — Tmux Helpers and Polish

**Goal**: Tmux integration, final polish, and comprehensive testing.

**Packages**: all (polish pass)

**Deliverables**:
1. Tmux detection (`$TMUX` environment variable)
2. Tmux prefix key passthrough
3. Tmux cheatsheet overlay (built-in action)
4. Tmux submenu items (configurable)
5. Polish:
   - Shift+Enter sends configurable sequence
   - Runtime theme switching via `set_theme:` action without restart
   - Window opacity support (SDL2 `SDL_SetWindowOpacity`)
   - Font size adjustment keybinds (zoom in/out/reset)
   - System theme detection (GTK `gsettings`, Qt environment variables)
6. Comprehensive testing:
   - ANSI art test files
   - VT escape sequence test scripts
   - Automated cell grid verification
   - Manual test checklist execution

**Acceptance Criteria**:
- Running inside tmux: prefix key passes through to tmux
- Tmux cheatsheet overlay renders and dismisses
- All features from Phases 1-7 work together
- ANSI art renders correctly
- No goroutine leaks (verified with `runtime.NumGoroutine()`)
- No data races (verified with `-race` flag)

---

## 9. Test Strategy

### 9.1 Automated Tests

#### Unit Tests

| Package | What to Test |
|---------|-------------|
| `config` | TOML parsing, default values, validation, unknown key warnings |
| `themes` | Theme file parsing, color resolution order, iTerm2 import |
| `menu` | Menu config parsing, action parsing, enabled condition evaluation |
| `input` | Key notation parsing, SDL key → VT sequence translation, keybind matching |
| `scrollback` | Ring buffer behavior, unlimited mode disk swap, search matching |
| `renderer/colors` | ANSI index → ImU32, RGB → ImU32, bold-as-bright |
| `tabs` | Tab create/close/switch, tab ID generation, title update |

#### Integration Tests

Spawn a real terminal (SafeEmulator + PTY), send commands, verify cell grid state:

```go
func TestLsRendersOutput(t *testing.T) {
    term := NewTestTerminal(80, 24)
    defer term.Close()

    term.SendCommand("echo hello\r")
    term.WaitForOutput("hello", 2*time.Second)

    // Verify "hello" appears in the cell grid
    row := term.GetRow(1) // second row (after prompt)
    assert.Contains(t, row.Text(), "hello")
}
```

#### VT Escape Sequence Tests

Scripts in `testdata/scripts/` that exercise specific escape sequences:

| Test Script | Exercises |
|-------------|-----------|
| `colors_16.sh` | 16 ANSI colors (foreground + background) |
| `colors_256.sh` | 256-color palette rendering |
| `colors_truecolor.sh` | 24-bit true color gradients |
| `cursor_movement.sh` | CUU, CUD, CUF, CUB, CUP |
| `scroll_region.sh` | DECSTBM scroll region |
| `alt_screen.sh` | smcup/rmcup alternate screen buffer |
| `text_attrs.sh` | Bold, dim, italic, underline, strikethrough, inverse |
| `osc_title.sh` | OSC 0/2 title setting |
| `bracketed_paste.sh` | Bracketed paste mode enable/disable |
| `box_drawing.sh` | Box drawing characters (borders, corners) |
| `wide_chars.sh` | CJK double-width characters |

### 9.2 ANSI Art Test Files

Place `.ans` files in `testdata/ansi/` for visual regression testing. Classic ANSI art exercises:
- 16-color foreground/background
- Block characters (▀▄█░▒▓)
- Box drawing (┌┐└┘├┤┬┴┼─│)
- Color blending and attribute combinations

### 9.3 Race Condition Testing

Run all tests with Go's race detector:

```bash
go test -race ./...
```

The SafeEmulator serializes access, but the notification channel, tab management, and clipboard operations all involve concurrency.

### 9.4 Manual Test Checklist

A checklist for each release. Execute each item and verify correct behavior:

```
## Core Terminal
- [ ] Start xerotty → shell prompt appears
- [ ] Type commands → output renders correctly
- [ ] Run `vim` → full-screen TUI renders, cursor moves, insert mode works
- [ ] Run `htop` → renders with colors, mouse clicking works
- [ ] Run `mc` (Midnight Commander) → dual-pane renders, box drawing correct
- [ ] Resize window → terminal dimensions update, resize overlay shows
- [ ] Exit shell → window closes

## Colors
- [ ] Run 256-color test → all colors render
- [ ] Run true color test → smooth gradients
- [ ] Switch theme in config → restart → new colors apply
- [ ] Bold text uses bright colors (when enabled)
- [ ] Reverse video (inverse) renders correctly

## Tabs
- [ ] Ctrl+Shift+T → new tab opens
- [ ] Ctrl+Shift+W → tab closes
- [ ] Last tab close → window exits
- [ ] Alt+N → switches to tab N
- [ ] Tab title updates from OSC sequences
- [ ] Tab reorder by drag
- [ ] Type in one tab → other tab unaffected

## Context Menu
- [ ] Right-click → menu appears with config items
- [ ] "Copy" only appears when text is selected
- [ ] "Open Link" only appears when right-clicking a URL
- [ ] "New Tab" works from menu
- [ ] exec: action runs shell command
- [ ] Submenus render and function

## Clipboard
- [ ] Select text → auto-copied to PRIMARY
- [ ] Middle-click → paste from PRIMARY
- [ ] Ctrl+Shift+C → copy to CLIPBOARD
- [ ] Ctrl+Shift+V → paste from CLIPBOARD
- [ ] Multi-line paste → unsafe paste warning
- [ ] Paste with "sudo" → unsafe paste warning

## Scrollback
- [ ] Generate long output → scroll up with mouse wheel
- [ ] Shift+PageUp/PageDown → pages through scrollback
- [ ] Type a key while scrolled up → snaps to bottom
- [ ] Ctrl+Shift+F → search overlay appears
- [ ] Search finds text in scrollback
- [ ] Match highlighting works

## Scrollbar
- [ ] visible=always → scrollbar always shown
- [ ] visible=auto → scrollbar appears when scrolled, disappears at bottom
- [ ] Drag scrollbar thumb → terminal scrolls
- [ ] Click track → page scroll

## Links
- [ ] URL in output → underline on hover
- [ ] Ctrl+click → opens in browser
- [ ] Right-click on URL → "Open Link" in menu
- [ ] Right-click elsewhere → no "Open Link"

## Special Keys
- [ ] Shift+Enter → newline without submit (test in Claude Code or Python REPL)
- [ ] Backspace sends correct sequence
- [ ] Home/End work in shell and in vim
- [ ] Ctrl+C interrupts running process
- [ ] Ctrl+D sends EOF / closes shell
```

---

## 10. CLAUDE.md Template

The following should be placed as `CLAUDE.md` in the project root for AI assistants working on the codebase:

```markdown
# xerotty

Customizable terminal emulator for Linux. Built in Go with SDL2 + Dear ImGui.
See SPEC.md for the full specification.

## Build

    go build -o xerotty ./cmd/xerotty

## Run

    ./xerotty

Config: `~/.config/xerotty/config.toml`

## Architecture

- Main thread: SDL2/OpenGL + ImGui rendering (must be OS thread)
- Per tab: 2 goroutines (PTY reader, emulator reader)
- SafeEmulator (not Emulator) for thread safety
- New window = new process, no shared state

## Key Packages

| Package | Purpose |
|---------|---------|
| `internal/app` | Main loop, SDL2 lifecycle, keybind dispatch |
| `internal/config` | TOML config parsing and defaults |
| `internal/terminal` | SafeEmulator + PTY + goroutines |
| `internal/renderer` | Cell grid → ImDrawList |
| `internal/tabs` | Tab management |
| `internal/menu` | Config-driven context menu |
| `internal/input` | Key translation, clipboard |
| `internal/themes` | Theme loading, color resolution |
| `internal/scrollback` | Scrollback buffer, search |

## Dependencies

- creack/pty — PTY management
- charmbracelet/x/vt — Terminal emulation (SafeEmulator)
- AllenDang/cimgui-go — Dear ImGui Go bindings
- BurntSushi/toml — Config parsing

## Testing

    go test ./...
    go test -race ./...
```

---

## 11. Glossary

| Term | Definition |
|------|-----------|
| PTY | Pseudoterminal — a pair of virtual character devices (master/slave) that provide a terminal interface |
| SafeEmulator | Thread-safe wrapper from charmbracelet/x/vt that serializes concurrent read/write access |
| ImDrawList | ImGui's low-level drawing API for custom rendering (rectangles, lines, text, images) |
| OSC | Operating System Command — terminal escape sequences for setting title, colors, etc. |
| SGR | Select Graphic Rendition — escape sequences for text attributes and colors (e.g., `\x1b[31m` for red) |
| RLE | Run-Length Encoding — optimization that groups consecutive identical values |
| PRIMARY | X11 selection buffer — automatically set when text is highlighted, pasted with middle-click |
| CLIPBOARD | X11 clipboard — set by explicit copy (Ctrl+C), pasted by explicit paste (Ctrl+V) |
| Bracketed paste | Terminal mode where pasted text is wrapped in escape sequences so the application can distinguish it from typed input |
| Cell | One character position in the terminal grid, defined by column and row |
| Glyph | The visual representation of a character in a specific font |
