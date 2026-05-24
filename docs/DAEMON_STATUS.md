# Daemon — current state + where the code lives

The daemon arc is **shipped on `spike/daemon`**. This is the
orientation doc: what exists, where, how to verify. `DAEMON_PLAN.md`
is the design + rationale + phase history.

## The shape

One binary, `xerotty`, three roles (subcommand on argv[1]):

- `xerotty`          — GUI (default).
- `xerotty serve`    — headless daemon (PTYs + wire socket + MCP).
- `xerotty connect`  — CLI thin client.

No separate `xerottyd` executable; socket filenames keep the
`xerottyd.sock` / `xerottyd.mcp.sock` names for path stability.

Two protocols, two sockets:
- **Wire** (GUI/CLI ↔ daemon): msgpack, `internal/protocol`.
  `[u32 len BE][u8 type][msgpack body]`, `ProtocolVersion = 3`.
- **MCP** (agents ↔ daemon, GUI ↔ agents): line-delimited
  JSON-RPC 2.0, `internal/mcp` + `internal/guimcp`.

## Verify it

```sh
./build.sh                       # builds ./xerotty (runs go generate for msgp)
go test -race ./internal/daemon/ ./internal/daemonsource/ \
  ./internal/clientproto/ ./internal/protocol/ ./internal/mcp/ \
  ./internal/terminal/         # all green
```

Key end-to-end tests:
- `daemon.TestDaemonRoundTrip` — spawn daemon, attach, echo a
  marker through a PTY, assert it lands in the shipped cell grid.
- `daemonsource.TestReattachRestoresTabsAndScrollback` — detach,
  reattach, tabs + scrollback survive.
- `daemonsource.TestChunkedImagePaste`, `TestOSC52ClipboardSet`,
  `TestProposalGateWire`, `TestScrollbackOrderUnderRotation`,
  `TestViewportConsistencyUnderBurst`.
- `mcp.TestMCPStandardProtocol`, `TestMCPTrustBoundary`.
- `terminal.TestCursorStyleDECSCUSR`.

Try daemon mode live: put `source = "daemon"` under `[tabs]` in
your config, launch `xerotty`. It auto-spawns `xerotty serve`
(detached), and tabs now survive closing the GUI.

## Where the code lives

- `internal/protocol/` — wire format. Message structs +
  `//go:generate msgp` (generated `*_gen.go` checked in), frame
  codec, `PackStyle`/`UnpackStyle` (attrs + underline + per-channel
  palette/RGB + fg/bg "set" bits for ANSI-black-vs-default).
- `internal/daemon/` — `Daemon` (socket listener, client registry,
  broadcast helpers), `Session` (tabs + windows + clipboard +
  propose queue), `clientConn` (per-connection dispatch + publish
  loop), `cell_convert.go`.
- `internal/runner/` — `serve.go` (`xerotty serve`), `connect.go`
  (`xerotty connect`, incl. `--ssh`), `stdio_bridge.go`
  (`--stdio` bridges to / auto-spawns a persistent daemon).
- `internal/clientproto/` — client side of the wire protocol;
  per-message-type channels; `DialSSH` / `DialCommand`.
- `internal/daemonsource/` — `Hub` (one per daemon connection,
  demuxes frames to per-tab `Source`s, reconnect-aware) and
  `Source` (terminal.Source backed by a shadow vt.SafeEmulator).
  `EnsureLocalDaemon` does the GUI's auto-spawn.
- `internal/terminal/source.go` — the `Source` interface; both
  `*terminal.Terminal` (in-process PTY) and `daemonsource.Source`
  implement it.
- `internal/mcp/` — per-daemon JSON-RPC/MCP server + trust model
  (`default_mode` / `allow_mode_change` / `approval_token` /
  `agent/authenticate`).
- `internal/guimcp/` — GUI's aggregating MCP server: one socket,
  host-namespaced tab IDs across all hubs.
- `internal/app/` — GUI integration: per-window source factories,
  daemon-window mapping, reattach/adopt, propose-gate banner,
  clipboard polling, remote host actions.

## Known limitations (deliberate / future)

- Multi-attach input is last-writer-wins, not collaborative
  cursor-sharing.
- `xerotty serve` links SDL3/ImGui even though it never renders;
  a build tag to strip them for minimal headless installs isn't
  done.
- `guimcp` is ungated (it's the user's own trusted GUI process)
  and covers daemon-backed tabs only — which is all tabs in
  daemon mode; pure in-process PTY mode has no MCP by design.
- SHM local cell-grid transport (Plan's Phase 8) not built —
  msgpack is fast enough.
- `cell_convert.go` walks the `ansi.{Basic,Indexed,True}Color`
  type tree; new upstream color types RGB-encode via the default
  branch (correct, just less compact).
- Tests need `/dev/ptmx`; PTY-less CI must skip.
