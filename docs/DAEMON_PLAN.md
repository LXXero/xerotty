# xerotty daemon + thin UI + MCP

> **HEAD UPDATE**: This plan originally proposed a separate
> `xerottyd` binary for the daemon role. That was collapsed
> early into the implementation — there's only ONE binary
> (`xerotty`) with subcommands:
>
> - `xerotty`            — GUI (default), can attach to local
>                          or remote daemons
> - `xerotty serve`      — daemon mode (what this doc calls
>                          "xerottyd"); owns PTYs + wire socket
>                          + MCP socket
> - `xerotty connect`    — CLI thin client
>
> Socket FILENAMES still use the `xerottyd.sock` /
> `xerottyd.mcp.sock` names for path stability, but the binary
> itself is just `xerotty`. Anywhere this doc says "the
> xerottyd binary", read "xerotty serve". Do NOT split it back
> into a second binary.

The major architectural arc. Split xerotty into a **headless daemon**
(`xerotty serve`) that owns terminal sessions, and a **thin UI client**
(`xerotty`) that attaches to one or more daemons over a structured
protocol. The daemon also exposes an **MCP socket** so AI agents
(Claude Code, Xyphia, custom orchestrators) can read/write sessions
as first-class clients alongside the UI.

## Why

Right now xerotty bundles UI + PTY ownership + scrollback in one
process. That couples a bunch of things that don't have to be coupled:

- Crash the UI → lose all running processes.
- SSH to a remote box → fight escape-sequence-based image / clipboard
  protocols (Kitty graphics, OSC 52, OSC 1337) that get mangled by
  every middleman.
- AI control means scraping the UI somehow, no first-class data path.
- Multiple humans on the same session = tmux territory, separate tool.
- Running xerotty headless (CI, autonomous agents) requires GPU/display.

Daemon mode collapses tmux + mosh + iTerm2-session-restore + remote-
terminal + AI control surface into one protocol. Same trick
[wezterm's mux mode](https://wezterm.org/multiplexing.html) plays for
remote sessions, plus MCP plus the AI-driver model from `MCP_PLAN.md`
(not yet written but discussed).

## Architecture

```
┌──────────────────────────────────────────────────────────┐
│ host A (laptop)                                          │
│                                                          │
│ xerotty UI (one process per user)                        │
│   ├── tab attached to host A's local xerottyd            │
│   ├── tab attached to host B's xerottyd over SSH         │
│   └── tab attached to host C's xerottyd over SSH         │
└──────────────────────────────────────────────────────────┘
       │            │             │
       │ unix sock  │ ssh stdio   │ ssh stdio
       │            │             │
   ┌───▼────┐   ┌───▼────┐    ┌──▼─────┐
   │xerottyd│   │xerottyd│    │xerottyd│
   │ host A │   │ host B │    │ host C │
   └───┬────┘   └───┬────┘    └──┬─────┘
       │            │             │
       │ MCP socket each          │
       │                          │
   ┌───▼──────────────────────────▼─────┐
   │ AI clients: Claude Code, Xyphia,   │
   │ custom scripts. Each can drive any │
   │ daemon's tabs in observe/propose/  │
   │ auto mode.                         │
   └────────────────────────────────────┘
```

**xerottyd is headless.** It owns:
- PTYs and their child processes
- in-memory cell grid (per session/tab)
- scrollback (memory + disk in unlimited mode)
- tab/session list (per-daemon namespace)
- the wire protocol socket (UI clients)
- the MCP socket (AI clients)

It renders nothing. No SDL, no ImGui, no GL on the daemon side. Just
PTY I/O, the vt state machine, file/socket I/O.

**xerotty (the UI)** owns:
- SDL3 window + GL + ImGui
- glyph cache + font system + theming
- input → protocol-event translation
- protocol-cell-frame → on-screen rendering
- tab bar + prefs dialog + every other widget
- the **per-tab Source abstraction** (see below) — each tab
  independently picks where its cells come from

## Per-tab Source — the UI is a multiplexer

The UI doesn't have to commit to "all in-process" or "all daemon" —
each tab independently picks its source. One window can have:
- a local in-process PTY tab (today's behavior, fastest, simplest)
- a tab connected to a local `xerottyd` (persistent + MCP-accessible)
- a tab connected to `xerottyd@kh` over SSH (remote shell, persistent)
- a tab connected to `xerottyd@xRyzen` over SSH

All four use the same renderer, same input handling, same scrollback
visualization. They differ only in where the cells originate.

```go
// internal/terminal/source.go (sketch)
type Source interface {
    Resize(cols, rows int)
    Write(b []byte)                 // input bytes → PTY (local or remote)
    CellAt(row, col int) *uv.Cell
    Cursor() (row, col int, visible bool)
    ScrollbackLen() int
    DataCh() <-chan struct{}        // wakes UI when new data arrives
    Close()
    // ... title, cwd, foreground proc, ...
}

// implementations:
type LocalPTYSource  struct { /* SafeEmulator + PTY, today's terminal.Terminal */ }
type DaemonSource    struct { /* conn to xerottyd + cell mirror that diffs apply to */ }
```

UI doesn't care which. `Tab.Source` is whichever was selected when
the tab opened.

## Lifecycle (model A, wezterm-style)

Daemon mode is **opt-in per tab**, not all-or-nothing. The daemon
binary `xerottyd` is **lazily auto-spawned** when a tab first needs
it; users never manage the daemon lifecycle directly.

**Default behavior** (out of the box): new tabs are in-process
`LocalPTYSource`. Identical to today. Fast startup, no daemon
involved.

**Opt-in to "every tab persists"**: one config knob.
```toml
[startup]
default_tab_source = "daemon"   # "local" (default) | "daemon"

[daemon]
auto_spawn = true               # fork xerottyd in background if not running
                                # when a daemon-source tab first opens.
                                # default: true
idle_timeout_minutes = 0        # 0 = never auto-exit; >0 = GC when no
                                # sessions attached for that long
listen = "unix"                 # "unix" local only (default), or
                                # "unix+tls" to also expose a TLS port
                                # for cross-host direct attach
```

With `default_tab_source = "daemon"` set, the user's daily flow is
identical (`xerotty`, terminal opens, type into it) but now:
- closing the UI doesn't kill running processes — daemon survives
- reopening attaches to the same tabs, mid-scrollback intact
- another machine can SSH-attach to the SAME daemon and see the SAME
  tabs, work continues from wherever
- AI agents (Claude / Xyphia) can connect to the MCP socket whenever

**Remote tabs** always go through SSH:
```sh
xerotty connect ssh://kh             # opens new window attached to kh's daemon
xerotty connect ssh://kh --tab        # opens new tab in existing window
```
Under the hood: SSH to host, run `xerottyd attach` over stdio, every
protocol frame flows over the SSH pipe. No new ports opened on the
remote, no auth to configure — reuses `~/.ssh/config`.

**Right-click / hotkey** in any running UI:
- "New Tab" → uses `default_tab_source`
- "New Local Tab (in-process)" → forces in-process even if default is daemon
- "New Tab on…" → submenu of configured remotes (see below)

## Remote config

Known remotes listed in TOML, surfaced in the UI menu + commands:
```toml
[[remote]]
name = "kh"
ssh  = "kh.zaxxon.cc"
color = "#ff5555"           # tab badge color (red for prod)
warn_destructive = true     # confirm dialog before sudo/rm-rf typed in this tab

[[remote]]
name = "xRyzen"
ssh  = "xero@xRyzen.local"
color = "#50fa7b"
```

`xerotty connect ssh://kh` resolves `kh` against this list first, then
falls back to a raw SSH host string.

## Headless installs

`xerottyd` is its own binary in `cmd/xerottyd/`. No GUI deps — links
neither SDL3 nor ImGui nor cairo. Just PTY + protocol + msgpack +
optional MCP server.

Distribution model:
- **dev/desktop machine**: install both `xerotty` and `xerottyd`
  (single repo, two build artifacts). User runs `xerotty`; daemon
  auto-spawns when needed; no other config required.
- **headless server**: install only `xerottyd`. Run via systemd user
  unit. No SDL3 install needed on the server. UI clients attach over
  SSH; AI agents connect to MCP via SSH-forwarded socket.
- **container**: `xerottyd` in a minimal image (statically linked or
  with just libc + pty). Mount the socket out for the host to attach.

**MCP clients** are equal-tier to UI clients. They speak a separate
JSON-RPC protocol on a separate socket, but they're driving the same
daemon. Per-tab modes (observe/propose/auto) control what writes they
can do.

## Wire protocol (UI ↔ daemon)

**Format: msgpack via `github.com/tinylib/msgp` (codegen).**

Rationale:
- msgpack: structured binary, ~3-10x smaller and ~5-10x faster
  encode/decode than JSON, native libs in every language so
  third-party clients (phone, web, language bindings for Xyphia)
  are cheap to write.
- tinylib/msgp's codegen: ~5-10x faster than reflection-based
  msgpack (vmihailenco) on hot paths, zero allocations per encode/
  decode, no runtime type lookup. Adds a `go generate` step which
  is wired into `build.sh` + `make` so it's invisible.
- Schema evolution: msgpack handles added/removed fields cleanly
  (decoders ignore unknown keys), so wire-format changes don't
  break old clients connecting to a new daemon.
- Debugging: standard `msgpack-cli` works for dumping frames.

Local transport via SHM for cell grid (zero-copy, daemon writes, UI
memory-reads) is an optional **Phase 5+ optimization** if profiling
shows the local cell-update path is bottlenecked. msgpack alone is
fast enough that it's probably never needed; SHM is in the back
pocket.

**Frame format**:
```
[u32 length (big-endian)][msgpack payload]
```

**Transport options**:
- Local: unix socket at `$XDG_RUNTIME_DIR/xerottyd.sock` (or
  `~/.cache/xerotty/sock` macOS).
- Remote: SSH-stdio. UI runs `ssh host xerottyd --stdio`; the daemon
  on the remote speaks the protocol on its stdin/stdout. No TCP
  listener, no separate auth, leverages `~/.ssh/config`.
- (Future) Remote over TCP+TLS for low-overhead persistent connections
  on trusted networks.

**Frame format**:
```
[u32 length][json payload]
```

Each message type is a Go struct with a `//go:generate msgp` directive
in the file. `build.sh` and `make` run `go generate ./...` before
build so generated `*_gen.go` encoders are always in sync with the
struct definitions — schema drift is impossible.

Payload is one of these message types (sketch — not final):

UI → daemon:
- `attach { session_id?, new? }` — connect to existing session or create
- `detach`
- `input_keys { events: [...] }` — keystroke events
- `input_paste { bytes: base64 }`
- `input_paste_image { mime: "image/png", bytes: base64 }`
- `resize { cols, rows, px_w, px_h }`
- `scroll { offset_lines }`
- `tab_create { initial_command? }`
- `tab_close { tab_id }`
- `tab_focus { tab_id }`
- `set_mode { tab_id, mode: "observe"|"propose"|"auto"|"manual" }`

daemon → UI:
- `attached { session_id, tabs: [...] }`
- `cell_diff { tab_id, cells: [ {row, col, content, style, width}... ] }`
- `cell_full { tab_id, rows: [[Cell...]...] }` — initial frame / resync
- `cursor { tab_id, row, col, visible, style }`
- `scrollback_append { tab_id, lines: [[Cell...]...] }`
- `bell { tab_id }`
- `title { tab_id, title }`
- `cwd { tab_id, path }`
- `child_exit { tab_id, exit_code }`
- `image_paste_available { tab_id, paste_id, mime, size, sha256 }`
  — daemon stores the binary; clients (AI via MCP) request it by id

**Backpressure**: bounded send queue per client. If a UI is slow,
daemon coalesces cell diffs (most recent state of each cell wins) and
keeps the latest cursor / cwd; never blocks the PTY reader.

## MCP socket (AI ↔ daemon)

Separate socket: `$XDG_RUNTIME_DIR/xerottyd-mcp.sock`. Standard
JSON-RPC 2.0 per MCP spec. Tools roughly:

**read tools**:
- `list_tabs(daemon_id?)` — id, title, host, cwd, mode, foreground proc
- `get_screen(tab_id)` — current visible cell grid
- `get_scrollback(tab_id, lines)` — recent N lines
- `get_selection(tab_id)` — what the user has selected
- `get_cwd(tab_id)`
- `get_foreground_proc(tab_id)`
- `get_recent_paste(tab_id)` — last image/text the user pasted in
  the UI; AI gets the binary directly, no escape sequences

**write tools** (gated by mode):
- `write(tab_id, text)` — raw type, no newline
- `run(tab_id, command)` — type + newline
- `send_key(tab_id, key)` — `Ctrl+C`, `Escape`, etc.
- `open_tab(daemon_id, command?)`
- `close_tab(tab_id)`
- `request_control(tab_id, reason)` — switches mode → `auto`,
  user gets a notification asking to approve

**multi-client coordination**: each connect carries a `client_id`.
Per-tab "current driver" lock. Many `observe` clients allowed, one
write-capable driver at a time. Handoff via `request_control`.

## Modes (from MCP_PLAN discussion)

Three modes per tab:
- 👁️ `observe` — AI can read, can't write
- 💭 `propose` — AI's writes queue as dim ghost-text on prompt; user
  Enter accepts, Esc rejects
- 🤖 `auto` — AI writes directly

Default for new tabs: `observe`. User upgrades via right-click menu or
hotkey. Mode is per-tab, stored in session state.

(No auto-gate / classifier mode — dodges the "what's safe" rabbit
hole.)

## Phased delivery

Each phase is a separately useful checkpoint. Don't move on until
the current phase works end-to-end.

### Phase 0 — protocol skeleton, local only
- New `cmd/xerottyd` and `internal/daemon` package.
- New `internal/protocol` package: message struct definitions with
  `//go:generate msgp` directives. Frame codec is length-prefixed
  msgpack via tinylib/msgp generated encoders.
- daemon listens on unix socket, accepts attach.
- Refactor `internal/terminal` so PTY ownership can live in the daemon
  (it mostly already can — minimal changes).
- xerotty UI grows a `--connect unix:///path/sock` flag that talks the
  protocol instead of using `internal/terminal` directly.
- Acceptance: `xerottyd` running locally, `xerotty --connect ...`
  shows a working shell tab, input + output works end-to-end.

### Phase 1 — multi-tab + detach/reattach
- Daemon manages tab list as protocol-addressable IDs.
- UI can `tab_create` / `tab_close` / `tab_focus`.
- Detaching UI doesn't kill daemon-side tabs.
- Reattaching gets a full `cell_full` frame for sync, then incremental.
- Acceptance: open 3 tabs, close UI, reopen UI, all 3 still running
  with their state intact.

### Phase 2 — SSH remote
- xerotty UI `--connect ssh://host` runs `ssh host xerottyd --stdio`.
- daemon's `--stdio` mode skips the unix listener and speaks protocol
  on stdin/stdout.
- Acceptance: run xerottyd on a remote box, attach from laptop, work
  in a shell as if local. Test image/clipboard paste (next phase).

### Phase 3 — structured paste + clipboard sync
- UI sends `input_paste_image` as binary frame.
- daemon stores the image; if foreground app understands Kitty / OSC
  1337, daemon synthesizes that escape; otherwise stores for `get_recent_paste`.
- Clipboard sync: daemon ↔ UI exchange clipboard contents via protocol
  frames instead of OSC 52.
- Acceptance: paste image in laptop UI while attached to remote daemon
  → image reaches Claude Code running on remote without escape-sequence
  acrobatics.

### Phase 4 — MCP
- New `internal/mcp` package wrapping a Go MCP server.
- daemon spins it up on `$XDG_RUNTIME_DIR/xerottyd-mcp.sock`.
- Read tools first; verify Claude Code can list tabs + read scrollback.
- Then write tools gated by mode.
- Mode UI: tab badge + right-click "Set mode: …" + hotkey cycle.
- Acceptance: tell Claude "list my running tabs and figure out which
  one has the failing build", it reads scrollback, identifies, asks
  for `request_control` on that tab, you approve, it runs commands.

### Phase 5 — multi-driver coordination
- Per-tab driver lock + handoff request flow.
- Xyphia connects as a second MCP client, can drive its own tabs.
- Acceptance: Claude on tab 1, Xyphia on tab 2, both working, both
  show up in UI with distinct icon/color.

### Phase 6 — host badges + ssh config integration
- UI reads `~/.ssh/config` for known hosts → `xerotty connect prod-db`.
- Per-host color/icon in tab bar so prod is visually obvious.
- "Are you sure?" gate for destructive commands typed into red-flagged
  tabs.

### Phase 7 — multi-attach
- Two UI clients on same daemon can attach to the same session.
- Read-only attach is easy; read-write needs cursor-sharing decisions.

### Phase 8 (optional) — SHM cell grid for local
- Only if profiling shows local cell-update traffic via msgpack is
  a real bottleneck.
- Daemon writes cell grid to a POSIX shm segment / memfd_create-
  backed buffer; UI memory-reads it directly on render.
- Control messages (input, resize, mode change) still flow over the
  msgpack socket.
- Fallback: any UI on a platform without shm (Windows? remote?)
  uses msgpack-frame cells. Daemon always serves both transports.
- Probably never needed — msgpack handles 60fps full-screen updates
  with room to spare.

## Open design questions

- **Wire format detail**: cell diffs vs row diffs? Probably cells with
  RLE for runs of identical style. Profile after Phase 1.
- **Reconnect resilience**: mosh-style state sync token? Or just always
  full-resync on attach (simpler, fine if cell grid + few hundred
  scrollback lines is small)?
- **Authentication on TCP transport** (future phase): mTLS? token?
  Restricted to LAN by default?
- **MCP transport when AI is on a different host than daemon**: SSH
  port-forward the MCP socket? Run MCP-over-TCP option?
- **Image paste lifetime**: how long does the daemon keep pasted
  images? LRU + size cap? Disk-back?
- **Session naming**: numeric IDs only, or human-readable session names
  (`xerotty attach build-farm`)?

## Notes on existing code

- `internal/terminal` already has the clean PTY + emulator structure
  needed. Daemon refactor mostly moves who CALLS `terminal.New`, not
  the implementation.
- `internal/scrollback` and `internal/terminal/diskscrollback.go` move
  into the daemon as-is.
- `internal/renderer` stays on the UI side (it's all glyph cache + GL).
- `internal/input` stays on the UI side (it's keyboard event translation
  to byte sequences, but the bytes go over the protocol now).
- `internal/menu` mostly stays UI but the menu items it can produce
  start gaining "AI mode" entries from Phase 4 onward.

## What this absorbs

By the end of Phase 6, xerotty does the work of:
- tmux (session persistence, detach/reattach, multi-window) — better,
  because it's structured not VT-stream-based
- mosh (long-lived remote sessions over flaky network) — protocol-level
  framing makes this easy to add a UDP transport later
- iTerm2's session restore — daemon never lost state in the first place
- a chunk of what people use Claude Code's shell-tool for — now the
  shell is observable and the AI is a real participant
- (eventually) AI orchestration platforms — Xyphia connects to N
  daemons across N hosts and runs distributed work via MCP
