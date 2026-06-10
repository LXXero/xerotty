# xerotty

A terminal emulator for Linux and macOS that puts mouse users first, with a fully configurable right-click context menu and a TOML-driven everything-else.

Companion to [SevenTTY](https://github.com/LXXero/SevenTTY) — an SSH client and terminal emulator for classic Mac OS 7/8/9. Where SevenTTY brings modern SSH to vintage Macs, xerotty brings the user-centric design philosophy of classic Mac OS to modern Linux and macOS terminals.

## Why

xerotty optimizes for the thing most terminals ignore: the **GUI experience**. Tabs you can click, drag-reorder, and middle-click-close. Scroll that works like scroll. Right-click that works like right-click — opening a context menu that leads with the things you actually opened it for (New Tab, New Window) instead of burying them under clipboard items every shell already has shortcuts for.

The whole reason xerotty exists is that *the menu is yours*. Order, items, submenus, conditions, shell-exec actions — all driven from `~/.config/xerotty/config.toml`. No recompiling. No hidden defaults you can't override.

Same binary, same behavior, same menus on Linux and macOS. Built on ImGui and SDL3 — no GTK, no Cocoa, no platform widget toolkits dragging platform-specific bugs into your daily driver.

The rest follows from "no, you don't have to fight your terminal":

- Drag-resize that snaps to cell boundaries (macOS) so the terminal grid stays cell-aligned during the drag instead of leaving a sub-cell gutter.
- Live-resize that actually re-renders during the drag instead of stretching the previous frame like an image.
- Selection that respects how you started it — single-click drags by character, double-click drags by word, triple-click drags by line, with an iTerm2-style anchor that stays put.
- Real bold from your font's bold face when one exists, faux-bold when it doesn't (Monaco-style), drawn through the OS font system so emoji and Nerd Font glyphs Just Work without you pre-declaring atlas ranges.
- Theming via `[colors]` blocks or full `[theme]` files, including bundled iTerm2-imported palettes and a one-shot `tools/iterm2-import.go` for converting your own.
- A lava-lamp mode, because a terminal you stare at all day might as well be beautiful: soft animated glow blobs drifting behind the cells, colored from the active theme (`[appearance.glow]`, opt-in).

## Status

Working daily-driver. Recent arcs: the SDL2→SDL3 platform migration, and an optional headless **daemon** that owns terminal sessions so the GUI can detach/reattach, attach to remote hosts over SSH, and expose sessions to AI agents over MCP — all opt-in; in-process by default. Earlier focus: macOS support (cell-snap resize, live-resize render, OSC preprocessor, mouse mirror, clipboard via SDL native), runtime glyph cache replacing ImGui's static atlas for terminal cells, iTerm2-style word/line drag selection. See `TODO.md` for what's done and what's open.

## Features

### Terminal
- Tabs (rename, close, drag-to-reorder, on-exit policy: close / hold / hold_on_error)
- Configurable scrollback (memory / unlimited), search (Ctrl+F), Shift+Home / Shift+End
- Unsafe-paste detection (multiline / `sudo` / `rm -rf` / `curl | sh` patterns) with a yes/no confirm dialog
- Process-aware tab title from terminal escape sequences (OSC 0/1/2)
- Fullscreen (F11), runtime theme switching, font zoom (Ctrl+= / Ctrl+- / Ctrl+0)
- Lava-lamp glow backdrop — theme-derived animated color blobs behind the cells; opt-in, low-fps self-wake only while enabled (`[appearance.glow]` or the prefs dialog)

### Selection
- Char-precise drag (single click), word-snap drag (double click), line-snap drag (triple click)
- 3-class token model — `$` is its own token, not "$" + the space after it
- Selection auto-copies to PRIMARY (Linux) on release for middle-click paste; Cmd+C / Ctrl+Shift+C copies to CLIPBOARD. Opt-in `copy_on_select` ALSO writes CLIPBOARD on every selection (off by default; matches xterm/gnome/iTerm/etc.)

### Font / glyph system
- OS-backed font discovery — CoreText on macOS, fontconfig + FreeType on Linux
- Per-codepoint runtime glyph cache replaces ImGui's static atlas for terminal cells, so the full Unicode range is available without pre-declaring glyph ranges
- Real bold via `kCTFontBoldTrait` / fontconfig weight lookup; faux-bold fallback (`kCGTextFillStroke`) for fonts with no bold face
- Color emoji + Nerd Font fallback via OS cascade
- HiDPI: glyphs rasterized at framebuffer scale, pixel-snapped on draw
- Synthesized box-drawing and block elements so heavy / double / light variants tile pixel-perfectly

### Platform support
- **Linux**: X11 + Wayland (XWayland and native), PRIMARY selection via `xclip` / `xsel` / `wl-paste`
- **macOS**: Cmd keybindings via `ConfigMacOSXBehaviors`, point=pixel DPI matching iTerm, NSPasteboard via SDL native, cell-snap drag-resize, live-resize render via `SDL_AddEventWatch`

### Configurable everything
- Right-click menu — fully driven from TOML, supports nested submenus, action conditions (`enabled = "has_selection"`), shell-exec actions with `$XEROTTY_SELECTION` / `$XEROTTY_CWD`
- Keybinds — every action rebindable, including a separate `Cmd+...` set for macOS
- Themes — bundled (Dracula, Gruvbox Dark, Monokai, Solarized Dark/Light, Tango); load any iTerm2 `.itermcolors` via `tools/iterm2-import.go`

### Daemon / remote / AI control (optional)
- In-process by default — no daemon, no server, no phone-home. Everything below is strictly opt-in.
- `xerotty serve` — headless daemon that owns PTYs, scrollback, and the wire protocol; the GUI can close and reattach with tabs + scrollback intact (`source = "daemon"` under `[tabs]`)
- Remote attach over SSH — drive terminal sessions running on another host from the local GUI
- `xerotty connect` — CLI thin client to a local or remote daemon
- AI control via MCP — agents (Claude Code, custom orchestrators) read/write sessions as first-class clients over a JSON-RPC/MCP socket, alongside the GUI; trust-gated (`default_mode` / approval tokens)
- `xerotty mcp` — stdio bridge so MCP clients need zero socket plumbing: `claude mcp add xerotty -- xerotty mcp` and agents get `list_tabs` / `get_screen` / `send_input` / … as native tools ([`docs/MCP.md`](docs/MCP.md))
- `./build.sh headless` produces a lean `serve` + `connect` binary with no SDL3/GL/ImGui linked, for server installs
- Hot upgrade — `xerotty serve --upgrade` swaps the running daemon to a newly-installed binary **without killing its shells** (exec-in-place, nginx-style; [`docs/UPGRADE_PLAN.md`](docs/UPGRADE_PLAN.md))
- Design + code map: [`docs/DAEMON_PLAN.md`](docs/DAEMON_PLAN.md), [`docs/DAEMON_STATUS.md`](docs/DAEMON_STATUS.md)

## Build

Requires Go 1.22+ and SDL3 development headers.

```bash
# Linux (Debian/Ubuntu — SDL3 is in apt from trixie/24.04+; on older
# releases, build it from https://github.com/libsdl-org/SDL)
sudo apt install libsdl3-dev libfontconfig-dev libfreetype-dev pkg-config

# macOS (Homebrew)
brew install sdl3 pkg-config

git clone https://github.com/LXXero/xerotty
cd xerotty
./build.sh
# macOS: builds xerotty.app/ — open via `open xerotty.app` or copy to /Applications
# Linux: builds ./xerotty — run directly
```

## Configuration

xerotty reads `~/.config/xerotty/config.toml` on start. The full schema is in [`SPEC.md`](SPEC.md) §7. There's also an in-app preferences dialog (under the menu, or bind it to a key) covering fonts, theme picker, keybinds, clipboard behavior, link detection, and the unsafe-paste rules — every setting persists to the same TOML file.

Bundled themes live in [`themes/`](themes/) and are referenced by name:

```toml
[appearance]
theme = "dracula"  # or "gruvbox-dark", "monokai", "solarized-dark", "solarized-light", "tango"
```

The lava-lamp glow backdrop, in full (everything optional except `enabled`):

```toml
[appearance.glow]
enabled   = true
colors    = ["#ff79c6", "#bd93f9"]  # omit to derive from the active theme's accents
blobs     = 5      # lava clusters (1-16)
background_fps = 10 # lamp animation cap in unfocused windows (0 = full rate; text is never throttled)
speed     = 1.0    # drift / morph speed multiplier
scale     = 0.7    # blob size relative to the window
intensity = 0.35   # glow alpha (0-1)
fps       = 20     # animation tick — only while enabled
```

To convert an iTerm2 theme:

```bash
go run ./tools/iterm2-import.go path/to/Theme.itermcolors > ~/.config/xerotty/themes/theme.toml
```

## Architecture

```
PTY (creack/pty)  →  SafeEmulator (charmbracelet/x/vt)  →  ImDrawList (SDL3 + Dear ImGui)
     ↑                                                              ↓
  keyboard / mouse  ←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←  SDL3 window
```

Two goroutines per terminal (PTY reader, emulator-response reader); main thread locked to the OS thread for SDL3/OpenGL drives the ImGui frame loop. "New Window" spawns an additional in-process OS window via ImGui multi-viewport — one process, one ImGui context, N visible windows sharing font caches and config. See [`docs/MULTI_WINDOW_REFACTOR.md`](docs/MULTI_WINDOW_REFACTOR.md) for the architecture.

Tabs run in-process by default. With `source = "daemon"` they instead live in a headless `xerotty serve` process over a msgpack wire protocol, so the GUI is a thin client that can detach, reattach, and span local + remote daemons; a JSON-RPC/MCP socket lets AI agents drive the same sessions. See [`docs/DAEMON_PLAN.md`](docs/DAEMON_PLAN.md).

Full architecture, package responsibilities, and rendering pipeline detail in [`SPEC.md`](SPEC.md).

## Repository layout

```
cmd/xerotty/         entry point (xerotty / serve / connect)
internal/app/        main loop, window/tab lifecycle, keybind dispatch, prefs
internal/config/     TOML parsing, defaults
internal/terminal/   SafeEmulator + PTY, disk scrollback, Source interface
internal/renderer/   cell grid → ImDrawList
internal/fontsys/    OS font discovery (CoreText / fontconfig)
internal/glyphcache/ per-codepoint GPU texture cache
internal/platform/   SDL3 + Dear ImGui backend (cgo glue)
internal/menu/       config-driven right-click menu
internal/themes/     theme loading
internal/scrollback/ buffer / search / disk swap
# daemon / remote / MCP arc:
internal/protocol/   msgpack wire format (codegen via msgp)
internal/daemon/     session + tab + window mgmt, client registry
internal/runner/     serve / connect / stdio-bridge subcommands
internal/clientproto/  client side of the wire protocol
internal/daemonsource/ Hub + Source for daemon-backed GUI tabs
internal/mcp/        per-daemon JSON-RPC/MCP server
internal/guimcp/     GUI's aggregating MCP server (one socket, all daemons)
docs/                planning notes (daemon, SDL3, resize, multi-window)
themes/              bundled palettes
tools/               iterm2-import, glyph-dump diagnostic
```

## Related

- [SevenTTY](https://github.com/LXXero/SevenTTY) — SSH + terminal for classic Mac OS 7/8/9. xerotty is the modern-OS counterpart.

## License

[GPL-3.0](LICENSE). Free as in freedom.
