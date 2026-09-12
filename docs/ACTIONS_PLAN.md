# Actions Plan — one registry, many invokers

Status: DESIGN (nothing implemented). Owner: xero. Drafted 2026-09-11.

## Motivation

Actions today are strings dispatched by a ~40-case `switch` in
`dispatchAction` (internal/app/app.go). Keybinds map chord→string,
menu items carry the same strings, and `ShortcutForAction` derives
menu shortcut labels from the live keybind map. That's already
many-invokers-to-one-action *behavior* — but with no registry
underneath it:

- Nothing can ENUMERATE actions: the prefs keybind editor can't offer
  a picker, and a typo'd action in config.toml is a silently dead key
  discovered at press time.
- Parameterized actions (`goto_tab:N`, `set_theme:name`) are ad-hoc
  string splits scattered across call sites.
- Menu items and keybinds share only the bare string — no label,
  category, or "needs an active tab" metadata.
- MCP can drive terminal I/O but not the UI itself.

## Layout decision

**Actions are generic named verbs. Assignments are many-to-one
references pointing at them.** One invoker resolves to exactly one
action; one action accepts any number of invokers (multiple keybinds,
multiple menu items, palette entries, MCP calls). An action never
"owns" a key. This is the VS Code / Emacs command model, and the
current code already exhibits it (`Ctrl+Plus` and `Ctrl+Shift+Plus`
both → `font_size_up`).

```
              ┌─ keybind "Ctrl+Shift+T" ─┐
  invokers ─► ├─ menu item "New Tab" ────┼─► action new_tab ─► handler
              ├─ palette entry (future) ─┤    {label, category,
              └─ MCP invoke_action ──────┘     context, arg spec}
```

## Registry schema

```go
type Action struct {
    ID       string                     // "new_tab", "goto_tab"
    Label    string                     // default menu/palette label
    Category string                     // "tabs", "window", "scrollback", "config", …
    Arg      ArgSpec                    // NoArg | IntArg | StringArg | EnumArg(…)
    Context  func(*Window) bool         // nil = always available; else gates menus (gray) and keybinds (no-op)
    Run      func(*Window, string)      // arg pre-validated per ArgSpec
}
```

- Registration replaces the `dispatchAction` switch one case at a
  time — mechanical migration, no behavior change.
- Invocation syntax stays `id` or `id:arg` (the existing colon
  convention), parsed ONCE in the dispatcher.
- Config compatibility: **TOML format unchanged.** `[keybinds]` and
  `[[menu.items]]` already reference actions by string; they just
  start being validated against the registry at load ("`new_tabb` —
  did you mean `new_tab`?" as a startup warning, not a dead key).

## Everything-as-an-action (config mutations included)

Any config setting becomes actionable through two generated action
families, derived from the config schema (reflection over the TOML
struct tags — no hand-maintained list):

- `toggle:<path>`   — booleans: `toggle:appearance.cursor_blink`
- `set:<path>=<v>`  — anything: `set:scrollback.scroll_on_output=true`,
                      `set:appearance.opacity=0.9`

Semantics: apply to the LIVE config exactly like a prefs-dialog edit
(same apply/save path, so persistence and live-reload behavior stay
identical), then mark config dirty per the existing prefs save flow.
Existing one-off actions that predate this (`toggle_opacity`,
`set_theme:<name>`) remain as aliases.

Bind them anywhere: a keybind to flip cursor blink, a menu item to
toggle scroll_on_output, an MCP call to switch themes.

## MCP: invoke_action

One new tool on BOTH sockets (guimcp routes to the focused/named
window; per-daemon MCP is limited to actions whose Context doesn't
require a GUI window):

```json
{"tool": "invoke_action", "args": {"action": "toggle:appearance.cursor_blink", "window": "optional"}}
```

`list_actions` (registry dump with categories + arg specs) rides
along for discoverability. Trust gating follows the existing
observe/propose/auto model — config-mutating and destructive actions
are writes.

## Command palette (later, falls out for free)

Enumerate registry → fuzzy match on label/id → invoke. Needs no new
model; deliberately out of scope for the first pass.

## Phases

1. **Registry + migration** — introduce the registry, convert the
   `dispatchAction` switch case-by-case, keep behavior identical.
   Startup validation of keybinds/menu references.
2. **Prefs picker** — keybind editor lists actions from the registry
   (category-grouped) instead of free-text entry.
3. **Config actions** — the `toggle:`/`set:` families via config
   schema reflection + the shared apply/save path.
4. **MCP** — `invoke_action` + `list_actions` on guimcp (and the
   daemon socket where Context allows).
5. **Palette** — if/when wanted.

## Non-goals / notes

- No re-entrant action composition (actions calling actions) in v1 —
  keeps Run signatures simple; revisit if a real need shows up.
- Chord syntax, menu TOML schema, and `ShortcutForAction` labeling
  stay as-is; the registry slots UNDER them.
- Per-mode binding contexts (e.g. search-overlay-only chords) fit the
  model later via Context predicates on the binding side; explicitly
  deferred.
