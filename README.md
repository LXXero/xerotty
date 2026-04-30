# xerotty

A terminal emulator for Linux and macOS that puts mouse users first, with a fully configurable right-click context menu and a TOML-driven everything-else.

Companion to [SevenTTY](https://github.com/LXXero/SevenTTY) — an SSH client and terminal emulator for classic Mac OS 7/8/9. Where SevenTTY brings modern SSH to vintage Macs, xerotty brings the user-centric design philosophy of classic Mac OS to modern Linux and macOS terminals.

## Why

xerotty optimizes for the thing most terminals ignore: the **GUI experience**. Tabs you can click, drag-reorder, and middle-click-close. Scroll that works like scroll. Right-click that works like right-click — opening a context menu that leads with the things you actually opened it for (New Tab, New Window) instead of burying them under clipboard items every shell already has shortcuts for.

The whole reason xerotty exists is that *the menu is yours*. Order, items, submenus, conditions, shell-exec actions — all driven from `~/.config/xerotty/config.toml`. No recompiling. No hidden defaults you can't override.

Same binary, same behavior, same menus on Linux and macOS. Built on ImGui and SDL2 — no GTK, no Cocoa, no platform widget toolkits dragging platform-specific bugs into your daily driver.

The rest follows from "no, you don't have to fight your terminal":

- Drag-resize that snaps to cell boundaries (macOS) so the terminal grid stays cell-aligned during the drag instead of leaving a sub-cell gutter.
- Live-resize that actually re-renders during the drag instead of stretching the previous frame like an image.
- Selection that respects how you started it — single-click drags by character, double-click drags by word, triple-click drags by line, with an iTerm2-style anchor that stays put.
- Real bold from your font's bold face when one exists, faux-bold when it doesn't (Monaco-style), drawn through the OS font system so emoji and Nerd Font glyphs Just Work without you pre-declaring atlas ranges.
- Theming via `[colors]` blocks or full `[theme]` files, including bundled iTerm2-imported palettes and a one-shot `tools/iterm2-import.go` for converting your own.

## Status

Working daily-driver. Recent focus: macOS support (cell-snap resize, live-resize render, OSC preprocessor, mouse mirror, clipboard via SDL native), runtime glyph cache replacing ImGui's static atlas for terminal cells, iTerm2-style word/line drag selection. See `TODO.md` for what's done and what's open.

## Features

### Terminal
- Tabs (rename, close, drag-to-reorder, on-exit policy: close / hold / hold_on_error)
- Configurable scrollback (memory / disk / unlimited), search (Ctrl+F), Shift+Home / Shift+End
- Unsafe-paste detection (multiline / `sudo` / `rm -rf` / `curl | sh` patterns) with a yes/no confirm dialog
- Process-aware tab title from terminal escape sequences (OSC 0/1/2)
- Fullscreen (F11), runtime theme switching, font zoom (Ctrl+= / Ctrl+- / Ctrl+0)

### Selection
- Char-precise drag (single click), word-snap drag (double click), line-snap drag (triple click)
- 3-class token model — `$` is its own token, not "$" + the space after it
- Selection auto-copies to PRIMARY (Linux) on release; Cmd+C / Ctrl+Shift+C copies to CLIPBOARD

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

## Build

Requires Go 1.22+ and SDL2 development headers.

```bash
# Linux (Debian/Ubuntu)
sudo apt install libsdl2-dev libfontconfig-dev libfreetype-dev pkg-config

# macOS (Homebrew)
brew install sdl2 pkg-config

git clone https://github.com/LXXero/xerotty
cd xerotty
./build.sh
./xerotty
```

## Configuration

xerotty reads `~/.config/xerotty/config.toml` on start. The full schema is in [`SPEC.md`](SPEC.md) §7. There's also an in-app preferences dialog (under the menu, or bind it to a key) covering fonts, theme picker, keybinds, clipboard behavior, link detection, and the unsafe-paste rules — every setting persists to the same TOML file.

Bundled themes live in [`themes/`](themes/) and are referenced by name:

```toml
[appearance]
theme = "dracula"  # or "gruvbox-dark", "monokai", "solarized-dark", "solarized-light", "tango"
```

To convert an iTerm2 theme:

```bash
go run ./tools/iterm2-import.go path/to/Theme.itermcolors > ~/.config/xerotty/themes/theme.toml
```

## Architecture

```
PTY (creack/pty)  →  SafeEmulator (charmbracelet/x/vt)  →  ImDrawList (cimgui-go SDL2 backend)
     ↑                                                              ↓
  keyboard / mouse  ←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←←  SDL2 window
```

Two goroutines per terminal (PTY reader, emulator-response reader); main thread locked to the OS thread for SDL2/OpenGL drives the ImGui frame loop. Each "New Window" is a fresh OS process — no shared state, no IPC.

Full architecture, package responsibilities, and rendering pipeline detail in [`SPEC.md`](SPEC.md).

## Repository layout

```
cmd/xerotty/         entry point
internal/app/        SDL2/ImGui lifecycle, main loop, keybind dispatch
internal/config/     TOML parsing, defaults
internal/terminal/   SafeEmulator + PTY
internal/renderer/   cell grid → ImDrawList
internal/fontsys/    OS font discovery (CoreText / fontconfig)
internal/glyphcache/ per-codepoint GPU texture cache
internal/sdlhack/    SDL2 platform-quirk workarounds
internal/menu/       config-driven right-click menu
internal/themes/     theme loading
internal/scrollback/ buffer / search / disk swap
docs/                planning notes (resize, original architecture)
themes/              bundled palettes
tools/               iterm2-import, glyph-dump diagnostic
```

## Related

- [SevenTTY](https://github.com/LXXero/SevenTTY) — SSH + terminal for classic Mac OS 7/8/9. xerotty is the modern-OS counterpart.

## License

TBD.
