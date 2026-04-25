# xerotty — Terminal Emulator

A customizable terminal emulator built in Go with SDL2 + Dear ImGui. The whole point: **fully configurable right-click context menus** and a UI that doesn't fight the mouse.

Pairs with [seventty](https://github.com/LXXero/SevenTTY) in the naming convention.

## Stack

| Layer | Library | Why |
|---|---|---|
| PTY | `creack/pty` | Standard Go PTY library, pure Go |
| Terminal emulation | `charmbracelet/x/vt` | Full VT parser + screen buffer, used by Charm ecosystem |
| GUI | `AllenDang/cimgui-go` (SDL2 backend) | Dear ImGui = fully customizable menus, fast text rendering |
| Config | `BurntSushi/toml` | TOML config file |

## Architecture

```
PTY (creack/pty)  →  SafeEmulator (charmbracelet/x/vt)  →  ImDrawList (cimgui-go)
     ↑                                                            ↓
  keyboard input  ←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←  SDL2 window
```

Two goroutines per terminal:
1. **PTY reader**: reads PTY fd → feeds `SafeEmulator.Write()` → signals main thread
2. **Emulator reader**: reads `SafeEmulator.Read()` → writes back to PTY (for device responses)

Main thread (locked to OS thread for SDL2/OpenGL):
- Drains data notifications
- Handles SDL input events → translates to `SendKey()`/`SendText()`
- Renders tab bar (ImGui tabs)
- Renders active terminal's cell grid via `ImDrawList.AddRectFilled()` (backgrounds) + `ImDrawList.AddText()` (glyphs)
- Renders right-click context menu from config

## Package Layout

```
xerotty/
├── cmd/xerotty/
│   └── main.go              # Entry point: flags, config, create App, run
├── internal/
│   ├── app/
│   │   └── app.go           # App struct, backend init, main loop, keybind dispatch
│   ├── config/
│   │   └── config.go        # Config types, TOML parsing, defaults
│   ├── terminal/
│   │   ├── terminal.go      # Terminal struct: SafeEmulator + PTY + goroutines
│   │   └── pty.go           # Shell detection, PTY spawn helpers
│   ├── renderer/
│   │   ├── renderer.go      # Cell grid → ImDrawList rendering
│   │   ├── font.go          # Font loading, cell metrics, glyph ranges
│   │   └── colors.go        # ansi.Color → ImU32 conversion
│   ├── tabs/
│   │   └── tabs.go          # TabManager: create/close/switch tabs
│   ├── menu/
│   │   └── menu.go          # Config-driven context menu, action registry
│   └── input/
│       ├── input.go         # SDL key → vt key mapping, modifier translation
│       └── clipboard.go     # SDL clipboard read/write
├── go.mod
└── go.sum
```

## Config File (`~/.config/xerotty/config.toml`)

```toml
shell = ""           # empty = $SHELL or /bin/bash
scrollback = 10000

[font]
path = ""            # empty = bundled/system monospace
size = 14.0

[colors]
foreground = "#d4d4d4"
background = "#1e1e1e"
cursor = "#ffffff"
palette = [
    "#000000", "#cd3131", "#0dbc79", "#e5e510",
    "#2472c8", "#bc3fbc", "#11a8cd", "#e5e5e5",
    "#666666", "#f14c4c", "#23d18b", "#f5f543",
    "#3b8eea", "#d670d6", "#29b8db", "#e5e5e5",
]

# Menu is fully customizable — order, items, everything
[[menu.items]]
label = "Open Tab"
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

[[menu.items]]
label = "Paste"
action = "paste"
shortcut = "Ctrl+Shift+V"

[[menu.items]]
action = "separator"

[[menu.items]]
label = "Close Tab"
action = "close_tab"
shortcut = "Ctrl+Shift+W"
```

## Rendering Strategy

- **Backgrounds**: Run-length encode consecutive cells with same bg color → single `AddRectFilled()` per run (huge perf win)
- **Foreground text**: Per-glyph `AddText()` using ImGui's built-in font atlas (already batched into one GPU draw call)
- **Wide chars**: CJK cells with Width=2 span two cell widths, skip the empty continuation cell
- **Cursor**: Blinking block/underline/bar drawn over the cell at cursor position
- **Scrollback**: Mouse wheel adjusts scroll offset, renders from scrollback buffer when offset < 0

## Implementation Phases

### Phase 1 — Single terminal, no tabs, no menu (~500 lines)
- `cmd/xerotty/main.go`
- `internal/app/app.go` — SDL2+ImGui backend, main render loop
- `internal/terminal/terminal.go` + `pty.go` — spawn shell, PTY goroutines
- `internal/renderer/renderer.go` + `font.go` + `colors.go` — cell grid rendering
- `internal/input/input.go` — keyboard input forwarding
- `internal/config/config.go` — hardcoded defaults only
- **Goal**: window opens, bash runs, you can type commands and see output

### Phase 2 — Tabs (~120 lines)
- `internal/tabs/tabs.go` — TabManager
- Wire ImGui tab bar into app loop
- Ctrl+Shift+T creates new tab, Ctrl+Shift+W closes

### Phase 3 — Context menu + config file (~210 lines)
- `internal/menu/menu.go` — config-driven right-click menu
- TOML config parsing in `internal/config/config.go`
- Action registry (new_tab, new_window, copy, paste, close_tab)
- **This is the whole reason the project exists**

### Phase 4 — Scrollback + clipboard + polish (~200 lines)
- Scroll offset tracking, mouse wheel handling
- Text selection with mouse drag
- Copy/paste via SDL clipboard
- Process exit detection (auto-close or "[process exited]")
- Window title from terminal escape sequences

## Estimated total: ~1,030 lines of Go

## Key Design Decisions

1. **`SafeEmulator` (not `Emulator`)** — PTY goroutine writes concurrently with main thread reading cells
2. **New Window = new OS process** — `os.Executable()` + `exec.Command()`, no shared state complexity
3. **Menu entirely from config** — no hardcoded menu items, user controls everything
4. **TERM=xterm-256color** — standard, well-supported
5. **ImGui font atlas for glyphs** — no custom texture atlas needed for MVP, ImGui batches draws internally
6. **TOML config** — familiar, simple, matches the `[[array.of.tables]]` pattern well for menu items
