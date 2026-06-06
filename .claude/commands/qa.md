# /qa — functional + visual QA pass for xerotty

Run the automated layers first, then (if a GUI or daemon is running)
drive the live instance through its MCP socket like a user would.
Report findings as a checklist with PASS/FAIL + evidence (test
output, screenshots, screen dumps). Anything FAIL gets a minimal
repro description.

## 1. Automated suites (always)

```sh
./build.sh                                # must say "built:"
go test ./internal/...                    # full unit/integration
go test -run TestMCPFunctionalSession ./internal/runner/   # agent-shaped e2e
go test ./internal/renderer/              # quad-stream invariants
```

## 2. Visual captures (needs a display)

```sh
# Fresh instance, deterministic-ish config; review the PNGs visually.
env XDG_CONFIG_HOME=/tmp/qa-cfg ./xerotty --screenshot /tmp/qa-shot.png --screenshot-frames 60
```

Check: prompt/text crisp, no glyph garbage, theme colors right.
For glyph coverage, make the test shell print emoji / CJK / nerd
glyphs / box drawing first (see git history: "GLYPH TORTURE").

## 3. Live-instance MCP drive (when user's GUI or daemon is up)

Connect via `xerotty mcp` (or the gui aggregator socket). Then:
- `set_agent_mode auto` (ask the user first if mode is propose)
- `create_tab` named "qa-probe"; run `printf`/`seq` checks; verify
  `get_screen` styled output + cursor; `get_scrollback` after an
  overflow; `send_keys` with a chord (e.g. ctrl+l clears).
- Links: print `https://example.com/qa`, scroll it into history,
  confirm it still appears in get_scrollback (renderer-side link
  click can't be driven via MCP — note as manual).
- `close_tab` the probe. NEVER touch the user's own tabs.

## 4. Known manual-only checks (ask the user)

- Context menu: submenu arrow position stable on open
- Tab drag between windows; new-tab-after-drag goes to right group
- Selection highlight stays content-anchored while scrolling
- Prefs dialog: combos open under their buttons; sliders aligned

Summarize as: suites (pass/fail counts), visual (notes per shot),
live drive (per-check), manual list handed to user.
