# Multi-Window Refactor Plan

Goal: one xerotty process per OS Dock entry, with multiple terminal windows hosted inside it. Today every "new window" action does `exec.Command(os.Executable()).Start()`, spawning a new process; on macOS each process is its own LSApplication, which means a separate Dock icon per window. There is no Mac flag, plist key, or LaunchServices invocation that coalesces Dock icons across processes — bundle ID identifies *which* app handles a launch, not *which Dock icon* shares. Real Mac apps that have "one icon, many windows" (Terminal.app, iTerm2, Safari) are single-process. So is the fix.

On Linux this isn't broken (WM_CLASS-based grouping already keeps multi-process xerotty under one taskbar entry), but the same single-process architecture works there too — and brings free benefits: one Go runtime, one font cache, one glyph cache, one SDL/GL init, faster window-open (no fork+exec).

This doc captures the implementation path so it can be executed in a focused session without re-deliberating the architecture.

---

## Architecture

Split today's `App` (which conflates process-wide and per-window state) into:

| Type | Lifetime | Owns |
|------|----------|------|
| `App` (renamed: process shell) | one per OS process | `cfg`, `theme`, `baseFontSize`, `baseCellW/H`, `pendingFontFace`, `pendingRemeasure`, font/glyph cache, the SDL/GL/ImGui backend, the event loop, `windows []*Window` |
| `Window` (new) | one per OS window | `backend` ref, `width/height`, `cellW/cellH`, `tabBarH`, `tabs`, `renderer`, `scroll`, selection state, hovered link, paste/rename state, search overlay state, prefs dialog, all the `lastTabBar*` / `skipDisplaySync` / `tabSwitchReq` flags |

A first-cut sketch of which `App` field goes where lives in §"Field migration" below.

## SDL+ImGui multi-window strategy

Two viable approaches:

### A. ImGui multi-viewport (preferred)

`ConfigFlagsViewportsEnable` is already set. ImGui's multi-viewport machinery (driven by the platform_io callbacks `ImGui_ImplSDL2_InitForOpenGL` installs) auto-creates a real OS window for any ImGui `Begin/End` window that ends up positioned outside the main viewport's bounds.

Approach:
- Window 0 is the cimgui-go-created main SDL_Window. Renders into `imgui.MainViewport()`'s draw list (current code path).
- Window 1+ each get an `imgui.Begin(uniqueName, ...)` block with `WindowFlagsNoTitleBar | NoResize | NoMove | NoBringToFrontOnFocus | NoSavedSettings`, positioned far enough that ImGui auto-promotes it to its own platform viewport. Render terminal content into that ImGui window's `GetForegroundDrawList()`.
- Mouse hit testing uses `imgui.GetMouseHoveredViewport()` to figure out which Window the cursor is in.

Pros: shares one ImGui context across all Windows, font/texture/style state stays unified, ImGui handles all the SDL_Window plumbing.

Cons: each per-Window state has to be threaded into the ImGui-window's Begin/End block; the rendering code that currently uses `imgui.MainViewport()` (~8 sites) needs to switch to the Window's own viewport.

### B. Multiple ImGui contexts

Each Window has its own `imgui.Context` (sharing the font atlas across contexts via the `ImGui::CreateContext(io.Fonts)` pattern), its own SDL_Window created via `SDL_CreateWindow`, its own GL context via `SDL_GL_CreateContext`. Per-frame loop: for each Window, `igSetCurrentContext`, `SDL_GL_MakeCurrent`, run the existing per-Window frame body, swap.

Pros: each Window is fully independent — no risk of one Window's UI state leaking into another.

Cons: significantly more cgo plumbing — `ImGui_ImplSDL2_InitForOpenGL` and `ImGui_ImplOpenGL3_Init` must be called per-context; event routing must dispatch SDL events by `windowID` to the right context's ImGui IO; font atlas lifetime is shared across contexts and needs care.

**Recommendation: A.** Multi-viewport is what ImGui was designed for; it's already enabled and already powers our popped-out prefs window. Adding our terminal windows to it follows the existing grain.

## Phases

### Phase 1 — Extract `Window` struct (no behavior change)

Mechanical refactor. Single Window in `App.windows[0]`. Same observable behavior as today.

Files touched:
- `internal/app/window.go` *(new)* — `Window` struct + all the methods today on `App` that operate on per-window state
- `internal/app/app.go` — `App` struct slims to process-wide fields + `windows []*Window`. `App.Run()` constructs `windows[0]` and runs the event loop. Process-wide methods stay (`beforeRender`, `idleTimeout` aggregates over Windows).
- `internal/app/config_dialog.go` — `configDialog` becomes per-Window field, methods take `*Window` instead of `*App`
- `internal/app/selection.go` — already per-Window-scoped, no change

Method migration table (incomplete — finalize during implementation):

| Today | Phase 1 |
|-------|---------|
| `(a *App) frame()` | `(w *Window) frame()` |
| `(a *App) gridSize()` | `(w *Window) gridSize()` |
| `(a *App) measureCell()` | `(w *Window) measureCell()` |
| `(a *App) resizeTerminals()` | `(w *Window) resizeTerminals()` |
| `(a *App) processKeys()` | `(w *Window) processKeys()` |
| `(a *App) dispatchAction(action)` | `(w *Window) dispatchAction(action)` |
| `(a *App) updateFontMetrics()` | `(w *Window) updateFontMetrics()` (mutates process-wide font fields too) |
| `(a *App) renderTabBar()` | `(w *Window) renderTabBar()` |
| `(a *App) renderContextMenu()` | `(w *Window) renderContextMenu()` |
| `(a *App) renderSearchOverlay()` | `(w *Window) renderSearchOverlay()` |
| `(a *App) renderResizeOverlay()` | `(w *Window) renderResizeOverlay()` |
| `(a *App) renderPasteDialog()` | `(w *Window) renderPasteDialog()` |
| `(a *App) renderRenameDialog()` | `(w *Window) renderRenameDialog()` |
| `(a *App) handleMouseSelection()` | `(w *Window) handleMouseSelection()` |
| `(a *App) drawScrollbar(...)` | `(w *Window) drawScrollbar(...)` |
| `(a *App) drawSearchHighlights(...)` | `(w *Window) drawSearchHighlights(...)` |
| `(a *App) menuContext()` | `(w *Window) menuContext()` |
| `(a *App) pasteText(text)` | `(w *Window) pasteText(text)` |
| `(a *App) getScroll(tabID)` | `(w *Window) getScroll(tabID)` |
| `(a *App) selectedText()` | `(w *Window) selectedText()` |
| `(a *App) isSearching()` | `(w *Window) isSearching()` |
| `(a *App) popupActive()` | `(w *Window) popupActive()` |
| `(a *App) initialWindowSize()` | `(w *Window) initialWindowSize()` |
| `(a *App) beforeRender()` | `(a *App) beforeRender()` — stays |
| `(a *App) idleTimeout()` | `(a *App) idleTimeout()` — aggregates over `a.windows` |

Field accesses inside moved methods: most stay `w.<field>`. Refs to `cfg` / `theme` / `baseFontSize` / `baseCellW` / `baseCellH` / `pendingFontFace` / `pendingRemeasure` become `w.app.<field>`.

External refs to `App` fields from `config_dialog.go` and `selection.go` need similar threading.

**Commit boundary**: builds cleanly, single Window behaves identically to today.

### Phase 2 — Render multiple Windows per frame

Wire the event loop to iterate `a.windows` and render each. With approach A (multi-viewport), this is:

1. `App` per-frame body (called once per `WaitEventTimeout` return):
   - For each `w` in `a.windows`:
     - If `w` is the main Window (index 0): `w.frame()` renders into `imgui.MainViewport()` as today.
     - Else: `imgui.SetNextWindowPos`, `imgui.SetNextWindowSize`, `imgui.Begin(w.imguiName, ..., NoTitleBar|NoResize|NoMove|NoSavedSettings|...)`, then `w.frame()` renders into `imgui.GetWindowDrawListV()`, then `imgui.End()`.
2. `renderer.Draw` and the various `imgui.MainViewport()` sites in `app.go` get a `viewport` / `drawList` parameter passed in.
3. Mouse-related code switches from `imgui.MousePos() - vp.Pos()` to `imgui.MousePos() - w.viewport.Pos()`.
4. The macOS live-resize watch needs windowID dispatch (today it only knows about the main SDL_Window).
5. `setContentResizeIncrements` is per-Window — phase 2 looks up the correct NSWindow by SDL windowID instead of `SDL_GL_GetCurrentWindow()`.

**Commit boundary**: with `windows[0]` only, behavior is identical. Adding a second Window manually (e.g., a debug action) shows a real second OS window with its own terminal grid.

### Phase 3 — `new_window` action creates Window in-process

Replace the `exec.Command(os.Executable()).Start()` branch in `dispatchAction("new_window")` with `a.NewWindow()` (returns `*Window`, appends to `a.windows`, initializes its `tabs.Manager`, etc.).

`newwindow_darwin.go` / `newwindow_other.go` go away — the new-window path is now platform-neutral.

The `XEROTTY_WIN_X/Y` env-cascade hack also goes away: we have the parent Window's position right there in the same process.

Tab manager construction (currently in `App.Run`'s first-frame init) moves to `NewWindow`.

**Commit boundary**: clicking `new_window` opens a second xerotty window in the same Dock icon.

### Phase 4 — Quit semantics

Today the process exits when the last tab closes (via `sdlQuit`). Now:
- Closing a Window's last tab closes that Window.
- Closing the last Window in `a.windows` exits the process.
- `Cmd+Q` / SDL_QUIT still exits the process.
- `Cmd+W` (and the `close_tab` action) closes the active tab; if the Window has no other tabs left, the Window closes too.

`tabs.Manager` is per-Window now, so tab close detection runs per Window.

### Phase 5 — Polish

- macOS `applicationShouldHandleReopen` handler: when user clicks the Dock icon while xerotty is running with no visible Windows (rare), open a new Window. Requires Cocoa NSApplicationDelegate plumbing; can punt initially.
- macOS Window menu integration: NSWindow auto-lists in the Window menu under the Dock icon if we use AppKit window discovery properly. Should "just work" once windows live in the same NSApplication.
- Update `SPEC.md` §4.5 to drop the "each window = new process" claim.

## Field migration

`App` → `Window` (every field that today refers to per-window UI/SDL state):

```
backend           → Window
width, height     → Window
cellW, cellH      → Window
tabBarH           → Window
tabs              → Window
renderer          → Window
scroll            → Window
fullscreen        → Window
tabBarHovered     → Window
tabSwitchReq      → Window
ready             → Window
sel               → Window
pendingPaste      → Window
resizeTime        → Window
resizeOverlay     → Window
resizeOverlayText → Window
lastCols          → Window
lastRows          → Window
hoveredLink       → Window
renamingTab       → Window
renameBuffer      → Window
sbDragging        → Window
searchFocusInput  → Window
searchInputFocused→ Window
searchOverlayW    → Window
lastDblClickTime  → Window
lastDblClickRow   → Window
lastDblClickCol   → Window
prefDialog        → Window
lastTabBarW       → Window
lastTabBarH       → Window
skipDisplaySync   → Window
```

`App` → `App` (process-wide, stays):

```
cfg
theme
baseFontSize
baseCellW
baseCellH
pendingFontFace
pendingRemeasure
windows []*Window  (new)
```

Question: `pendingRemeasure` could be per-Window if Windows ever had different fonts, but for now all Windows share the font, so `App`-scoped + propagate-to-all-Windows is simpler.

## Open architectural questions

1. **Per-Window or per-App preferences dialog?** Today the dialog mutates `a.cfg`, which is process-wide. If two Windows each have a dialog open, the last "Apply" wins. Per-Window dialog state with shared underlying config is the obvious answer (each dialog operates on a local copy until Apply).
2. **Theme per Window or per App?** Per-App is simpler and matches Mac convention (one preferences pane = one theme for the app). Per-Window would be nice for "tail logs in dark, edit in light" but not a v1 feature.
3. **Font reload latency.** Font face change triggers `pendingFontFace`, which runs at the next `beforeRender` and rebuilds the glyph cache. With N Windows all using the same font, that's still one cache rebuild — they just all pick it up at the next frame. ✅
4. **Cursor-blink animation across Windows.** Today blink interval drives the idle-timeout. With multi-Window, the blink toggle should fire on whichever Window has focus (or all of them in sync — probably the latter is more pleasant).
5. **macOS live-resize watch dispatch.** Today's `liveresize_darwin.go` watch assumes a single main SDL_Window. With multiple Windows, the watch needs to dispatch by `event.window.windowID` and drive the right Window's frame body.

## Test plan

- Before phase 1: full smoke test today's behavior. After phase 1: should be byte-for-byte identical.
- After phase 2 (with hard-coded second Window for testing): both windows render, both can be interacted with, mouse/keyboard route to the focused one.
- After phase 3: `new_window` action creates a Window in-process; verify Mac Dock shows one icon with a windows submenu; verify Cmd+W only closes the focused Window; verify Cmd+Q closes the process.
- After phase 4: last-Window-close exits process cleanly (tabs close, PTYs killed, GL contexts freed).
- Cross-platform smoke: full pass on Linux (X11 and Wayland) to confirm no regressions.

## Effort estimate

- Phase 1: half-day to a day (mostly mechanical, but ~1900 lines of `app.go` to migrate plus the cross-references).
- Phase 2: half-day (renderer threading, multi-viewport plumbing, mouse-coord adjustments).
- Phase 3: a couple hours.
- Phase 4: a couple hours.
- Phase 5: a couple hours.

Total: 2-3 focused days. Best done in a session where the only goal is this refactor, not interleaved with other patches.

---

## Why not the alternatives

For posterity, here are the dead-ends evaluated:

- **`open -n <bundle>` for new_window**: routes through LaunchServices but `-n` explicitly says "new instance", which on macOS means new LSApplication = new Dock icon. Already deployed in `newwindow_darwin.go`; cleaner than bare fork+exec but doesn't solve coalescing.
- **`LSUIElement=YES` on child processes**: hides the child from Dock and Cmd+Tab. Disastrous UX — users can't see or switch to the child windows except by clicking them directly.
- **`LSMultipleInstancesProhibited=YES`**: makes `open -n` reject new instances, but the existing instance still needs single-process multi-window support to actually open a new window in response. Same refactor required.
- **Broker process holding the Dock icon, xerotty workers as LSUIElement children**: complex multi-binary distribution, plus the LSUIElement Cmd+Tab problem above.
- **AppleEvent forwarding**: the receiving end has to actually open a new window. Without single-process multi-window support, that's a no-op. Same refactor required.
- **Conditional architecture (single-process on Mac, multi-process on Linux)**: two completely different lifecycles, two test matrices, theme/config-propagation semantics that differ across platforms. Maintenance nightmare for a fix that's better done once cross-platform.

Conclusion: single-process is the right answer everywhere, and the only answer that fixes Mac.
