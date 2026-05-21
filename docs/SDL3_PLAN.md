# SDL3 platform spike

Plan for replacing xerotty's platform backend with SDL3. Goal: fix the
class of bugs that come from cimgui-go's SDL2 backend (broken native
Wayland, no native popup menus, hacks around multi-viewport) without
rewriting the rest of the app.

## Why this exists

The current platform layer is:

```
xerotty Go ─ cimgui-go (Go bindings) ─ cimgui-go/backend/sdlbackend (SDL2)
                                       ─ sdl2-compat ─ SDL3 (Arch default)
```

Symptoms we've hit:

- **Native Wayland mode is broken**. cimgui-go's SDL2 multi-viewport doesn't
  pop windows out under Wayland — everything renders inside the carrier as
  one giant docked container. Force `SDL_VIDEODRIVER=wayland` and the
  terminal grid misaligns, prefs window opens inside the terminal, etc.
- **No native context menu primitive**. SDL2 has no popup window
  concept. We've burned a lot of time on workarounds (focus polling,
  `XGetInputFocus`, `_NET_ACTIVE_WINDOW`, SDL_CaptureMouse, raw
  XGrabPointer) and none of them gives the GTK-style "click anywhere
  outside dismisses" behavior every other terminal has, especially
  under XWayland where clicks on native Wayland apps never reach us.
- **SDL_CaptureMouse on sdl2-compat → SDL3 is silently a no-op on X11**.
  Returns success without actually XGrabbing, leaving us without a
  reliable global-mouse path even on real X11.

SDL3 fixes all of this:

- `SDL_CreatePopupWindow(parent, x, y, w, h, SDL_WINDOW_POPUP_MENU)`
  creates a window that the platform treats as a popup menu. On Wayland
  it's a real `xdg_popup` (compositor-managed dismiss). On X11 it
  XGrabs correctly + sets `_NET_WM_WINDOW_TYPE_POPUP_MENU`. On macOS
  it uses popup-window semantics. On Windows it uses `WS_POPUP`.
- Wayland support in SDL3 is the actively maintained path; multi-
  viewport pops out correctly.
- Upstream Dear ImGui ships `imgui_impl_sdl3.cpp` (platform) and
  `imgui_impl_opengl3.cpp` (renderer) so the ImGui side has first-class
  SDL3 integration.

What blocks the direct path: **cimgui-go has no SDL3 backend** as of
v1.5.0 (May 2026). The fix isn't to fork cimgui-go — it's to bypass
cimgui-go's backend entirely and write our own thin SDL3+ImGui shell
in cgo, then keep using cimgui-go for the ImGui Go API (Begin, Text,
SliderInt, …). One-time cgo plumbing cost, gain: every SDL3 feature
unlocked.

## Scope

In scope:

- New `internal/platform` package (or similar) that owns SDL3 init,
  the wl/x11/cocoa/win32 window, GL context, ImGui platform + renderer
  backends, event pump, frame driver.
- A small Go API the rest of xerotty calls into — should match the
  surface area we currently use from cimgui-go/backend/sdlbackend:
  CreateWindow, SetBgColor, SetBeforeRenderHook, run loop, texture
  manager, etc.
- Context-menu primitive backed by `SDL_CreatePopupWindow` — first
  thing built once the basic shell renders.
- Multi-window via SDL3's native multi-window APIs (NOT ImGui
  multi-viewport — we own the windows now).

Out of scope (initial spike):

- Full prefs window migration. Keep prefs as an in-process ImGui
  window until the popup primitive is proven.
- HiDPI rework. Keep current fb-scale handling. Refine later.
- Removing cimgui-go entirely. We still want the Go API.

## Acceptance criteria

Brutal and small. Don't migrate xerotty until ALL of these pass on a
spike binary:

1. Native Wayland window appears, no flicker, no double-window.
2. Terminal grid aligns 1:1 with cell boundaries (no inch-wide insets).
3. Right-click opens a popup menu that:
   - is a real `xdg_popup` on Wayland (verify with `wayland-info` or
     compositor logs)
   - dismisses on click ANYWHERE outside, including native Wayland apps
   - dismisses on Escape
   - dispatches the right action on item click
4. Two terminal windows can be open simultaneously, each rendering
   independently, each with working menus.
5. Same binary, with `SDL_VIDEODRIVER=x11`, works identically.
6. macOS build path compiles (don't need to run it day one).

## Architecture

```
xerotty Go
 ├─ cimgui-go (Go bindings for ImGui types, widget calls)   ← keep
 └─ internal/platform                                        ← new
    └─ C: SDL3 + imgui_impl_sdl3 + imgui_impl_opengl3
       │
       ├─ Init SDL3 + GL context
       ├─ Create main SDL_Window (visible)
       ├─ Drive frame: NewFrame → user-render-callback → EndFrame → Render
       ├─ Event pump: SDL_PollEvent → ImGui_ImplSDL3_ProcessEvent
       │   plus our own per-event handlers (PTY wakes, custom keybinds)
       ├─ SecondaryWindow(parent, opts) — for "New Window" action
       └─ PopupWindow(parent, x, y, w, h) — for the context menu
```

Notes:

- cimgui-go's ImGui Go API calls happen against the **same** ImGui
  context our C side initializes. As long as we call `ImGui::CreateContext`
  in C and set up the platform/renderer backends, cimgui-go's
  `imgui.Begin` etc. will Just Work because they use the global
  `GImGui` pointer.
- Each `SDL_Window` we create gets its own ImGui rendering happening
  in-process; not via ImGui multi-viewport (which has the broken-on-
  Wayland problem) but via separate render passes per window with
  SDL3's window-switching APIs.

## Phased milestones

Each phase is a runnable binary. Ship the spike incrementally.

### Phase 0 — bring-up
- `internal/platform/sdl3.{h,c,go}` skeleton.
- `cgo pkg-config: sdl3` + bundled `imgui_impl_sdl3.cpp` + `imgui_impl_opengl3.cpp`.
- `Platform.Init()` creates SDL3 window + GL context, initializes ImGui
  context + backends.
- `Platform.Run(frameFunc)` drives the loop: events → NewFrame →
  frameFunc → Render → SwapWindow.
- Standalone main that just opens a 800×600 window with `ImGui::ShowDemoWindow()`.

**Done when**: demo window appears under both `SDL_VIDEODRIVER=x11`
and `SDL_VIDEODRIVER=wayland`, accepts input, closes cleanly.

### Phase 1 — terminal grid into the new shell
- Wire `internal/renderer` into `Platform.Run`'s frameFunc.
- Render one terminal grid (no tabs, no prefs) into the SDL3 window.
- Hook PTY data → wake event loop equivalent.

**Done when**: terminal renders correctly under both video drivers,
fonts crisp, cell alignment correct.

### Phase 2 — popup menu via SDL_CreatePopupWindow
- New API: `Platform.OpenPopup(parent, x, y, items) chan PopupResult`.
- Implementation creates an `SDL_WINDOW_POPUP_MENU` window as child of
  the main window. Renders menu items via a fresh ImGui pass into that
  popup's GL context. Listens for "outside click" via the platform's
  dismiss event (Wayland `popup_done`, X11 grab-broken event).

**Done when**: right-click in the terminal opens a menu that dismisses
correctly on click-anywhere-outside (incl native Wayland apps) on
Wayland, on X11, and (untested) on macOS.

### Phase 3 — multi-window
- New API: `Platform.NewWindow(opts)` creates an additional `SDL_Window`.
- Frame loop iterates all windows, runs frameFunc with the window as
  context, swaps each.
- Per-window ImGui state (or per-window context if needed).

**Done when**: "New Window" action opens a second terminal, each with
independent input and menus.

### Phase 4 — migrate
- Move the rest of `internal/app` over to `internal/platform`.
- Delete the cimgui-go/backend/sdlbackend dependency from `go.mod`.
- Delete the unused experimental code: XGrabPointer / focus polling
  helpers (the `wlpopup` spike was deleted once `SDL_CreatePopupWindow`
  proved out; our own `wlgrab`/`xgrab` shims in `internal/platform`
  now handle the missing grab/dismiss behavior).
- Update SPEC.md to describe the new architecture.

**Done when**: feature parity with current xerotty, all open issues
related to menus / Wayland fixed, no SDL2 dependency.

## Risk areas

- **Bundling imgui_impl_sdl3.cpp**: needs the matching Dear ImGui
  version that cimgui-go uses (check what version cimgui-go bundles
  and match). Otherwise ABI mismatch → crashes.
- **ImGui context sharing**: cimgui-go expects to own the context. If
  it auto-initializes on first call, we need to either suppress that
  or call cimgui-go's own init API with our pre-existing context.
  Worst case: write a small Go shim that exposes the C-side context
  pointer to cimgui-go.
- **SDL3 Wayland popup parent-surface**: `SDL_CreatePopupWindow`
  requires a parent SDL_Window. If our menu is opened from a
  popped-out terminal window (Phase 3+), we need to thread the right
  parent through. SDL3 handles this for us if we pass the SDL_Window
  pointer correctly.
- **Multi-window without ImGui multi-viewport**: each SDL window needs
  its own ImGui rendering pass. Either per-window ImGui contexts (more
  isolation, more memory) or a single context with manual draw-list
  redirection per window. Decide in Phase 3.
- **GL context per window**: SDL3 supports sharing GL contexts between
  windows. Use shared context so font textures don't have to be
  uploaded per window.

## What to delete after migration

- `internal/app/sdl_helpers.go` — SDL2-specific
- ~~`internal/wlpopup/`~~ — deleted; superseded by `SDL_CreatePopupWindow` + `internal/platform/wlgrab_linux.c`
- `internal/app/cellsnap_*.go` — SDL3 has resize-increment APIs that
  may obsolete the macOS hack
- `internal/app/liveresize_*.go` — same
- The `backend` import from cimgui-go (just `imgui` stays)

## Things to look up

- Exact Dear ImGui version cimgui-go v1.5.0 bundles
- Whether cimgui-go exposes a way to use an externally-created context,
  or if we need to monkey-patch the `imgui.CurrentContext()` accessor
- SDL3 popup window event signature (the exact way "popup dismissed"
  is reported — looks like `SDL_EVENT_WINDOW_HIDDEN` or a dedicated
  popup-done event)
- Whether `SDL_WINDOW_POPUP_MENU` requires `SDL_WINDOW_BORDERLESS`
  alongside or sets it implicitly
