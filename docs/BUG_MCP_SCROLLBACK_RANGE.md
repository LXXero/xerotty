# Bug: GUI MCP `get_scrollback` returns nothing (or the wrong rows) once scrollback outgrows the local mirror

Found 2026-10-02 while reading a long-running xyphia tab (`local:72`, ~10.9k rows) through `xerotty mcp`.

## Status: FIXED (2026-10-02)

**Corrected root cause:** only **windowed** (`scrollback.mode = "unlimited"`)
mode was broken. In capped mode `ScrollbackLen()` is the mirror length itself,
so absolute and mirror indices already coincide; the GUI simply can't see past
the cap. The report's numbers are windowed: `total` 10910 vs an 8000-row
window at `winStart` 2910. `from:0` therefore served absolute row 2910, and
every `from ≥ 8000` (including the default last-200 read) clamped to nil.

**Fix:**
- `daemonsource.Source.SnapshotScrollbackRange` treats `from`/`to` as absolute.
  Rows inside the window are served from the mirror. Rows outside it are
  fetched from the daemon with a **private, correlated** range request.
- The private request carries a new `ReqID` on `ScrollbackRequest`, echoed on
  `ScrollbackRange`. The hub routes these replies to the waiting caller via
  `Hub.FetchScrollbackRange` instead of the display-window path. This means an
  MCP history read never yanks the user's scrolled view.
- If a fetch fails (daemon gone, timeout, or a daemon too old to echo
  `ReqID`), the result is the longest **correctly-labelled prefix**. It is
  never shifted rows.
- `guimcp.getScrollback` reports the served range. On a short read, `to` is
  lowered and `truncated: true` is set. The tool description now says indices
  are absolute (0 = oldest).
- Tests are in `internal/daemonsource/scrollback_range_test.go` (capped,
  windowed without a hub, and end-to-end over a real daemon). The windowed and
  wire tests fail on the old code with exactly the reported symptoms.

Deploying needs both a new daemon (`xerotty serve --upgrade`) and a GUI restart.
The original report follows.

## Symptom

- `get_scrollback {tab_id}` (the default "last 200 rows" call) returns
  `{"from":10710,"to":10910,"total":10910,"lines":[]}`: **zero lines**.
- Explicit ranges whose `from` is ≥ the local mirror length return `lines: []`.
  The first empty index here was 8000.
- Explicit ranges below the mirror length do return rows, but they are the
  **wrong** rows. `from:0` returned text from the middle of the session, not
  the oldest line. The returned rows are offset by however many rows the mirror
  has dropped or windowed out.
- While a tab's scrollback is smaller than the mirror, everything works. That's
  why this went unnoticed: the same tab read fine earlier at `total` 3581.

## Repro

```sh
# any tab whose daemon scrollback > the GUI's per-Source mirror cap
printf '%s\n' \
 '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}' \
 '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
 '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_scrollback","arguments":{"tab_id":"local:72"}}}' \
 | xerotty mcp | tail -1 | jq -r '.result.content[0].text' | jq -c '{from,to,total,n:(.lines|length)}'
# -> {"from":10710,"to":10910,"total":10910,"n":0}
# from:7990,to:8000 -> n:10 (but mislabeled rows); from:8000,to:8001 -> n:0
```

## Root cause

The **index spaces are mixed** between `guimcp` and `daemonsource`:

- `internal/guimcp/server.go` `getScrollback` (~L334) computes the range from
  `src.ScrollbackLen()`, which for a daemon-backed tab is the **daemon's true
  depth** (`scrollbackLen.Load()`, 10910). It then passes **absolute** row
  indices to `src.SnapshotScrollbackRange(from, to)`.
- `internal/daemonsource/source.go` `SnapshotScrollbackRange` (~L292) indexes
  `s.scrollback` (the **local mirror**) with those values directly. It also
  clamps `to` to `len(s.scrollback)`, so any `from ≥ len(mirror)` returns nil.
- The mirror is bounded (`scrollbackCap`, default 10000, 8000 on this machine)
  and drops its oldest rows when full. In windowed mode it is also offset by
  `s.winStart`. So mirror index `i` ≠ absolute row `i`.
- `SnapshotWindow`, in the same file, already handles this correctly
  (`idx -= s.winStart`, absolute indexing against `sbLen`). The range method
  was never brought in line with it.

`terminal.Terminal.SnapshotScrollbackRange` (local, non-daemon tabs) is fine.
It resolves absolute indices across disk and memory. Only the daemon-mirror
path is affected.

## Suggested fix

1. In `daemonsource.Source.SnapshotScrollbackRange`, treat `from`/`to` as
   **absolute** and translate them to mirror indices the way `SnapshotWindow`
   does. The mirror's first absolute row is `sbLen - len(s.scrollback)` in
   capped mode, and `s.winStart` in windowed mode.
2. For absolute rows the mirror doesn't hold (older than the cap, or outside
   the window), either fetch them from the daemon on demand (the
   `EnsureScrollbackWindow` / daemon range-fetch path already exists, see
   `internal/daemon/conn.go` ~L1638), or return what's available and report
   the actual served `from`/`to` in the response. Never silently return
   mislabeled rows.
3. Have `guimcp.getScrollback` report the actually-served range, so callers
   can tell truncated history from empty history.

## Test to add

A daemonsource unit test: build a Source with a small cap (say 10). Append 25
rows labelled `row-0`…`row-24`, so daemon depth is 25 and the mirror holds
15..24. Then assert:
- `SnapshotScrollbackRange(20, 25)` returns `row-20`…`row-24`.
- The default "last N" range returns the newest rows.
- A range entirely below the mirror is either fetched or reported as
  unavailable, never served as `row-0..` content that is really `row-15..`.

Also add one test in windowed mode with a non-zero `winStart`.

## Impact

Agents reading tabs over MCP (xyphia's terminal driver, Claude Code sessions)
get empty or misattributed history on long-running tabs, with no error. This
silent failure is the dangerous part. Separate from this bug, the
`get_scrollback` tool description doesn't say that indices are absolute
(0 = oldest).
