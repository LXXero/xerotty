package app

// The action registry — phase 1 of docs/ACTIONS_PLAN.md.
//
// Actions are generic named verbs; invokers (keybinds, menu items,
// future palette entries and MCP invoke_action) are many-to-one
// references to them. One invoker resolves to exactly one action; an
// action accepts any number of invokers. An action never "owns" a
// key.
//
// Phase 1 is a MECHANICAL migration: every case of the old
// dispatchAction switch became a registration whose Run closure is
// the verbatim case body — behavior-identical, including each body's
// own nil-tab / empty-selection guards. The Context field exists for
// phase 2 (menu graying, palette filtering) and is deliberately
// unused here so nothing changes.
//
// Invocation syntax is unchanged: "id" or "id:arg" (goto_tab:3,
// set_theme:dracula). Parsing happens ONCE, in resolveAction.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/AllenDang/cimgui-go/imgui"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/input"
	"github.com/LXXero/xerotty/internal/menu"
	"github.com/LXXero/xerotty/internal/platform"
	"github.com/LXXero/xerotty/internal/renderer"
	"github.com/LXXero/xerotty/internal/themes"
)

// ArgKind describes what a registered action expects after "id:".
type ArgKind int

const (
	// NoArg actions match their bare id only.
	NoArg ArgKind = iota
	// IntArg actions require "id:<integer>"; a non-integer arg is a
	// silent no-op (matching the old switch's strconv guard).
	IntArg
	// StringArg actions require "id:<anything>"; the arg may itself
	// contain colons (kick_client:hub:clientID) — only the FIRST
	// colon splits id from arg.
	StringArg
)

// Action is one registered verb.
type Action struct {
	ID       string
	Label    string // default menu/palette label
	Category string
	Arg      ArgKind
	ArgHint  string // documents the arg for pickers/errors
	// Context gates availability (menus gray, palette filters).
	// Unused in phase 1 — bodies keep their own guards.
	Context func(*Window) bool
	Run     func(w *Window, arg string)
}

var actionRegistry = map[string]*Action{}

func registerAction(a Action) {
	if a.ID == "" || a.Run == nil {
		panic("registerAction: action needs an ID and a Run")
	}
	if strings.Contains(a.ID, ":") {
		panic("registerAction: id " + a.ID + " may not contain ':' (reserved for args)")
	}
	if _, dup := actionRegistry[a.ID]; dup {
		panic("registerAction: duplicate id " + a.ID)
	}
	cp := a
	actionRegistry[a.ID] = &cp
}

// resolveAction maps an invocation string ("id" or "id:arg") to its
// registered action. Arg-taking actions match only the colon form;
// NoArg actions only the bare id — same strictness the old prefix
// checks had.
func resolveAction(s string) (*Action, string, bool) {
	if a, ok := actionRegistry[s]; ok && a.Arg == NoArg {
		return a, "", true
	}
	if i := strings.IndexByte(s, ':'); i > 0 {
		if a, ok := actionRegistry[s[:i]]; ok && a.Arg != NoArg {
			return a, s[i+1:], true
		}
	}
	return nil, "", false
}

// KnownAction reports whether an invocation string resolves — the
// startup config validation uses it to catch typo'd keybind/menu
// references at load time instead of at press time.
func KnownAction(s string) bool {
	_, _, ok := resolveAction(s)
	return ok
}

func init() {
	// --- tabs ---
	registerAction(Action{ID: "new_tab", Label: "New Tab", Category: "tabs", Run: func(w *Window, _ string) {
		cols, rows := w.gridSize()
		// Inherit the currently active tab's CWD when the pref is on
		// so "New Tab" picks up wherever the user was working. Falls
		// through to xerotty's CWD when there's no active tab (first
		// tab) or GetCWD returns "" (process gone, /proc lookup
		// failed, etc.).
		var cwd string
		if w.app.cfg.Tabs.InheritCWD {
			if parentTab := w.tabs.Active(); parentTab != nil && parentTab.Terminal != nil {
				cwd = parentTab.Terminal.GetCWD()
			}
		}
		if tab, err := w.tabs.NewTab(cols, rows, cwd); err == nil && tab != nil {
			// AutoSelectNewTabs only catches new tabs once the bar has prior
			// frame state. On the 1→2 transition (tab bar first appears) it
			// can't, so request an explicit switch to the new tab.
			w.tabSwitchReq = tab.ID
		}
	}})
	registerAction(Action{ID: "close_tab", Label: "Close Tab", Category: "tabs", Run: func(w *Window, _ string) {
		w.tabs.CloseActive()
		// macOS's NSWindow performClose: default-binds to Cmd+W and
		// fires AFTER our keybind handler, so without swallowing the
		// "close one tab" keypress also closes the whole window. The
		// flag is checked in the PlatformRequestClose path below. ~5
		// frames is enough to cover the latency between the keybind
		// firing and SDL3 surfacing the OS-level close event.
		w.swallowOSCloseFrames = 5
	}})
	registerAction(Action{ID: "next_tab", Label: "Next Tab", Category: "tabs", Run: func(w *Window, _ string) {
		w.tabs.Next()
		if t := w.tabs.Active(); t != nil {
			w.tabSwitchReq = t.ID
		}
	}})
	registerAction(Action{ID: "prev_tab", Label: "Previous Tab", Category: "tabs", Run: func(w *Window, _ string) {
		w.tabs.Prev()
		if t := w.tabs.Active(); t != nil {
			w.tabSwitchReq = t.ID
		}
	}})
	registerAction(Action{ID: "goto_tab", Label: "Go to Tab", Category: "tabs", Arg: IntArg, ArgHint: "tab number (1-based)", Run: func(w *Window, arg string) {
		if n, err := strconv.Atoi(arg); err == nil {
			w.tabs.GoTo(n)
			if t := w.tabs.Active(); t != nil {
				w.tabSwitchReq = t.ID
			}
		}
	}})
	registerAction(Action{ID: "rename_tab", Label: "Rename Tab", Category: "tabs", Run: func(w *Window, _ string) {
		if tab := w.tabs.Active(); tab != nil {
			w.renameBuffer = tab.DisplayTitle()
			w.renamingTab = true
			imgui.OpenPopupStr("Rename Tab")
		}
	}})

	// --- window ---
	registerAction(Action{ID: "new_window", Label: "New Window", Category: "window", Run: func(w *Window, _ string) {
		// Single-process multi-window: append a new Window to this
		// App's slice. The render loop in Run() picks it up next
		// frame and wraps it in an ImGui top-level window that
		// multi-viewport auto-promotes to its own OS window. Same
		// NSApplication on macOS = one Dock icon for N windows;
		// same WM_CLASS on Linux = one taskbar group. See
		// docs/MULTI_WINDOW_REFACTOR.md for the architectural why.
		w.app.spawnWindow()
	}})
	registerAction(Action{ID: "fullscreen", Label: "Fullscreen", Category: "window", Run: func(w *Window, _ string) {
		w.fullscreen = !w.fullscreen
		// Multi-window: target THIS Window's SDL_Window, not the
		// hidden carrier. SDL_GL_GetCurrentWindow() would silently
		// fullscreen the invisible carrier and look like a no-op.
		if h := w.sdlWindowHandle(); h != 0 {
			platform.SetFullscreen(h, w.fullscreen)
		}
	}})
	registerAction(Action{ID: "toggle_opacity", Label: "Toggle Opacity", Category: "window", Run: func(w *Window, _ string) {
		// Flip between the configured opacity and fully opaque. Opaque is
		// the screenshot-safe state: a translucent window blends whatever
		// is behind it, so a capture can leak other windows — toggle to
		// opaque before shooting. App-level so all windows flip together;
		// the per-Window opacity apply in Run() picks it up. PostWake
		// forces an immediate render so the change is visible at once.
		w.app.forceOpaque.Store(!w.app.forceOpaque.Load())
		platform.PostWake()
	}})
	registerAction(Action{ID: "quit", Label: "Quit", Category: "window", Run: func(w *Window, _ string) {
		// Whole-app exit, all windows. macOS gets this for free from
		// AppKit (Cmd+Q → NSApp.terminate → SDL_EVENT_QUIT); Linux
		// has no OS-level equivalent, so it's a bindable action
		// (default Ctrl+Shift+Q, the konsole/xfce4-terminal
		// convention). Same platform.Quit() path as the last-window
		// close, so daemon tabs detach cleanly and sessions survive.
		platform.Quit()
	}})

	// --- clipboard & links ---
	registerAction(Action{ID: "copy", Label: "Copy", Category: "clipboard", Run: func(w *Window, _ string) {
		// selectedText already applies cfg.Clipboard.TrimTrailingWhitespace
		// per-row via extractText, so no extra trimming here.
		text := w.selectedText()
		if text != "" {
			input.ClipboardWrite(text)
			// Push the copied text to every daemon we're attached
			// to so MCP agents reading get_clipboard see it (and
			// future OSC 52 reads from PTY children can return
			// it). Sending to multiple daemons is cheap and
			// keeps the user's clipboard view consistent across
			// local and remote sessions.
			w.app.broadcastClipboard(text)
		}
	}})
	registerAction(Action{ID: "paste", Label: "Paste", Category: "clipboard", Run: func(w *Window, _ string) {
		// Image-first: a screenshot copied via Cmd+Shift+4 etc.
		// goes to the daemon as raw bytes (which writes it to a
		// temp file the PTY child can read by path). Falls back
		// to text paste when the clipboard has no image. Lets
		// "paste a screenshot into Claude Code over SSH" Just
		// Work without OSC52 / base64 brittleness.
		if mime, data, err := input.ClipboardReadImage(); err == nil && len(data) > 0 {
			if tab := w.tabs.Active(); tab != nil && tab.Terminal != nil {
				if err := tab.Terminal.PasteImage(mime, "", data); err != nil {
					fmt.Fprintf(os.Stderr, "xerotty: image paste: %v\n", err)
				}
				return
			}
		}
		text, err := input.ClipboardRead()
		if err == nil && text != "" {
			w.pasteText(text)
		}
	}})
	registerAction(Action{ID: "paste_selection", Label: "Paste Selection", Category: "clipboard", Run: func(w *Window, _ string) {
		text, err := input.PrimaryRead()
		if err == nil && text != "" {
			w.pasteText(text)
		}
	}})
	registerAction(Action{ID: "select_all", Label: "Select All", Category: "clipboard", Run: func(w *Window, _ string) {
		if tab := w.tabs.Active(); tab != nil {
			cols := tab.Terminal.Emulator().Width()
			rows := tab.Terminal.Emulator().Height()
			w.sel.startCol = 0
			w.sel.startRow = 0
			w.sel.endCol = cols - 1
			w.sel.endRow = rows - 1
			w.sel.active = true
			w.sel.dragging = false
		}
	}})
	registerAction(Action{ID: "open_link", Label: "Open Link", Category: "links", Run: func(w *Window, _ string) {
		if w.hoveredLink != nil {
			openURL(w.hoveredLink.URL, w.app.cfg.Links.Opener)
		}
	}})
	registerAction(Action{ID: "copy_link", Label: "Copy Link", Category: "links", Run: func(w *Window, _ string) {
		if w.hoveredLink != nil {
			input.ClipboardWrite(w.hoveredLink.URL)
		}
	}})

	// --- scrollback ---
	registerAction(Action{ID: "scroll_page_up", Label: "Scroll Page Up", Category: "scrollback", Run: func(w *Window, _ string) {
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			_, rows := w.gridSize()
			s.PageUp(rows, tab.Terminal.ScrollbackLen())
		}
	}})
	registerAction(Action{ID: "scroll_page_down", Label: "Scroll Page Down", Category: "scrollback", Run: func(w *Window, _ string) {
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			_, rows := w.gridSize()
			s.PageDown(rows)
		}
	}})
	registerAction(Action{ID: "scroll_top", Label: "Scroll to Top", Category: "scrollback", Run: func(w *Window, _ string) {
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			s.Offset = tab.Terminal.ScrollbackLen()
		}
	}})
	registerAction(Action{ID: "scroll_bottom", Label: "Scroll to Bottom", Category: "scrollback", Run: func(w *Window, _ string) {
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			s.Reset()
		}
	}})
	registerAction(Action{ID: "search", Label: "Search Scrollback", Category: "scrollback", Run: func(w *Window, _ string) {
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			s.OpenSearch()
			w.searchFocusInput = true
		}
	}})
	registerAction(Action{ID: "clear_scrollback", Label: "Clear Scrollback", Category: "scrollback", Run: func(w *Window, _ string) {
		if tab := w.tabs.Active(); tab != nil {
			tab.Terminal.ClearScrollback()
			if s, ok := w.scroll[tab.ID]; ok {
				s.Reset()
			}
		}
	}})

	// --- terminal ---
	registerAction(Action{ID: "reset_terminal", Label: "Reset Terminal", Category: "terminal", Run: func(w *Window, _ string) {
		if tab := w.tabs.Active(); tab != nil {
			// Send RIS (Reset to Initial State) escape sequence
			tab.Terminal.Write([]byte("\x1bc"))
			tab.Terminal.ClearScrollback()
			if s, ok := w.scroll[tab.ID]; ok {
				s.Reset()
			}
			w.sel.clear()
		}
	}})

	// --- font ---
	registerAction(Action{ID: "font_size_up", Label: "Increase Font Size", Category: "font", Run: func(w *Window, _ string) {
		// Per-window zoom — only this Window's font size changes.
		// Other Windows keep their own zoom level (iTerm2-style).
		w.fontSize += 1
		w.updateFontMetrics()
	}})
	registerAction(Action{ID: "font_size_down", Label: "Decrease Font Size", Category: "font", Run: func(w *Window, _ string) {
		if w.fontSize > 6 {
			w.fontSize -= 1
			w.updateFontMetrics()
		}
	}})
	registerAction(Action{ID: "font_size_reset", Label: "Reset Font Size", Category: "font", Run: func(w *Window, _ string) {
		// Reset to the configured default for this Window only.
		w.fontSize = renderer.PixelSize(&w.app.cfg)
		w.updateFontMetrics()
	}})

	// --- config ---
	registerAction(Action{ID: "preferences", Label: "Preferences", Category: "config", Run: func(w *Window, _ string) {
		w.openPreferences()
	}})
	registerAction(Action{ID: "set_theme", Label: "Set Theme", Category: "config", Arg: StringArg, ArgHint: "theme name", Run: func(w *Window, arg string) {
		if t, err := themes.Load(arg); err == nil {
			applyColorOverrides(&t, &w.app.cfg)
			w.app.theme = t
			// Theme is process-wide: every Window's renderer needs
			// the new palette or peer Windows render against the
			// stale one until they're individually re-themed. Same
			// loop applyPreferences uses for the prefs-driven path.
			for _, win := range w.app.windows {
				if win.renderer != nil {
					win.renderer.Theme = t
					win.renderer.InvalidateCellCache()
				}
			}
			// Update SDL background color to match new theme.
			bgR := float32((t.Background>>0)&0xFF) / 255.0
			bgG := float32((t.Background>>8)&0xFF) / 255.0
			bgB := float32((t.Background>>16)&0xFF) / 255.0
			platform.SetBgColor(imgui.NewVec4(bgR, bgG, bgB, 1.0))
		}
	}})

	// --- remote ---
	registerAction(Action{ID: "new_tab_remote", Label: "New Remote Tab", Category: "remote", Arg: StringArg, ArgHint: "host alias", Run: func(w *Window, arg string) {
		if err := w.openRemoteTab(arg); err != nil {
			fmt.Fprintf(os.Stderr, "xerotty: new_tab_remote %s: %v\n", arg, err)
		}
	}})
	registerAction(Action{ID: "attach_remote", Label: "Attach Remote Tabs", Category: "remote", Arg: StringArg, ArgHint: "host alias", Run: func(w *Window, arg string) {
		if err := w.openRemoteReattach(arg); err != nil {
			fmt.Fprintf(os.Stderr, "xerotty: attach_remote %s: %v\n", arg, err)
		}
	}})
	registerAction(Action{ID: "connect_remote", Label: "Connect to Host…", Category: "remote", Run: func(w *Window, _ string) {
		w.openConnectDialog()
	}})
	registerAction(Action{ID: "remote_new_tab", Label: "New Tab on This Host", Category: "remote", Run: func(w *Window, _ string) {
		runRemoteOnActiveHost(w, "remote_new_tab", w.openRemoteTab)
	}})
	registerAction(Action{ID: "remote_new_window", Label: "New Window on This Host", Category: "remote", Run: func(w *Window, _ string) {
		runRemoteOnActiveHost(w, "remote_new_window", w.openRemoteWindow)
	}})
	registerAction(Action{ID: "kick_client", Label: "Disconnect Client", Category: "remote", Arg: StringArg, ArgHint: "hub:clientID", Run: func(w *Window, arg string) {
		// ClientIDs may themselves contain colons ("xerotty-gui:xryzen"),
		// so split off the hub name only.
		parts := strings.SplitN(arg, ":", 2)
		if len(parts) != 2 {
			return
		}
		hub := w.app.hubsByName()[parts[0]]
		if hub == nil {
			fmt.Fprintf(os.Stderr, "xerotty: kick_client: no hub %q\n", parts[0])
			return
		}
		if err := hub.KickClient(parts[1]); err != nil {
			fmt.Fprintf(os.Stderr, "xerotty: kick_client %s: %v\n", arg, err)
		}
		// Re-fetch soon so the menu reflects the kick on next open.
		w.app.refreshClientsMenu()
	}})

	// --- custom ---
	registerAction(Action{ID: "exec", Label: "Run Command", Category: "custom", Arg: StringArg, ArgHint: "shell command", Run: func(w *Window, arg string) {
		ctx := w.menuContext()
		// ExecAction parses the full "exec:<cmd>" form itself.
		menu.ExecAction("exec:"+arg, ctx)
	}})
}

// runRemoteOnActiveHost shares the remote_new_tab / remote_new_window
// body: act on the host of the CURRENTLY active tab — a new tab or
// window on the same remote box you're looking at. No-op (with a
// note) when the active tab is local; the plain new_tab/new_window
// actions cover the local case.
func runRemoteOnActiveHost(w *Window, name string, open func(string) error) {
	t := w.tabs.Active()
	if t == nil || t.Host == "" {
		fmt.Fprintf(os.Stderr, "xerotty: %s: active tab is not on a remote host\n", name)
		return
	}
	if err := open(t.Host); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty: %s %s: %v\n", name, t.Host, err)
	}
}

// validateActionRefs warns (stderr, once at startup) about keybind
// and menu references that resolve to no registered action — a typo
// in config.toml used to be a silently dead key or menu item
// discovered at press time; now it is named at load time. "separator"
// is the menu's separator convention, and submenu parents carry no
// action of their own.
func (a *App) validateActionRefs() {
	for chord, act := range a.cfg.Keybinds {
		if !KnownAction(act) {
			fmt.Fprintf(os.Stderr, "xerotty: [keybinds] %s -> unknown action %q\n", chord, act)
		}
	}
	var walk func(items []config.MenuItem, path string)
	walk = func(items []config.MenuItem, path string) {
		for _, it := range items {
			if len(it.Submenu) > 0 {
				walk(it.Submenu, path+it.Label+" > ")
				continue
			}
			// "separator" and "_"-prefixed expansion placeholders
			// (e.g. _remote_hosts, replaced by expandMenu at render
			// time) are menu grammar, not actions.
			if it.Action == "" || it.Action == "separator" || strings.HasPrefix(it.Action, "_") {
				continue
			}
			if !KnownAction(it.Action) {
				fmt.Fprintf(os.Stderr, "xerotty: menu item %s%q -> unknown action %q\n", path, it.Label, it.Action)
			}
		}
	}
	walk(a.cfg.Menu.Items, "")
}
