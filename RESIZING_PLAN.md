# Window Resize / Cell-Snap Planning Doc

Notes from a long debugging session on 2026-04-26. Use this to decide direction
before tackling the snap-on-resize work.

---

## What works today

- Drag-resize: window resizes smoothly, terminal grid floors to fit available
  space. No snap.
- Font zoom (Ctrl+= / Ctrl+-): font scales, window snaps to maintain row/col count.
  Single-tab fast in both directions, multi-tab fast both directions after
  commit `e78c08a` (asymmetric tab bar size update).
- Mash + zoom: full speed.
- Ctrl-hold + zoom: now full speed both directions.

## What's broken / suboptimal

- **Sub-cell remainder.** Drag-resize lands at arbitrary pixel sizes. Terminal
  grid floors → leftover sub-cell pixels along right/bottom edges → background
  shows through where ASCII art expects continuous fill. This is the visible
  symptom for "ASCII art rendering with gaps."
- **No live resize-snap (xfce4-terminal feel).** Other terminals (xfce4,
  gnome-terminal) constrain drag-resize to whole-cell increments at the WM
  level — feels like "the window resists until you cross a cell boundary."

---

## Why we can't easily replicate xfce4-terminal's behavior

xfce4-terminal uses GTK's `gtk_window_set_geometry_hints()` with
`GDK_HINT_RESIZE_INC | GDK_HINT_MIN_SIZE | GDK_HINT_BASE_SIZE`. On X11 this
maps directly to `XSetWMNormalHints` with `PResizeInc`, and the WM enforces it.
On Wayland there's no equivalent protocol, so GTK fakes it client-side: in their
`xdg_surface.configure` handler they call `gdk_window_constrain_size()` to round
the proposed size to whole-cell multiples *before* acking the configure. The
compositor then displays the snapped size — the user never sees the unsnapped
intermediate.

**SDL2 doesn't expose the configure ack.** Its Wayland backend handles
xdg_surface internally and always acks with the proposed size. By the time we
see anything (via DisplaySize polling or a SDL_AddEventWatch on
WINDOWEVENT_SIZE_CHANGED), the WM has already accepted the unsnapped size and
SDL has resized its framebuffer to match. We can only call `SDL_SetWindowSize`
back to a snapped value, which is a separate round trip.

**SDL3 is no better here.** No `SDL_SetWindowSizeIncrements` exists despite
some sources claiming it does (verified against current SDL3 source/wiki).
SDL3 *does* add `SDL_SyncWindow` which blocks until the WM applies a size
change — that could enable a cleaner snap-back, but blocks the main thread
for the duration of the WM round trip (potentially 100ms+).

---

## Things we tried today

### After-drag snap (~150ms stable then snap once)
Wait until DisplaySize hasn't changed for a short interval, then call
`SetWindowSize` once with the snapped dimensions.
- Pro: smooth during drag (no jitter), final geometry has no sub-cell remainder
- Con: window edge tracks the cursor live, snap is visibly an after-the-fact
  correction. Not GTK-feel.

### Live snap (every resize event → snap-back immediately)
On every WM-proposed size, immediately call `SetWindowSize` back with snapped.
- Pro: closest to live-snap appearance
- Con: jittery — user sees flicker between proposed and snapped on every
  drag mouse movement. SDL discourse warns explicitly against this pattern.

### Render gutter (paint background where snap is needed)
Let the WM grow the window to whatever the user drags, but render terminal
content only in the snapped subset; fill the remaining gutter with theme bg.
- Pro: terminal content snaps in cell-multiples, no gutter flicker
- Con: window edge still moves with cursor (only content snaps); doesn't
  achieve the "edge resists" feel either

### Take-over resize (`SDL_SetWindowResizable(false)` + custom mouse handling)
Disable the WM's resize handles, implement border-grab + mouse-drag-resize
ourselves via mouse events. Full control over when the actual window resizes.
- Pro: can implement exactly the GTK-style resist behavior
- Con: lose WM's native resize cursor on edges, hover highlights, edge-snap
  to display borders, etc. ~150 lines of new code. Different feel than other
  apps on the system.

---

## Paths forward, ordered by effort

### 1. Ship after-drag snap (current commit reverted; ~30 lines to re-add)
- Effort: 1 hour
- Result: no more sub-cell remainder, ASCII art gaps fixed, window snaps once
  after drag. UX compromise: visible after-drag snap correction.
- Status: implemented earlier today, tree was reverted because user wanted
  closer-to-live behavior.

### 2. Render gutter
- Effort: half day
- Result: terminal snaps in cells while user drags; window edge keeps tracking
  cursor; theme-bg gutter fills the diff.
- Useful if "edge resists" feel isn't critical but you want the terminal
  content to look snap-aligned during drag.

### 3. Take over resize
- Effort: 2-3 days
- Result: closest match to GTK-style resist feel. Full control.
- Cost: lose WM-native resize affordances; maintain custom code.

### 4. Migrate to SDL3 + use SDL_SyncWindow for snap-back
- Effort: 3-5 days (Zyko0/go-sdl3 binding + custom imgui_impl_sdl3 Go wrapper
  + xerotty migration + bug shakeout)
- Result: cleaner snap-back behavior using SyncWindow; blocking duration
  may still feel laggy on slow Wayland configures. Also unlocks better HiDPI
  and forward-looking SDL3 maintenance.
- Risk: SDL3 ecosystem still maturing; new binding burden.

### 5. Patch SDL2 / upstream contribution
- Effort: unbounded (open PR, wait for merge, wait for release, etc.)
- Result: proper API exposure (configure ack hook or SizeIncrements port)
- Reality: not a near-term solution.

---

## Recommendation (subjective)

For now: **option 1 (after-drag snap)** as the "good enough" win that fixes
the ASCII art gap symptom and matches what most terminal users expect. Visible
after-drag correction is annoying but rare in the user's typical workflow
(you don't drag-resize constantly).

For later: **option 4 (SDL3 migration)** when there's a focused window of
time, since it also unlocks HiDPI improvements and is forward-looking. The
snap behavior would still need additional work post-migration (using
SyncWindow), but the migration itself is independent value.

**Avoid options 2 and 3** unless option 1 turns out to feel worse than
expected after a few days of use.

---

## Reference: today's findings

- `e78c08a` — fixed multi-tab held-zoom auto-repeat by making tab bar size
  update asymmetric (defer on grow, immediate on shrink). Different bug,
  related rabbit hole.
- `167d6e3` — renamed `fullscreen.go` → `sdl_helpers.go` since the file now
  holds general SDL cgo helpers, not just fullscreen.
