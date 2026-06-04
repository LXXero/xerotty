# MCP — driving xerotty from AI agents

xerotty exposes terminal sessions to MCP clients (Claude Code, custom
orchestrators, anything that speaks the Model Context Protocol). An
agent can list tabs, read screens and scrollback, type input, paste,
and spawn tabs — on the local machine and on every remote host the
GUI is attached to.

## Quick start

MCP clients spawn a command and speak JSON-RPC over its stdin/stdout.
`xerotty mcp` is that command — a bridge to the unix sockets the MCP
servers actually live on. One-time setup with Claude Code:

```sh
claude mcp add xerotty -- xerotty mcp
```

That's it. The agent now sees `list_tabs`, `get_screen`,
`send_input`, etc. as native tools. Works with the GUI running
(aggregated view of all hosts) or against a bare `xerotty serve` on
a headless box — the bridge tries the GUI socket first and falls
back to the local daemon's.

For a daemon on another machine, run the bridge over ssh:

```sh
claude mcp add xerotty-kh -- ssh kh xerotty mcp --daemon
```

## The two servers

| server | socket (default) | scope | write gating |
|---|---|---|---|
| GUI aggregator | `$XDG_RUNTIME_DIR/xerotty-gui.mcp.sock` | every tab on every connected daemon, IDs namespaced `<host>:<tabid>` (`local:1`, `kh:3`) | none — it's the user's own GUI process |
| per-daemon | `$XDG_RUNTIME_DIR/xerottyd.mcp.sock` (alongside the daemon's wire socket, `.mcp.sock` suffix) | that daemon's tabs only | trust model below |

Both speak line-delimited JSON-RPC 2.0 with the standard MCP shape
(`initialize` / `tools/list` / `tools/call`).

## `xerotty mcp` flags

```
xerotty mcp                  discover the GUI aggregator, then the local daemon
xerotty mcp --daemon         local daemon only (skip the GUI socket)
xerotty mcp --socket PATH    explicit socket (e.g. a daemon started with --socket)
```

Stderr carries diagnostics (which socket it bridged to); stdout is
exclusively the JSON-RPC stream.

### How discovery works

Servers **record** where they actually bound (a pathfile under
`os.UserCacheDir()/xerotty/` — `~/.cache` on Linux,
`~/Library/Caches` on macOS), and the bridge dial-verifies
candidates in order:

1. `--socket PATH` (explicit — no fallback)
2. recorded GUI MCP socket
3. default GUI MCP socket
4. recorded daemon MCP socket
5. `tabs.daemon_socket` from config (`.mcp.sock` derived)
6. default daemon MCP socket

Recordings exist because computed defaults can't work on macOS:
without `XDG_RUNTIME_DIR` the temp dir comes from `$TMPDIR`, which
differs per launch context — a Finder-launched GUI and an
agent-spawned bridge would compute different paths. Defaults without
XDG now live in a stable `/tmp/xerotty-<uid>/` (0700) for the same
reason. Stale recordings are harmless: every candidate is dialed
before use.

## Tools

GUI aggregator: `list_tabs`, `get_screen`, `get_scrollback`,
`send_input`, `send_paste`. `list_tabs` returns per-tab metadata for
triage without reading every screen: cwd, foreground process, dims,
exit state, and which tab the user has focused.

Per-daemon adds tab lifecycle + trust management on top of those:
`create_tab` (idempotent via a stable `name` — reuse, don't stack),
`close_tab`, `resize_tab`, `get_clipboard`, `list_proposals`,
`approve_proposal`, `drop_proposal`, `set_agent_mode`,
`authenticate`, `list_clients`, `get_server_info`.

Both screen readers take `styled: true` to return per-line **runs of
styled text** instead of flat strings — `{t, fg, bg, a}` with terse
keys (palette colors as ints, truecolor as `#rrggbb`, attrs like
`"faint"` / `"bold,underline"`). `get_screen` always includes
`cursor: {row, col, visible}`. Together these let an agent separate
presentation from content — the motivating case being TUI ghost
text: Claude Code renders its autocomplete suggestion as faint text
after the cursor, which in a flat string is indistinguishable from
input the user actually typed. Faint run at/after the cursor → a
suggestion, not a command; red runs → error output.

`tools/list` is always the authoritative catalog — descriptions and
schemas come back in the listing.

## Trust model (per-daemon socket)

Each connection to a daemon's MCP socket has a mode:

- **observe** (default) — reads work, writes return error `-32099`.
- **propose** — writes are queued for review (the GUI shows an
  approval banner; `list_proposals` / `approve_proposal` manage the
  queue). A propose-mode agent cannot approve its own writes.
- **auto** — writes go straight to the PTY. For agents with full
  delegated authority (headless servers, CI).

Configured under `[mcp]` in `~/.config/xerotty/config.toml`:

```toml
[mcp]
default_mode      = "observe"   # connection start mode
allow_mode_change = true        # false → connections can't elevate their own mode
approval_token    = ""          # shared secret for agent/authenticate
```

The GUI aggregator is deliberately ungated: it runs inside the
user's own GUI process and exposes exactly what the user already
sees. Lock down agents you don't fully trust by pointing them at a
daemon socket with `default_mode = "observe"` /
`allow_mode_change = false` instead.

## Debugging without an MCP client

The daemon socket also answers plain JSON-RPC methods (`tabs/list`,
`tab/screen`, `tab/input`, …) so you can poke it from a shell:

```sh
echo '{"jsonrpc":"2.0","id":1,"method":"tabs/list"}' \
    | nc -U "$XDG_RUNTIME_DIR/xerottyd.mcp.sock"
```

…but agents should use `xerotty mcp` — hand-rolling socket I/O is
the debugging path, not the integration path.
