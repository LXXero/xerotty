# xerotty daemon + thin UI + MCP

The architectural arc that split xerotty into a **headless daemon**
that owns terminal sessions and a **thin UI client** that attaches
to one or more daemons over a structured protocol — plus an **MCP
socket** so AI agents (Claude Code, Xyphia, custom orchestrators)
read/write sessions as first-class clients alongside the UI.

**Status: shipped on `spike/daemon`.** This doc is the design +
rationale; the "Phased delivery" section near the bottom tracks
exactly what landed. `docs/DAEMON_STATUS.md` is the
where-the-code-lives + how-to-verify companion.

## One binary, three roles

There is a SINGLE binary, `xerotty`, with subcommands — NOT a
separate `xerottyd`:

- `xerotty`          — GUI (default). Runs in-process PTY tabs, or
                       attaches to local + remote daemons.
- `xerotty serve`    — daemon mode: owns PTYs, the wire-protocol
                       socket, and the MCP socket. Headless (links
                       SDL3/ImGui but never opens a window in this
                       mode).
- `xerotty connect`  — CLI thin client; attaches to a daemon and
                       proxies the terminal you launched it from.

Socket *filenames* keep the `xerottyd.sock` / `xerottyd.mcp.sock`
names for path stability, but no `xerottyd` executable exists.
Don't reintroduce one.

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
│ xerotty GUI (one process per user)                       │
│   ├── tab attached to host A's local `xerotty serve`     │
│   ├── tab attached to host B's daemon over SSH           │
│   └── tab attached to host C's daemon over SSH           │
│                                                          │
│ also runs an aggregating MCP socket covering all of      │
│ the above (internal/guimcp) — one socket, host-          │
│ namespaced tab IDs ("local:5", "kh:3").                  │
└──────────────────────────────────────────────────────────┘
       │            │             │
       │ unix sock  │ ssh stdio   │ ssh stdio
       │            │             │
  ┌────▼──────┐ ┌───▼───────┐ ┌──▼────────┐
  │xerotty    │ │xerotty    │ │xerotty    │
  │serve A    │ │serve B    │ │serve C    │
  └────┬──────┘ └───┬───────┘ └──┬────────┘
       │            │             │
       │ per-daemon MCP socket    │
       │                          │
   ┌───▼──────────────────────────▼─────┐
   │ AI clients: Claude Code, Xyphia,   │
   │ custom scripts. Connect to a       │
   │ daemon's own MCP socket, or to the │
   │ GUI's aggregating socket for the   │
   │ whole multi-host view. observe/    │
   │ propose/auto modes per connection. │
   └────────────────────────────────────┘
```

**The daemon (`xerotty serve`) is headless.** It owns:
- PTYs and their child processes
- in-memory cell grid (per session/tab)
- scrollback (memory + disk in unlimited mode)
- tab/session list (per-daemon namespace)
- the wire protocol socket (UI clients)
- the MCP socket (AI clients)

It renders nothing in this mode — no window, no GL draw. For
server installs, `./build.sh headless` (`-tags headless`) builds
a binary that doesn't link SDL3/GL/ImGui/freetype/fontconfig at
all (~6.6M vs ~12M): `serve` + `connect` only, GUI default
stubbed. Same `xerotty serve` command surface — install the lean
artifact AS `xerotty` so the SSH bridge + auto-spawn stay
uniform. (Default build is still full-GUI; lean needs the
explicit tag.) Just PTY I/O, the vt state machine, file/socket
I/O.

**xerotty (the GUI)** owns:
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
- a local in-process PTY tab (fastest, simplest)
- a tab on the local daemon (persistent + MCP-accessible)
- a tab on `kh`'s daemon over SSH (remote shell, persistent)
- a tab on `xRyzen`'s daemon over SSH

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

// implementations (shipped):
//   *terminal.Terminal           — in-process PTY (internal/terminal)
//   *daemonsource.Source         — conn to a daemon + a shadow
//                                  vt.SafeEmulator that CellFull/
//                                  CellDiff frames patch
//                                  (internal/daemonsource)
```

UI doesn't care which. `tabs.Tab.Terminal` (typed `terminal.Source`)
is whichever was selected when the tab opened. The real interface
(`internal/terminal/source.go`) grew well past this sketch:
title/bell/clipboard callbacks, cursor style+visibility, scrollback
snapshots, image paste, etc.

## Lifecycle (model A, wezterm-style)

Daemon mode is **opt-in**, not all-or-nothing. The daemon is
**lazily auto-spawned** (`xerotty serve`, detached) when first
needed; users never manage the daemon lifecycle directly.

**Default behavior** (out of the box): new tabs are in-process
PTYs (`*terminal.Terminal`). Identical to pre-daemon xerotty.
Fast startup, no daemon involved.

**Opt-in via one config knob** (`internal/config`):
```toml
[tabs]
source = "daemon"          # "pty" (default) | "daemon" | "daemon:<host>"
daemon_socket = ""          # override; default $XDG_RUNTIME_DIR/xerottyd.sock
```

- `"daemon"` — local auto-spawned daemon.
- `"daemon:<host>"` — a remote host from `[[hosts]]` becomes the
  DEFAULT source; new tabs land on that box.

When daemon mode is on, the GUI auto-spawns `xerotty serve`
(`daemonsource.EnsureLocalDaemon`, Setpgid-detached so it
outlives the GUI) and the daily flow is identical (`xerotty`,
terminal opens, type into it) but now:
- closing the UI doesn't kill running processes — daemon survives
- reopening attaches to the same tabs, mid-scrollback intact
- another machine can SSH-attach to the SAME daemon and see the SAME
  tabs, work continues from wherever
- AI agents (Claude / Xyphia) can connect to the MCP socket whenever

**Remote tabs** go through SSH:
```sh
xerotty connect --ssh kh              # CLI client attached to kh's daemon
```
And from the GUI, host actions (`new_tab_remote:<host>` /
`attach_remote:<host>`, auto-populated into the right-click menu's
"Remote" submenu from `[[hosts]]`). Under the hood: SSH to the
host, run `xerotty serve --stdio`, which BRIDGES the SSH pipe to a
persistent daemon on that box (auto-spawning one if absent — see
`internal/runner/stdio_bridge.go`). Every protocol frame flows
over the SSH pipe. No new ports, no auth to configure — reuses
`~/.ssh/config`. Disconnecting kills only the bridge; the remote
daemon + its tabs survive.

`new_tab_remote:<host>` opens a fresh tab on the host;
`attach_remote:<host>` adopts the host's already-running tabs
(preserving its window layout + focus).

## Remote config

Known hosts listed in TOML (`[[hosts]]`), surfaced in the GUI's
"Remote" submenu + the `daemon:<host>` source mode:
```toml
[[hosts]]
name      = "kh"
ssh_dest  = "kh.zaxxon.cc"      # anything ssh(1) accepts
ssh_args  = ["-i", "~/.ssh/key"] # optional, before the dest
remote_cmd = ""                  # default "xerotty serve --stdio"
```

## Headless installs

Distribution model (single `xerotty` binary everywhere):
- **dev/desktop machine**: install `xerotty`. User runs `xerotty`;
  daemon auto-spawns via `xerotty serve` when needed; no other
  config required.
- **headless server**: install `xerotty`, run as
  `xerotty serve` (or via a systemd user unit running the same).
  UI clients attach over SSH via `xerotty connect --ssh host` or
  by adding the host to `[[hosts]]` in the GUI's config. AI
  agents connect to MCP via SSH-forwarded socket.
- **container**: `xerotty` in a minimal image. Mount the socket
  out for the host to attach.

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

**Frame format** (actual, `internal/protocol/codec.go`):
```
[u32 length (big-endian)][u8 MsgType][msgpack payload]
```
`maxFrameSize` is 16 MiB; image paste is chunked
(`MsgInputImageChunk`, 1 MiB chunks) so no single frame carries a
whole image. `ProtocolVersion` is 3; the Hello handshake rejects
mismatches outright (no silent half-compatibility).

**Transport options**:
- Local: unix socket at `$XDG_RUNTIME_DIR/xerottyd.sock`.
- Remote: SSH-stdio. `xerotty connect --ssh host` runs
  `ssh host xerotty serve --stdio`, which bridges to a persistent
  remote daemon (auto-spawn + bridge in
  `internal/runner/stdio_bridge.go`). No TCP listener, no separate
  auth, leverages `~/.ssh/config`.
- (Future) Remote over TCP+TLS for low-overhead persistent
  connections on trusted networks.

Each message type is a Go struct with a `//go:generate msgp`
directive in `messages.go`. `build.sh` and `make` run
`go generate ./...` before build so generated `*_gen.go` encoders
stay in sync with the structs — schema drift is impossible.

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

Two layers, both line-delimited JSON-RPC 2.0 speaking the MCP
shape (`initialize` / `tools/list` / `tools/call`, native methods
also reachable for `nc -U` debugging):

- **Per-daemon** (`internal/mcp`): each `xerotty serve` exposes
  `$XDG_RUNTIME_DIR/xerottyd.mcp.sock` covering THAT daemon's tabs
  (uint32 IDs).
- **GUI aggregating** (`internal/guimcp`): the GUI runs ONE socket
  (`$XDG_RUNTIME_DIR/xerotty-gui.mcp.sock`) covering every daemon
  it's attached to (local + remotes), with host-namespaced string
  IDs (`"local:5"`, `"kh:3"`). Ungated — it's the user's own
  trusted GUI process. Reads/writes go through the same
  `daemonsource.Source` the GUI renders from.

Tools (per-daemon server; the aggregating server exposes the
read/write subset with namespaced IDs):

**read tools**:
- `list_tabs(daemon_id?)` — id, title, host, cwd, mode, foreground proc
- `get_screen(tab_id)` — current visible cell grid
- `get_scrollback(tab_id, lines)` — recent N lines
- `get_selection(tab_id)` — what the user has selected
- `get_cwd(tab_id)`
- `get_foreground_proc(tab_id)`

Shipped tool names: `list_tabs`, `get_screen`, `get_scrollback`,
`get_clipboard`, `send_input`, `send_paste`, `create_tab`,
`close_tab`, `resize_tab`, `list_proposals`, `approve_proposal`,
`drop_proposal`, `set_agent_mode`, `authenticate`, `list_clients`,
`get_server_info`. (The aggregating GUI server exposes the
read/write subset over namespaced IDs.)

## Modes + trust boundary

Mode is **per-connection** (not per-tab — simpler, and matches how
agents actually attach):
- `observe` — read only; writes return an error.
- `propose` — writes QUEUE on the daemon (`Session.proposed`,
  bounded). They don't apply until approved.
- `auto` — writes apply directly.

No auto-gate / classifier mode (dodges the "what's safe" rabbit
hole).

**Propose is a real gate, two ways to consume it:**
- *GUI banner* — the daemon broadcasts the queue
  (`MsgProposalsChanged`) to wire clients; the GUI renders an
  "Agent proposals" overlay with per-item Approve/Drop, scoped to
  the originating hub. Resolve goes back via `MsgProposalResolve`.
- *Programmatic* — `list_proposals` / `approve_proposal` /
  `drop_proposal` MCP tools.

**Authority**: `[mcp]` config gates self-elevation —
```toml
[mcp]
default_mode      = "observe"   # connection start mode
allow_mode_change = true        # false → can't elevate own mode
approval_token    = ""          # reviewer presents via agent/authenticate
```
With `allow_mode_change=false`, a propose-mode agent can't promote
itself to auto or approve its own writes; only a connection that
`authenticate`d with `approval_token` (or is already auto) can
approve. That's how propose becomes a human (or distinct-authority)
gate rather than honor-system.

**Multi-client**: input is last-writer-wins per tab (no hard
driver lock). Many readers fine. `list_clients` shows who's
attached.

## Phased delivery

Each phase is a separately useful checkpoint. Don't move on until
the current phase works end-to-end.

### Phase 0 — protocol skeleton, local only [SHIPPED]
- `internal/runner/serve.go` (invoked as `xerotty serve`) and
  `internal/daemon` package.
- `internal/protocol` package: message struct definitions with
  `//go:generate msgp` directives. Frame codec is length-prefixed
  msgpack via tinylib/msgp generated encoders.
- daemon listens on unix socket, accepts attach.
- `internal/terminal` refactored so PTY ownership lives in the
  daemon (the in-process GUI uses the same code path through
  `terminal.Source`).
- xerotty UI flips between in-process and daemon-backed tabs via
  `cfg.Tabs.Source = "daemon"` / `"daemon:<name>"`.
- Acceptance: `xerotty serve` running locally, `xerotty connect`
  shows a working shell tab.

### Phase 1 — multi-tab + detach/reattach [SHIPPED]
- Daemon manages tab list as protocol-addressable IDs.
- UI can tab create / close / focus / move.
- Detaching UI doesn't kill daemon-side tabs.
- Reattaching gets a full cell frame + partial scrollback
  backfill.
- Acceptance: open 3 tabs, close UI, reopen UI, all 3 still
  running with their state intact.

### Phase 2 — SSH remote [SHIPPED]
- `xerotty connect --ssh host` runs `ssh host xerotty serve
  --stdio`, which BRIDGES to a persistent remote daemon
  (auto-spawning one if absent). SSH session ending doesn't
  kill remote tabs.
- daemon's `--stdio` flag now means "bridge stdio to the
  persistent daemon socket"; the old "ephemeral in-process
  daemon" mode is `--stdio-ephemeral` for scripted one-offs.
- Acceptance: run `xerotty serve` on a remote box (or let SSH
  bridge auto-spawn it), attach from laptop, work in a shell
  as if local. Disconnect, reattach later, tabs still there.

### Phase 3 — structured paste + clipboard sync [SHIPPED]
- Image paste as binary frames, chunked (`MsgInputImageChunk`).
  Daemon writes the bytes to a temp file on ITS filesystem and
  types the path into the PTY — so Claude Code on a remote box
  reads it natively, no Kitty/OSC1337/base64 acrobatics.
- Clipboard sync via protocol frames AND OSC 52: `MsgClipboardData`
  (client→daemon, for OSC 52 reads), `MsgClipboardSet`
  (daemon→client, when a PTY app OSC-52-writes the clipboard).
  The GUI polls the OS clipboard on a slow timer so remote OSC 52
  GET returns live data, not just what xerotty itself copied.

### Phase 4 — MCP [SHIPPED]
- `internal/mcp` per-daemon server on `xerottyd.mcp.sock`;
  `internal/guimcp` aggregating server on `xerotty-gui.mcp.sock`.
- Full read + write tool set (see above), write tools gated by
  per-connection mode.
- Mode UI: propose-queue approval banner + tab host badges.

### Phase 5 — multi-driver coordination [SHIPPED, simplified]
- Multiple MCP clients + multiple wire clients on one daemon.
- Input is last-writer-wins (no hard driver lock — deliberately
  simpler than the originally-planned lock/handoff). `list_clients`
  surfaces who's attached.

### Phase 6 — host badges + remote integration [SHIPPED]
- `[[hosts]]` registry; GUI "Remote" submenu auto-populated;
  `daemon:<host>` default-source mode; `--ssh` for the CLI client.
- Tabs carry a `Host` field; the tab title shows a "host: " badge.
- (Per-host color + destructive-command gate: not done; future.)

### Phase 7 — multi-attach [SHIPPED]
- Multiple clients attach to the same daemon/session; cell frames
  fan out per client. Reattach restores window layout + focus.
  Cursor-sharing for simultaneous read-WRITE is still
  last-writer-wins, not collaborative.

### Phase 8 (optional) — SHM cell grid for local [NOT DONE]
- Only if profiling shows local cell-update traffic via msgpack is
  a real bottleneck. Hasn't been; msgpack handles 60fps full-screen
  updates with room to spare. In the back pocket.

### Phase 9 — topology broadcast + request correlation [SHIPPED]

Implementation notes (as built):
- `ProtocolVersion` bumped to 4. `TabCreate`/`TabCreated` carry `ReqID`;
  `Attached` carries `Revision`; new `MsgTopologyChanged`.
- Daemon funnel `CreateTab/CloseTab/MoveTab/CreateWindow/CloseWindow`
  bumps `Session.revision` and broadcasts `TopologySnapshot()` to every
  client of the session; wire handlers AND the MCP server route through
  it. Clients gate on revision (seeded from `Attached.Revision`).
- `Hub.NewTabIn` correlates acks by `ReqID` (router-demuxed, so
  concurrent creates don't steal each other's acks; late acks dropped);
  `Hub.applyTopology` reconciles adopted Sources (adopt new, mark
  vanished, revision-gate).
- M2: the per-tab publish loop stays alive after child exit (stops its
  state ticker) so a scrollback clear on a held/exited tab still
  reaches other clients.
- Deferred: live GUI window/tab rendering of remotely-driven topology
  changes (the Hub reconciles + `SetTopologyCallback` is wired, but the
  app doesn't yet add/remove tab UIs from the callback — see report).

Original design follows.

Structural changes to a session aren't propagated to other attached
clients. `MsgTabCreated` goes only to the requesting client;
MCP-created/closed tabs emit no wire event; close/move notify only the
caller. With one GUI per daemon this never showed — but the MCP /
multi-agent workflows now create tabs that a live GUI (or a second
client) never sees until reconnect. Two related defects fall out of
the same gap (both found in review):

- **No request correlation** — `Hub.NewTabIn` waits on the next
  `TabCreated` with no request ID, so a *late* ack after a timeout
  poisons the next create (adopts the wrong tab). `hub.go`.
- **Scrollback clear lost for exited tabs** — `broadcastScrollbackCleared`
  only wakes a tab's publish loop, which has already returned once the
  tab exited; held/exited tabs never deliver the clear. `daemon.go`.

Design — **snapshot + revision, not deltas**, so clients self-heal and
there's no delta-ordering bug class (the failure mode that bit the
Phase-7 fixes repeatedly):

- **Request correlation**: add a `ReqID` to the tab-create request,
  echo it in `MsgTabCreated`. `Hub.NewTabIn` matches on `ReqID` and
  drops unmatched/late acks. Closes the poisoning bug; also lets
  multiple in-flight creates coexist.
- **`MsgTopologyChanged{SessionName, Revision, Windows, Tabs,
  FocusedTabID}`**: monotonic per-session `Revision`; broadcast to
  *every* attached client whenever structure changes. A client applies
  it only if `Revision` is newer, replacing its tab/window view
  (idempotent — no ordering assumptions).
- **Daemon mutation helpers** `createTab / closeTab / moveTab /
  createWindow / closeWindow`: the single funnel that mutates the
  Session, bumps `Revision`, and broadcasts the snapshot. Both the wire
  handlers and the MCP server route through these instead of poking
  `Session` directly — so MCP-driven changes broadcast too.
- **Client reconcile**: the `Hub` applies a snapshot by diffing against
  its adopted tabs — adopt new IDs, drop vanished ones, update focus —
  reusing the existing attach-time adoption path.
- **Scrollback-clear (M2)**: fold delivery into the broadcast path (or
  a subscriber-level pending flag the client reads next frame) so a
  clear on a held/exited tab still reaches other clients.

Tests: client A create/close/move visible to client B; MCP create
visible to a wire client; a late `TabCreated` after timeout is ignored;
clear-scrollback on a held/exited tab notifies other clients.
Multi-client is now directly exercisable (two GUI windows on one
daemon, or MCP mutating while a GUI watches).

### Phase 10 — connection resilience: dead/hung/slept clients [SHIPPED · live-validated]

A client connection can go **half-open** with no signal to the daemon:
a laptop attached over SSH suspends, a link drops, a client stops
reading. Writes silently succeed into the kernel send buffer until it
fills, then block — and the OS won't error the socket for minutes (TCP
retransmit timeout) or never (a stdio pipe over dead SSH). Today the
daemon can't tell a live client from a vanished one, and structural
broadcasts run on the mutating goroutine, so one stuck client can stall
the daemon.

Two facts shape the design: (1) `internal/protocol/stdioconn.go`'s
`SetRead/WriteDeadline` are no-ops, so socket timeouts DON'T work over
the SSH-stdio transport — the main remote case; (2) the daemon persists
sessions and Phase 9 gives snapshot+revision resync, so **a connection
is disposable** — reaping a dead client is safe; it reconnects and
re-syncs with zero loss. So the philosophy is: don't fight to preserve
a stalled connection — detect it fast, reap it, let reconnect heal.

Layered, in dependency order:

1. **Per-client async bounded writers (prerequisite).** No broadcast
   writes on a mutating goroutine. Each `clientConn` gets an outbound
   writer goroutine + bounded queue; mutators enqueue and move on.
   Topology is **latest-wins / coalesced** (a single pending-snapshot
   slot); cell frames drop/coalesce to the next full/diff. Without
   this, heartbeat doesn't help — the mutator still blocks in `Write`.
2. **App-level heartbeat** — `Ping{nonce}` / `Pong{nonce}` control
   frames. The daemon pings ~every 5s and reaps a client after ~3
   missed pongs or ~15–20s with no inbound traffic (any inbound frame
   counts as liveness). The client pings the daemon too, for fast GUI
   "reconnecting" detection. This is the ONLY liveness detector that
   works over the SSH-stdio transport.
3. **Progress-based write deadlines (unix-socket transport).**
   Heartbeat detects liveness; a deadline is what unblocks a stuck
   writer goroutine. Use an idle-progress timeout: refresh a ~5s
   deadline after each *successful partial write*, kill only if no
   bytes move for the window — so a big-but-flowing write (a large
   paste) is never killed, only a genuinely stalled one. No-op over
   stdio (heartbeat carries detection there).
4. **Reconnect UX.** Mark sources "reconnecting", freeze the last
   render, never block the UI on a daemon RPC. **Local close removes
   the GUI tab/window immediately** (no round-trip to a dead daemon).
   Pitfall (codex): decide whether local-close means "hide locally" or
   "kill the remote tab" — if kill, persist a **close tombstone** to
   replay after reconnect, or the snapshot resync will **resurrect the
   closed tab**.
5. **SSH `ServerAliveInterval=10` / `ServerAliveCountMax=3`** — a cheap
   extra signal on the SSH transport, but NOT the correctness
   mechanism: it won't protect synchronous daemon broadcasts, and the
   app-level heartbeat + bounded writers are still required.

Supersedes the "broadcast back-pressure" open-question — layer 1 is
that fix, generalized.

**As-built status** (all SHIPPED + pushed on `spike/daemon`; `-race`
clean, GUI + headless build):

- **Layers 1-3: SHIPPED.** `0e16340` (layer 1 — per-client async
  bounded writers), `c180d4d` (layer 2 — app-level heartbeat),
  `5d81dfc` (layer 3 — progress-based write deadlines).
- **Heartbeat corrections: SHIPPED** (`2b72279`) — fixed three review
  findings (codex): (1) don't false-reap a slow-but-flowing reader whose
  pongs sit behind the outbound backlog — gate reap on writer progress;
  (2) track ping↔pong staleness, not generic inbound; (3) bound the
  pre-writer handshake so a hung handshake leaves no zombie.
- **Layer 4: SHIPPED.** `c20cc31` (4a — async bounded client writer,
  non-blocking UI), `9fd8706` (4b — self-healing hub: `Hub.conn` is an
  atomic pointer the reconnect loop swaps, freezing the same Sources and
  resyncing from the new snapshot, + "reconnecting…" dim/badge overlay),
  `759418a` (4c — close tombstones), `c85981d` (4d — client→daemon
  heartbeat).
- **Layer 5: SHIPPED.** `d2e2555` — SSH `ServerAliveInterval=10`/
  `ServerAliveCountMax=3` (placed after user args so they can override).
- **L4/L5 review fixes (codex): SHIPPED.** `2cce16b` (#1 HIGH —
  close-tombstones scoped to a per-process daemon identity:
  `Attached.InstanceID` nonce, ProtocolVersion 6→7, so a daemon restart
  drops stale tombstones and a reused tab ID isn't suppressed forever),
  `cd248f3` (#2/#3 — client dual-clock heartbeat + out-of-band Pong),
  `ecf9301` (#4 — gate PasteImage/ClearScrollback on the reconnect
  freeze).

**Bugs LIVE testing caught that static review + unit tests structurally
could NOT — all fixed.** They only manifest against a real socket that
buffers, or a real daemon process dying under a live GUI:

1. **Hung daemon never detected** (`63a6107`). The client reaper's
   dual-clock gated on `lastWriteProgress`, which our OWN 5s heartbeat
   pings kept refreshing — the kernel accepts the tiny ping into the
   send buffer even when the peer is comatose — so a `kill -STOP`'d
   daemon was never reaped and no reconnect fired. Fix: client reaps on
   `noPong && noInbound` (inbound at read-byte granularity); drop
   write-progress from the *client* reaper (the client writes too little
   for it to say anything about the daemon).
2. **Idle-zombie client never reaped** (`ba439c3`) — the symmetric bug
   daemon-side: on an idle session the daemon's own pings self-refreshed
   its write-progress, so a hung idle client lived forever. Fix: exclude
   Ping/Pong from `lastWriteProgress`.
3. **A daemon restart silently quit the whole GUI** — the reseat saga.
   Kill the local daemon → reconnect to the fresh (empty) daemon → every
   Source marked vanished → tabs force-removed → last window empties →
   the "last window closed = quit" path fires → clean `os.Exit` (no
   panic, no core — looked like a crash, was the app quitting itself).
   Fixed over three codex rounds: `c3a5a0e` (reseat a fresh tab instead
   of quitting), `aea352c` (reseat only on a REAL restart — InstanceID
   change, not a remote tab-close — + mint the reseat window once),
   `f9cc93c` (restart resync stops matching old Sources by reused tab ID;
   mint scoped to the daemon instance for the double-restart case),
   `763dd49` (gate NewTab on a window minted for the CURRENT instance +
   tighten the resync test). Each fix carries a teeth-verified regression
   test (proven to fail on the pre-fix path).

**Live validation** (local, hands-on — not headless-testable): `kill
-STOP` daemon → GUI freezes + "reconnecting…" badge within the liveness
window → `-CONT` → resyncs in place (verified via the daemon's re-attach
timestamp vs GUI start time). 3-min idle soak → no false reap. `kill -9`
daemon → GUI survives + reseats a working interactive tab (the original
crash). **Remote `daemon:<host>` / SSH-keepalive (L5) drop NOT yet tested
live** — needs a fleet box up.

**Related, shipped this arc (not Phase 10):** event-driven render loop
(`a567a01`, reviewed). The idle GUI parks in `WaitEventTimeout` at ~0%
CPU (was ~30-35% busy-looping at the frame cap); renders only on SDL
events / PTY+daemon wakes / a few "settle" frames after input, idle wait
bounded by the cursor-blink interval. Verified live (~35% → ~4% idle).
Codex review found one Low — a sub-ms blink interval floored to 0 and
fell back to the 1s safety net, freezing the cursor up to a second —
fixed by clamping to a 1ms floor (`23971ba`). Also shipped nearby:
tab-bar Enter-activation fix and daemon-scrollback copy fix. Plus
unrelated GUI work: window opacity re-wired (`d7d9321` — the SDL2→SDL3
migration had dropped the `SDL_SetWindowOpacity` call) + a
`toggle_opacity` action/keybind/menu (`7900c6d`, `2cc6392`) for
screenshot-safe opaque snapping.

## Beyond the original phases (also shipped)

- **terminal.Source interface** — the GUI multiplexer abstraction;
  in-process PTY + daemon both implement it, so a window mixes
  local + remote tabs transparently.
- **Cursor fidelity** — DECTCEM visibility + DECSCUSR shape/blink
  carried over the wire (`MsgCursor` Blink/StyleSet), normalized
  on the vt enum, honored by the renderer over config default.
- **Bell** — `MsgBell` fan-out → tab-bar urgency marker.
- **Scrollback streaming** — `MsgScrollbackAppend` (daemon runs
  unlimited+disk so absolute indices stay stable) +
  `MsgScrollbackCleared` broadcast on clear.
- **Hub reconnect** — dead daemon connections (SSH drop, restart)
  are detected + re-dialed transparently.
- **-race clean** across all daemon-related packages.

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
- **Broadcast back-pressure** (Phase 9 follow-up): `broadcastTopology`
  and the other fan-out broadcasts run synchronously on the caller's
  goroutine iterating clients, so one slow/blocked client can
  back-pressure the mutating caller. Fine at current client counts;
  revisit (per-client async send queue?) if many clients attach to one
  daemon. Same applies to cross-window-move GUI reconcile, which is
  implemented but not covered by the headless suite (interactive-path
  only).

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
