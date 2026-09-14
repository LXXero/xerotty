# ImGui / cimgui-go upgrade — plan, procedure, and the 1.6.0 pass

Status: DONE on branch `imgui-1.93` for Linux (build, full suite,
GL/GPU pixel A/B, loop-health both backends); macOS pass PENDING.
Drafted 2026-09-14.

## How the pieces fit (read this before touching any of it)

Three copies of Dear ImGui must agree EXACTLY, or the binary compiles
and then asserts `IMGUI_CHECKVERSION` ("Mismatched struct layout!")
at runtime:

1. **cimgui-go** (`go.mod`) — the Go bindings AND a prebuilt
   `lib/<os>/<arch>/cimgui.a` containing the ImGui core plus the
   precompiled Glfw / SDL2 / **OpenGL3** backends. That is why we
   vendor only `imgui_impl_opengl3.h`: the GL renderer backend comes
   out of `cimgui.a`. No SDL3 symbols live there, which is why we
   compile the SDL3 backends ourselves without a clash.
2. **`internal/platform/imgui/`** — vendored headers (`imgui.h`,
   `imgui_internal.h`, `imstb_textedit.h`, `imconfig.h`) our C++ shim
   compiles against. They MUST be the headers `cimgui.a` was built
   from; `imconfig.h` additionally carries the three defines cimgui-go's
   `lib/CMakeLists.txt` passes (`IMGUI_DISABLE_OBSOLETE_FUNCTIONS`,
   `IMGUI_USE_WCHAR32`, `IMGUI_IMPL_API=extern "C"`) — cgo's flag
   validator refuses the quoted `-D`, so they live in the header.
3. **Vendored backends** — `imgui_impl_sdl3.cpp`, `imgui_impl_sdlgpu3.cpp`,
   `imgui_impl_sdlrenderer3.cpp` (+ their headers in
   `imgui_backends/`), taken from the SAME cimgui-go bundle
   (`cwrappers/imgui/backends/`) and carrying our `XEROTTY PATCH`
   hunks.

cimgui-go bundles all of this under `$GOMODCACHE/github.com/!allen!dang/
cimgui-go@vX/cwrappers/imgui/`, so re-vendoring from the target module
dir guarantees consistency by construction. cimgui-go's own
`backend/sdlbackend` is SDL2-only (still true at v1.6.0) — we never
import `cimgui-go/backend`; `internal/platform` IS our backend.

## Procedure (what the 1.4.0 -> 1.6.0 pass actually did)

1. `go get github.com/AllenDang/cimgui-go@vNEW` (worktree, not main).
2. Trial `go build ./...` — surfaces Go-API breaks (1.6.0: exactly one,
   `FontConfig.SetPixelSnapV` obsoleted; default is true, delete the call).
3. Re-vendor from the new bundle:
   ```sh
   D=$GOMODCACHE/github.com/\!allen\!dang/cimgui-go@vNEW/cwrappers/imgui
   command cp -f $D/{imgui.h,imgui_internal.h,imstb_textedit.h,imconfig.h} internal/platform/imgui/
   command cp -f $D/backends/imgui_impl_{sdl3,sdlgpu3,sdlrenderer3}.cpp internal/platform/
   command cp -f $D/backends/imgui_impl_{sdl3,sdlgpu3,sdlgpu3_shaders,sdlrenderer3,opengl3,opengl3_loader}.h internal/platform/imgui_backends/
   ```
   (`command cp -f`: the interactive `cp -i` alias will hang a script.)
4. Re-apply our patches. Generate them FIRST, against the OLD bundle,
   so you have a clean set: `diff -u $OLD/backends/f internal/platform/f`
   for each patched file. Then `patch --batch --forward -F3 <target> <
   diff` per file; anything in `*.rej` is re-applied by hand against
   the new upstream code. 1.6.0 pass: sdlgpu3.cpp 5/5 clean,
   sdlgpu3.h 1/1, imconfig 1/1, sdl3.cpp 8/10 — two hand re-applies
   (viewport-enable flag; popup mouse-fallback guard), both because
   upstream had restructured the surrounding lines, not because the
   intent changed.
5. Fix our C++ shim against the new ImGui API (`glyphbatch.cpp` was
   the only file; see "What 1.93 changed" below).
6. Fix Go call sites (DrawList signature changes).
7. `./build.sh && go test ./...`, then the runtime matrix (below).

## Patch inventory (what must survive every upgrade)

`grep -rn -i "xerotty patch" internal/platform/imgui_impl_* internal/platform/imgui_backends/`

- **imgui_impl_sdl3.cpp** (8 markers / 10 hunks): cocoa_focus_darwin
  C-linkage decl; macOS-26 popup motion-coord rebuild; macOS key.mod
  lie during …; disable NSWindow animations; enable multi-viewport
  unconditionally (Wayland); popup-window mouse-fallback skip; popup
  DisplayScale gating (HighPixelDensity); per-window text-input.
- **imgui_impl_sdlgpu3.cpp** (5 hunks): C-linkage decl;
  CreatePipelineWithBlend (premul blit pipeline); RenderState exposes
  CommandBuffer/RenderPass; Metal drawable-pool cap; secondary-viewport
  theme clear color; encode-before-acquire; BLOCKING swapchain acquire
  (the SDL 3.4.14 inverted-fence dodge).
- **imgui_impl_sdlgpu3.h** (1 hunk): RenderState fields +
  CreatePipelineWithBlend decl.
- **imconfig.h** (1 hunk): the three CMakeLists-parity defines.
- imgui_impl_sdlrenderer3.cpp and every other header: UNPATCHED
  (byte-identical to the bundle) — just overwrite.

## What ImGui 1.92.4-WIP -> 1.93.0-WIP (1.92.9b docking) changed for us

- `ImDrawData::CmdListsCount` obsoleted (gone under
  `IMGUI_DISABLE_OBSOLETE_FUNCTIONS`): the synthetic ImDrawData in
  glyphbatch.cpp drops the assignment — `CmdLists.Size` is the count.
- `ImDrawCallback_ResetRenderState` constant obsoleted -> use
  `ImGui::GetPlatformIO().DrawCallback_ResetRenderState`. All three
  renderer backends (OpenGL3 in cimgui.a, SDLGPU3, SDLRenderer3)
  register it.
- Samplers removed from `ImGui_ImplSDLGPU3_RenderState`; the backend
  owns Linear/Nearest samplers and exposes
  `platform_io.DrawCallback_SetSamplerNearest/Linear`. The premul blit
  now requests Nearest through that callback instead of poking
  `SamplerCurrent`; our RenderState patch shrinks to just the
  CommandBuffer/RenderPass handles. `platform_gpu_nearest_sampler()` in
  sdl3.cpp is now unused (kept; harmless).
- ImDrawList "signature consistency" break: `AddLineV(p1,p2,col,thick)`
  is now an axis-aligned vertical-line helper — the thick general line
  is `AddLineArgs(p1,p2,col,thickness)` (pure rename for us).
  `AddRectV` reordered to `(min,max,col,rounding,THICKNESS,flags)` —
  our `0, 0, 1` calls became `0, 1, 0`.
- cimgui-go `lib/CMakeLists.txt`: C++17 (already ours), new
  `-DCIMGUI_VARGS0` (wrapper-only, no layout effect), same three
  ImGui defines — imconfig parity unchanged.

## Verification matrix

| check | 1.6.0 pass |
|---|---|
| `./build.sh` + `go test ./...` (Linux) | green |
| `tools/render-ab-check.sh` — GL vs GPU pixel A/B (exercises real ImGui init, glyph atlas, premul blit) | 0 differing pixels |
| `tools/loop-health-check.sh gl` / `gpu` | green (3/3 and 1/1) — note: the check pops a real window; stray desktop mouse input fails it spuriously, rerun before believing a failure |
| macOS: build (`make app`), dialogs/combos/popups, SDL_GPU Metal path, tab drag, capture | PENDING — needs a Mac |
| xlin / xryzen daemons | not affected (GUI-only change), but GUIs on every box need the rebuild |

## Rollback

Everything is on branch `imgui-1.93`; main is untouched. `go.mod` pin +
the vendored dirs are the whole surface — reverting the branch restores
1.4.0 exactly.

## Next time

Expect the same shape: one or two Go-API renames, a couple of C++
shim adaptations to whatever ImGui obsoleted, and 0–3 hand re-applies
in the sdl3 backend where upstream restructured near our patches.
Budget an hour on Linux plus a Mac pass. Track upstream absorptions:
if a future backend natively covers one of our patches (the blocking
acquire is the likeliest candidate), retire the hunk instead of
re-applying it.
