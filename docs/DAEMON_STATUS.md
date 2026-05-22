# Daemon status (historical Phase 0 doc — see HEAD note below)

> **HEAD UPDATE**: This doc was written when the daemon was a
> separate `./xerottyd` binary. That layout was collapsed shortly
> after — there's only ONE binary now (`./xerotty`) with
> subcommands:
>
> - `xerotty`            — GUI (default)
> - `xerotty serve`      — daemon (was `xerottyd`)
> - `xerotty connect`    — CLI thin client
>
> Everything in this doc that says "xerottyd binary" actually
> means "xerotty serve". Socket FILENAMES still use the
> `xerottyd.sock` name (so existing scripts that hardcode the
> path keep working) but no separate executable exists. Don't
> split the binary back out.

What's working on `spike/daemon` as of commit `3a44d7e`. What's
next. Read this when picking the work back up.

## How to verify it works

```sh
git checkout spike/daemon
./build.sh
```

Produces `./xerotty`. `xerotty serve` is the daemon mode;
`xerotty connect` is the CLI thin client. No separate binaries.

**End-to-end integration test** (the actual proof Phase 0 works):

```sh
go test ./internal/daemon/ -run TestDaemonRoundTrip -v
```

This spawns xerottyd in-process on a temp socket, connects via the
clientproto client, attaches to the default session, sends `echo
XEROTTY_PHASE0_OK\r` to a tab's PTY, and asserts the marker shows
up in the cell grid the daemon ships back. Passes in ~50ms.

**Manual smoke test**: launch the daemon, see it listen, kill it,
see it clean up its socket.

```sh
./xerottyd
# stderr: xerottyd: listening on /run/user/1000/xerottyd.sock
# stdout: /run/user/1000/xerottyd.sock
ls -la /run/user/1000/xerottyd.sock   # 0600 unix socket
# Ctrl+C
ls /run/user/1000/xerottyd.sock        # gone
```

## What's in place

### `internal/protocol/`
- Wire format: length-prefixed (u32 BE) frames, each carrying a
  MsgType byte + msgpack body.
- 16 message types covering Hello/HelloAck, Attach/Attached,
  TabCreate/TabClose/TabFocus/TabCreated, Resize, InputBytes/
  InputPaste, CellFull, CellDiff (struct defined, daemon doesn't
  emit yet), Cursor, Title, Bell, ChildExit, Error.
- Style bit-packing (`PackStyle`/`UnpackStyle`) — 7 attr flags +
  3-bit underline + per-channel palette/RGB with sentinel bits.
- Codegen via `//go:generate msgp` in `messages.go`. Generated
  files live next to source as `*_gen.go` (checked in; regenerated
  every `build.sh` run).
- Tests: full round-trip for every message type (auto-generated
  by msgp) + `codec_test.go` covering frame I/O + style packing.

### `internal/daemon/`
- `Daemon` — unix-socket listener, accepts client connections,
  refuses to start if another live daemon owns the socket, removes
  stale sockets on first start, graceful shutdown.
- `Session` — collection of tabs. Phase 0 only ever has one
  named "default". Tabs spawn real PTYs via `internal/terminal`.
- `clientConn` — per-connection handler. Hello handshake (version-
  checked), Attach dispatch, per-tab publish goroutine that wakes
  on `terminal.DataCh` and ships a `CellFull` frame.
- `cell_convert.go` — `ultraviolet.Cell` → `protocol.Cell`
  including the palette/RGB color classification.

### `cmd/xerottyd/`
- Entry point. `--socket /path` flag, defaults to
  `$XDG_RUNTIME_DIR/xerottyd.sock`. Stdio mode flag exists but
  refuses to run (Phase 2 SSH transport will implement it).

### `internal/clientproto/`
- `Client` wrapping a single daemon connection. Hello, Attach,
  SendTabCreate/Close/Focus, SendResize, SendInput/Paste. Async
  read loop dispatches incoming frames to per-type channels:
  CellFull, CellDiff, Cursor, Title, Bell, ChildExit, TabCreated,
  Attached, Errors. Closed channel + ExitErr for clean shutdown.

### Build wiring
- `build.sh` and `Makefile` both: install msgp if missing, run
  `go generate ./...`, build BOTH `xerotty` and `xerottyd`.
- `CLAUDE.md` updated with the spike-branch status so future
  agent sessions land oriented.

## What's NOT done

### Phase 0.5 — UI integration
Right now the UI (`./xerotty`) has zero knowledge of the daemon.
The user has to write their own client or use the integration
test to interact with the daemon.

To wire the UI:
1. Define `terminal.Source` interface (Resize, Write, CellAt,
   Cursor, DataCh, etc.) — the methods `tabs.Tab.Terminal`
   already calls.
2. Make `*terminal.Terminal` formally implement it (already does
   in fact, just needs the formal interface declaration).
3. New `internal/terminal/remote_source.go`: `RemoteSource` that
   implements `Source` by proxying to a `*clientproto.Client` +
   maintaining a local cell-grid mirror that the CellFull/CellDiff
   handlers patch.
4. Change `tabs.Tab.Terminal` from `*terminal.Terminal` to
   `terminal.Source`. Update all read sites (renderer, selection,
   menu context, etc).
5. Add `xerotty --connect unix:///path/sock` flag. When set, the
   initial tab uses a `RemoteSource` instead of a local PTY.
6. Add `[startup] default_tab_source = "daemon"` config plumbing
   so opt-in to "every tab via daemon" is one knob away.
7. Add auto-spawn — if `connect` target doesn't have a live
   daemon, fork `xerottyd` as a child process and connect once
   it's listening.

### Phase 1 — cell diffs
Daemon currently ships `CellFull` (entire grid) every time the
terminal produces output. Phase 1 swaps in `CellDiff` — only the
cells that changed since the last frame — for bandwidth efficiency.
Wire format already supports it; daemon side needs:
- a per-(client, tab) "last shipped" grid mirror
- a `compareGrids` pass that produces the diff
- CellFull as the initial-attach / resync path only

### Phases 2-8
Per `docs/DAEMON_PLAN.md`. SSH transport, paste/clipboard
structured frames, MCP socket, multi-driver coordination, host
badges, multi-attach, optional SHM cell grid.

## Things to be aware of

- The protocol assumes one client at a time per tab for input
  (last-writer-wins). Multi-client coordination (request/grant
  control) is Phase 5.
- Daemon process owns PTYs — kill the daemon and child shells
  die with it. Detach (close UI, keep daemon) is the persistence
  story.
- Test relies on `/dev/ptmx` being available. CI without PTY
  access will need a skip.
- `protocol.Cell.Content` is a string (grapheme cluster), not a
  rune. Don't try to compare against a `rune` literal.
- `cell_convert.go` has TODO ground for true-color colors: the
  current implementation walks the `ansi.{Basic,Indexed,True}Color`
  type tree. If the upstream `ultraviolet` adds new color types,
  this needs an update; the default branch RGB-encodes anything
  unknown so it'll still render correctly, just less efficiently.
