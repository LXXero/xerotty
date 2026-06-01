# xerotty TODO

## Done

### Core terminal
- [x] unsafe paste dialog (yes/no confirm + multiline / newline-guard / regex patterns)
- [x] tab rename
- [x] font_size_reset (Ctrl+Shift+0)
- [x] scroll_on_output freeze — viewport stays put when scrolled back, offset adjusts for new scrollback
- [x] fullscreen (F11) — `SDL_WINDOW_FULLSCREEN_DESKTOP`
- [x] runtime theme switching — `set_theme:<name>` action
- [x] Shift+Home / Shift+End — scroll_top / scroll_bottom
- [x] on_child_exit — close / hold / hold_on_error
- [x] Shift+Enter — newline or escape sequence (configurable)
- [x] new tab inherits CWD — `inherit_cwd` config option
- [x] select_all / clear_scrollback / reset_terminal actions
- [x] config dialog (preferences UI; flat font picker, theme picker, keybinds, clipboard, links, etc.)
- [x] context menu submenus — `[[menu.items.submenu]]` nested arrays in TOML
- [x] multi-window — single process, ImGui multi-viewport, one Dock icon for N OS windows

### Selection
- [x] double-click word selection (3-class: word / whitespace / punctuation)
- [x] double-click whitespace selects the run
- [x] triple-click selects line
- [x] iTerm2-style hold-and-drag after double-click extends selection by word
- [x] iTerm2-style hold-and-drag after triple-click extends selection by row

### Font / glyph system
- [x] OS-backed font discovery (`internal/fontsys`) — CoreText on macOS, fontconfig+FreeType on Linux
- [x] dynamic per-codepoint glyph cache (`internal/glyphcache`) — full Unicode coverage incl. SMP emoji, no static atlas size limit for terminal cells
- [x] real bold (CoreText `kCTFontBoldTrait` / fontconfig weight=bold) with faux-bold fallback (`kCGTextFillStroke`) for fonts lacking a bold face
- [x] color emoji + Nerd Font cascade — primary font tried first, OS cascade for missing glyphs, color glyphs proportionally fitted to cell
- [x] HiDPI: glyphs rasterized at fb-scale × pxSize, drawn at logical, integer-pixel-snapped
- [x] box-drawing (U+2500–U+257F) and block elements (U+2580–U+259F) synthesized as filled rects so heavy/double/light variants tile cleanly with no gaps
- [x] variable-font guard (`fontsys.IsVariableFont`) — skips fvar/gvar fonts that crash ImGui's bundled stbtt parser

### Platform support — macOS
- [x] DPI: point = pixel (`72 dpi` reference) so `12pt` matches iTerm
- [x] mouse-event mirror (`internal/sdlhack`) — recovers from SDL2/Cocoa dropped mouse-up events
- [x] mirror guards: only inject DOWN when cursor is in main window content rect AND not in a live-resize-driven frame
- [x] Cmd keybindings via `ConfigMacOSXBehaviors` (Cmd+T, Cmd+W, Cmd+C, etc. with display labels showing "Cmd+...")
- [x] clipboard via SDL native API (`SDL_GetClipboardText` / `SDL_SetClipboardText`) — uses NSPasteboard
- [x] OSC preprocessor — bypasses charm/x/ansi treating `\x9c` as ST mid-UTF-8 (window title escape sequences)
- [x] cell-snap window resize (`NSWindow.setContentResizeIncrements:`)
- [x] live-resize render — SDL event watch drives a full ImGui frame from inside AppKit's tracking mode

### SDL3 platform migration
- [x] SDL2 → SDL3 + Dear ImGui's official SDL3 backend, via the `internal/platform` cgo layer (`docs/SDL3_PLAN.md`)
- [x] native Wayland popups (`SDL_CreatePopupWindow` + `wlgrab`) replacing the broken multi-viewport ImGui popups; keyboard-navigable right-click menu + prefs choosers
- [x] tab drag, disk scrollback, snap-resize ported onto SDL3

### Daemon / remote / MCP (`docs/DAEMON_PLAN.md`, `docs/DAEMON_STATUS.md`)
- [x] one binary, three roles — `xerotty` (GUI) / `xerotty serve` (headless daemon) / `xerotty connect` (CLI client)
- [x] msgpack wire protocol (`internal/protocol`, codegen via msgp); daemon owns PTYs + scrollback
- [x] detach / reattach — close the GUI, reopen, tabs + scrollback survive (`source = "daemon"`, auto-spawns the daemon)
- [x] SSH remote attach — drive sessions on another host from the local GUI
- [x] structured paste + OSC 52 clipboard sync over the wire
- [x] MCP control — per-daemon JSON-RPC/MCP server + GUI-aggregating socket (host-namespaced tab IDs); trust model (`default_mode` / approval tokens)
- [x] multi-attach + topology broadcast + snapshot/revision resync
- [x] connection resilience — dead/hung/slept client detection (async bounded writers, app heartbeat, write deadlines, SSH keepalive); live-validated
- [x] `./build.sh headless` — lean `serve` + `connect` binary, no SDL3/GL/ImGui linked, for server installs

## Open

### Low priority / later
- [ ] image paste / Kitty graphics protocol — base64 PNG/JPEG via OSC 1337 / APC; also iTerm2 inline images
- [ ] Tmux helpers (SPEC §6.13) — explicitly last per original plan
- [ ] iTerm2-style "smart selection" — URL / path / IP / git-hash auto-detect on quad-click or modifier-drag
- [ ] daemon: SHM local cell-grid transport (Plan Phase 8) — deferred; msgpack is fast enough so far
- [ ] daemon: per-host color badges + destructive-command gate for remote sessions (Plan Phase 6 future)

## Notes

- Planning notes from earlier debugging sessions live in `docs/`:
  - `docs/RESIZING_PLAN.md` — Wayland snap-on-resize tradeoffs (mostly resolved on macOS via cell-snap; Linux still drag-with-floor)
  - `docs/plan.md` — original early-phase architecture sketch (superseded by `SPEC.md`)
