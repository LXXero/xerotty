# MCP — driving xerotty from AI agents

xerotty exposes terminal sessions to MCP clients (Claude Code, custom
orchestrators, anything that speaks the Model Context Protocol). An
agent can list tabs, read screens and scrollback, type input, paste,
spawn tabs and wait for things to happen in them (see
[Tab events](#tab-events-wait_for_events)) — on the local machine and
on every remote host the GUI is attached to.

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

The bridge **survives its socket dying** (daemon hot upgrade, GUI
restart): it re-runs discovery and reconnects for up to 30s, so the
MCP client never sees its server process exit. At most one in-flight
request is lost per reconnect (its response died with the old
connection — the client's retry covers it). The bridge exits only
when its stdin closes (the client is gone) or nothing comes back
within the window.

A reconnect only dials sockets of the **same kind** it first attached
to (GUI aggregator or daemon), because the two servers type `tab_id`
differently (strings like `local:3` vs plain integers). That lock also
holds across the bridge's self-upgrade: when the xerotty binary was
replaced, the bridge exec's into the new one and passes its server
kind in `XEROTTY_MCP_FLAVOR` (next to the trust mode in
`XEROTTY_MCP_REPLAY_MODE`); the new image waits up to the same 30s for
a socket of that kind instead of falling back to the other one.

Because the daemon's trust mode is **per-connection** state, the
bridge also re-asserts the client's last requested mode (from
`set_agent_mode`) on every reconnect — without that, each daemon
upgrade silently demoted agents back to `observe` and their next
write failed mysteriously. A flap guard backs off when connections
die young so a bouncing server can't induce a reconnect storm.

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
`send_input`, `send_keys`, `send_paste`, `create_tab`, `close_tab`, `rename_tab`,
`wait_for_events`. `list_tabs`
returns per-tab metadata for triage without reading every screen:
cwd, foreground process, dims, exit state, and which tab the user
has focused. `create_tab` takes `host` (any namespace from
list_tabs — so an agent can open a tab on a remote daemon) and an
optional stable `name` for idempotent find-or-create: same name →
same tab (`reused: true`), no duplicate stacking. Created tabs pop
into the user's GUI immediately.

Per-daemon adds trust management + extras on top of those:
`create_tab` (same name semantics), `close_tab`, `rename_tab`, `resize_tab`,
`get_clipboard`, `list_proposals`,
`approve_proposal`, `drop_proposal`, `set_agent_mode`,
`authenticate`, `list_clients`, `get_server_info`. It has its own
`wait_for_events` too, for that daemon's tabs only.

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

**Keystrokes go through `send_keys`** — agents reliably lose the
JSON-escaping guessing game when expressing Enter/Ctrl-C as raw
bytes ("\r"? "\\r"? "\u000d"?), so keys have names instead:

```json
send_keys {"tab_id": "local:1", "text": "make test", "keys": ["enter"]}
send_keys {"tab_id": "local:1", "keys": ["ctrl+c"]}
send_keys {"tab_id": "local:1", "keys": ["up", "up", "enter"]}
```

`text` is typed first, completely literally; then each key token.
Tokens are a single literal character or a named key (`enter`,
`esc`, `tab`, `backspace`, `space`, `delete`, `insert`, arrows,
`home`/`end`, `pageup`/`pagedown`, `f1`–`f12`) with modifier
prefixes joined by `+` or `-`: `ctrl+c`, `alt+enter`,
`ctrl+shift+up`, and — because the remainder after the modifiers IS
the key — `ctrl++` is ctrl and `+`. tmux-style `C-c` / `M-x` work
too. Arrows honor the tab's app-cursor mode (DECCKM) server-side,
which raw bytes can't do; chords with no classic terminal encoding
(`ctrl+enter`, `ctrl++`) are sent as CSI-u for modern TUIs. Unknown
tokens error with the full vocabulary. `send_input` remains for
truly raw bytes (standard JSON unescaping, no extra layer).

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

## Tab events: `wait_for_events`

Each daemon records what happens in its tabs as events, so an agent
can learn that a tab needs looking at without polling every screen
and without other agents typing "I'm done" into its tab. Everything
comes from what the daemon already sees: its own tab bookkeeping and
the PTY output (OSC titles, BEL, notification OSCs). Nothing has to
be installed in the programs running in the tabs. Events never carry
screen contents; when one says to look, read the tab with
`get_screen`. The screen stays the source of truth: events say *when*
to read it.

### Event kinds

| kind | recorded when | fields beyond `seq`, `time`, `tab`, `name` |
|---|---|---|
| `tab_created` | a tab is opened (any client, MCP or GUI) | |
| `tab_closed` | a tab is closed; the `child_exited` from closing its PTY follows | |
| `tab_renamed` | a tab's assigned name changes | `old_name`, `new_name` |
| `child_exited` | the tab's program exits | `exit_code` |
| `title_changed` | the program sets a new title (OSC 0/1/2), filtered as below | `old_title`, `new_title`, `old_glyph`, `new_glyph`, `old_state`, `new_state` |
| `bell` | the program rings the bell (BEL); at most one event per tab per second | |
| `notification` | the program sends a desktop notification | `protocol`, `notify_title`, `notify_body` |

`name` is the tab's assigned name at the time (omitted for unnamed
tabs). `time` is the daemon's clock.

**Title glyphs.** `old_glyph` / `new_glyph` are the leading status
symbols of each title (the same ones the tab bar strips from labels),
and `old_state` / `new_state` classify them:

| program | working | idle |
|---|---|---|
| Claude Code | spinner: braille frames (`⠂`, `⠐`, …) or `◐◓◑◒` → `working` | `✳ Claude Code` → `idle` |
| Codex | braille spinner `⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`, sometimes doubled (`⠹ ⠹ \| project`) → `working` | no glyph at all (`project`, `<thread> \| project`) → `""` |

Any braille pattern counts as a spinner frame. Other glyphs pass
through with state `""` (unclassified). So "an agent finished" shows
up as `old_state: "working"` with either `new_state: "idle"` (Claude
Code) or `new_glyph: ""` (Codex).

Agents animate their spinner several times a second, so a title
change is only recorded when the title text (glyphs removed) changes,
when the classified state changes, or when an unclassified glyph
changes. A spinner going from one frame to the next is not an event.

**Notifications.** xerotty parses three OSC notification forms and
records them; it does not show desktop notifications.

| `protocol` | sequence | used by |
|---|---|---|
| `osc9` | `OSC 9 ; body` | iTerm2, also Ghostty and WezTerm. ConEmu's numeric subcommands (`9;4;…` progress bars) are not notifications and are dropped |
| `osc777` | `OSC 777 ; notify ; title ; body` | rxvt-unicode, foot, Ghostty |
| `osc99` | `OSC 99 ; metadata ; payload` | kitty; chunked (`d=0`) notifications are joined, base64 (`e=1`) payloads decoded |

Title and body are capped at 512 bytes each.

### The ring and the cursor

Each daemon keeps its last 4096 events in memory, numbered by a
sequence (`seq`) that starts at 1 when the daemon process starts. The
ring also has an `epoch`, a random id per daemon process: a restart
or `serve --upgrade` starts a new ring with a new epoch and `seq`
counting from 1 again.

`wait_for_events` returns the events after your cursor, oldest first,
or blocks until a matching one arrives or `timeout_ms` passes (then
`events` is empty). `timeout_ms` defaults to 30000 and is capped at
300000; `0` returns at once. Filters (`tab_ids`, `kinds`, and on the
GUI `hosts`) are optional; an unknown kind name is an error, so a typo
doesn't quietly match nothing. A reply's cursor advances past events
the filters rejected, including on timeout, so the next call doesn't
scan them again. The call is a read: it works in observe mode.

When events after your cursor are gone, the reply says so and holds
everything the ring still has that matches:

- `restarted`: the daemon restarted, so your cursor came from an
  earlier process and its events are lost.
- `overflow`: more events happened since your last call than the
  ring holds.

### Per-daemon: `after_seq` + `epoch`

```json
wait_for_events {"after_seq": 0, "timeout_ms": 0}
→ {"events": [...], "last_seq": 42, "epoch": "9f3c…", "missed": false}

wait_for_events {"after_seq": 42, "epoch": "9f3c…", "kinds": ["title_changed", "bell", "notification"]}
→ blocks until one of those arrives (or 30 s)
```

Pass `last_seq` and `epoch` back as `after_seq` and `epoch`. A gap
shows as `missed: true` with `missed_reason`. `tab_ids` are the
plain-integer ids from `list_tabs`.

### GUI aggregator: a cursor across hosts

The aggregator merges every attached host into one stream. Each event
gains `host` and a namespaced `tab_id` (`kh:3`); `seq` stays per-host.
Events from different hosts are interleaved by time, and each host's
own events stay in seq order.

The cursor is a map of host to `{epoch, seq}`, returned with every
reply. Pass it back unchanged; omit it on the first call to get
everything the daemons still hold, or call once with `timeout_ms: 0`
and keep only the cursor to start from now.

```json
wait_for_events {"timeout_ms": 0}
→ {"events": [...], "cursor": {"local": {"epoch": "9f3c…", "seq": 42},
                                "kh":    {"epoch": "51ab…", "seq": 7}}}

wait_for_events {"cursor": {"local": {...}, "kh": {...}}, "kinds": ["title_changed"]}
→ {"events": [{"host": "kh", "tab_id": "kh:3", "seq": 8, "kind": "title_changed",
               "old_state": "working", "new_state": "idle", ...}],
   "cursor": {...}, "missed": {"local": "restarted"}}
```

It blocks on every host at once and returns when any of them has a
matching event or a gap, after one more non-blocking read of the rest
so the reply holds what every host has at that moment. `missed` maps
each host with a gap to `restarted` or `overflow`; `errors` maps hosts
that could not be read (not connected, or a daemon too old to record
events) to the reason. Hosts outside the call's `hosts` / `tab_ids`
keep their cursor entries unchanged.

## Tab events as a stream: `xerotty events`

`xerotty events` prints the same events `wait_for_events` returns,
one JSON object per line. Without `--follow` it prints what the
daemons still hold and exits; with `--follow` it keeps printing until
killed. A background task that wakes on each output line (Claude
Code's Monitor tool, a `while read` loop) then gets pushed events:
nothing polls screens and no agent has to type into another tab.

```sh
# wake when an agent's title glyph changes, a bell rings or a
# program sends a notification; skip what already happened
xerotty events --follow --new --kind title_changed,bell,notification
```

| Flag | Meaning |
|------|---------|
| `--follow`, `-f` | keep printing events until killed |
| `--new` | skip the events already held; print only new ones (use with `--follow`) |
| `--host H` | only this host namespace (`local`, `kh`, …); repeatable or comma-separated |
| `--kind K` | only these kinds; repeatable or comma-separated |
| `--tab ID` | only this tab (`kh:3`; a bare `3` means `local:3`); repeatable or comma-separated |
| `--socket PATH` / `--daemon` | same discovery as `xerotty mcp` |

Each event line is `{host, tab_id, seq, time, kind, ...}` with the
kind's fields, the GUI aggregator's shape, even when it reads a
single daemon (that daemon's tabs are reported as host `local`). Two
other lines can appear:

- `{"kind":"events_missed","host":H,"reason":"restarted"|"overflow"}`:
  events from H were lost, because its daemon restarted and its
  sequence reset, or because more happened than its ring holds.
- `{"kind":"host_error","host":H,"error":"..."}`: H could not be read.
  Printed again only when the error changes.

It reads the GUI aggregator when the GUI is running, which covers
every attached host, and otherwise the local daemon's socket, which
covers local tabs only. With `--follow` it reconnects when the socket
goes away (GUI restart, `serve --upgrade`) and keeps its per-host
cursor, so a GUI restart loses nothing and a daemon restart shows up
as `events_missed`.

## Debugging without an MCP client

The daemon socket also answers plain JSON-RPC methods (`tabs/list`,
`tab/screen`, `tab/input`, `events/wait`, …) so you can poke it from a
shell:

```sh
echo '{"jsonrpc":"2.0","id":1,"method":"tabs/list"}' \
    | nc -U "$XDG_RUNTIME_DIR/xerottyd.mcp.sock"
```

…but agents should use `xerotty mcp` — hand-rolling socket I/O is
the debugging path, not the integration path.
