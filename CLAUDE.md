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

## Architecture

Currently bundled UI + PTY + scrollback in one process. There's an
active arc to split this into:

- `xerottyd` — headless daemon owning PTYs, scrollback, the wire
  protocol socket, and the MCP socket
- `xerotty` — thin UI client (SDL3 + ImGui) that attaches to one or
  more daemons over a structured protocol

See `docs/DAEMON_PLAN.md` for the design. Not yet started, but the
plan governs incoming architecture decisions.

Older arc that's already merged: SDL2 → SDL3 + the new
`internal/platform/` package. See `docs/SDL3_PLAN.md` for context on
what was done and why. The platform layer is bypass-cimgui-go custom
cgo glue around SDL3 + Dear ImGui's official SDL3 backend.

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
