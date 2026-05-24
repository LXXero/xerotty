# xerotty — agent notes

Conventions and project context for AI assistants working on this
codebase. Read this before making changes.

## Build — always use `build.sh` or `make`

**Never `go build ./cmd/xerotty` directly.** Use `./build.sh` (which
runs `go generate ./...` first, then dispatches to `make` on macOS or
plain `go build` on Linux). On macOS `make app` is what produces the
`.app` bundle with the icon + Info.plist needed for Dock-icon
coalescing.

Direct `go build` skips `go generate`, which will silently break the
daemon protocol once codegen-driven encoders (tinylib/msgp) land —
you'd get a binary with stale wire formats and cryptic
runtime-decode failures.

If you must run `go build` for a quick syntax check, that's OK — but
the artifact isn't shippable and isn't what tests should run against.

**Headless build:** `./build.sh headless` (or `make headless`)
produces `xerotty-headless` with NO SDL3/GL/ImGui/freetype/fontconfig
linked — `serve` + `connect` only, GUI default stubbed. For server
installs. The `-tags headless` build tag excludes `cmd/xerotty/gui.go`
(the ONLY file importing `internal/app`) and substitutes
`gui_headless.go`. **Invariant: `internal/app` must be imported from
exactly one `//go:build !headless` file** — build.sh guards this with
a `go list -deps` import-graph check + an `ldd` check, both of which
fail the build if a GUI import escapes the tag. Install the lean
artifact AS `xerotty` on servers so the SSH bridge + auto-spawn
(`xerotty serve`) stay uniform.

## Architecture

xerotty can run its tabs in-process (default) OR through a
headless daemon. ONE binary, three roles via subcommand:

- `xerotty` — GUI (default). In-process PTY tabs, or attaches to
  local + remote daemons.
- `xerotty serve` — headless daemon: owns PTYs, scrollback, the
  wire-protocol socket, and the MCP socket.
- `xerotty connect` — CLI thin client.

Shipped on `spike/daemon`. See `docs/DAEMON_PLAN.md` (design +
phase history) and `docs/DAEMON_STATUS.md` (package map +
how-to-verify). Two protocols: msgpack wire (`internal/protocol`)
for the terminal data plane, JSON-RPC/MCP (`internal/mcp` +
`internal/guimcp`) for AI control.

Older arc that's already merged: SDL2 → SDL3 + the new
`internal/platform/` package. See `docs/SDL3_PLAN.md` for context on
what was done and why. The platform layer is bypass-cimgui-go custom
cgo glue around SDL3 + Dear ImGui's official SDL3 backend.

Daemon-arc packages:
- `internal/protocol` — msgpack wire format (codegen via msgp).
- `internal/daemon` — session + tab + window management, client
  registry, broadcast helpers.
- `internal/runner` — serve / connect / stdio-bridge subcommands.
- `internal/clientproto` — client side of the wire protocol.
- `internal/daemonsource` — `Hub` + `Source` (terminal.Source
  backed by a shadow vt emulator) for daemon-backed GUI tabs.
- `internal/terminal/source.go` — the `Source` interface both
  in-process PTY and daemon tabs implement.
- `internal/mcp` — per-daemon JSON-RPC/MCP server.
- `internal/guimcp` — GUI's aggregating MCP server (one socket
  over all daemons, host-namespaced tab IDs).

GUI integration is done: `internal/app` flips tabs between
in-process and daemon-backed via `cfg.Tabs.Source`
(`pty`/`daemon`/`daemon:<host>`).

## Code structure

```
cmd/xerotty/         entry point
internal/app/        main loop, window/tab lifecycle, prefs dialog
internal/config/     TOML parsing
internal/terminal/   SafeEmulator wrapper, PTY, disk scrollback
internal/renderer/   cell grid → ImDrawList
internal/fontsys/    OS font discovery (CoreText / fontconfig)
internal/glyphcache/ per-codepoint GPU texture cache
internal/menu/       config-driven right-click menu
internal/themes/     theme loading
internal/scrollback/ buffer / search
internal/platform/   SDL3 + ImGui SDL3 backend (cgo shell)
internal/input/      keyboard event → byte sequence translation
docs/                planning notes (SDL3, daemon, resize, etc.)
themes/              bundled palettes
tools/               iterm2-import, glyph-dump
```

## Conventions

- **Cell type is `ultraviolet.Cell`** (`github.com/charmbracelet/ultraviolet`).
  `Content` is a UTF-8 grapheme cluster (string, not rune), `Style`
  carries fg/bg/attrs, `Width` is monospace cell width (1 / 2 / 0).
- **PTY data flow**: PTY bytes → `vt.SafeEmulator` (state machine) →
  cell grid + scrollback → renderer reads.
- **Don't add `//go:generate` directives without verifying** `build.sh`
  and `make` both pick them up.
- **Comments explain WHY, not WHAT**. Most existing comments here
  follow that rule — match the tone.

## When in doubt

- `SPEC.md` for the formal spec and config schema
- `TODO.md` for what's done and what's open
- `docs/DAEMON_PLAN.md` for the daemon/MCP arc
- `docs/SDL3_PLAN.md` for the (completed) SDL2→SDL3 migration
- `docs/MULTI_WINDOW_REFACTOR.md` for the multi-window architecture

When asked to add a feature, check those docs first — most decisions
that look arbitrary have a documented "why" somewhere.
