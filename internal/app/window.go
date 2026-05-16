package app

import (
	"github.com/AllenDang/cimgui-go/backend"
	"github.com/AllenDang/cimgui-go/backend/sdlbackend"
	"github.com/LXXero/xerotty/internal/renderer"
	"github.com/LXXero/xerotty/internal/scrollback"
	"github.com/LXXero/xerotty/internal/tabs"
)

// Window owns the per-OS-window state. One Window = one SDL_Window =
// one terminal grid + tab bar + per-window UI overlays.
//
// Phase 1 of the multi-window refactor (see docs/MULTI_WINDOW_REFACTOR.md):
// App embeds *Window so every existing a.<field> access keeps resolving
// via Go's struct field promotion. Method receivers stay on *App for now
// — they'll migrate to *Window in phase 2. The embed will become a slice
// in phase 3 to land actual multi-window support.
//
// Cross-window shared state (theme, config, font cache, base cell metrics)
// stays on the owning App so font reloads and theme switches apply
// everywhere at once.
type Window struct {
	// cimgui-go backend handle. Phase 1: one backend, one Window. Phase
	// 3: each Window will reference the shared App-level backend but
	// render through its own ImGui viewport.
	backend backend.Backend[sdlbackend.SDLWindowFlags]

	// Window dimensions in logical pixels, synced each frame from
	// imgui.IO.DisplaySize.
	width  int
	height int

	// Cell geometry — width/height = ceil(app.baseCellW/H * fontScale).
	// Stored per-window so resize math doesn't have to re-derive on
	// every cell lookup.
	cellW   float32
	cellH   float32
	tabBarH float32

	// Terminal grid content for this Window.
	tabs     *tabs.Manager
	scroll   map[int]*scrollback.State // per-tab scrollback offset
	renderer *renderer.Renderer

	// Per-Window UI state. Each Window tracks its own selection,
	// hovered link, search overlay, prefs dialog, etc. — moving any
	// of this to App would couple two open Windows together (e.g.,
	// dragging a selection in Window A would extend a selection in
	// Window B).
	fullscreen         bool
	tabBarHovered      bool
	tabSwitchReq       int
	ready              bool
	sel                selection
	pendingPaste       string
	resizeTime         float64
	resizeOverlay      bool
	resizeOverlayText  string
	lastCols           int
	lastRows           int
	hoveredLink        *linkHit
	renamingTab        bool
	renameBuffer       string
	sbDragging         bool
	searchFocusInput   bool
	searchInputFocused bool
	searchOverlayW     float32
	lastDblClickTime   float64
	lastDblClickRow    int
	lastDblClickCol    int
	prefDialog         configDialog
	lastTabBarW        float32
	lastTabBarH        float32
	skipDisplaySync    int
}

// newWindow returns a freshly-initialized Window with the minimum
// defaults needed before App.Run populates the rest (backend, tabs,
// renderer, cell metrics — all set during the boot sequence).
func newWindow() *Window {
	return &Window{
		scroll:       make(map[int]*scrollback.State),
		tabBarH:      0, // updated each frame from imgui.FrameHeight() when >1 tab
		tabSwitchReq: -1,
	}
}
