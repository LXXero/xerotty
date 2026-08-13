// Package app handles the SDL2/ImGui lifecycle and main render loop.
package app

import (
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/fontsys"
	"github.com/LXXero/xerotty/internal/glyphcache"
	"github.com/LXXero/xerotty/internal/guimcp"
	"github.com/LXXero/xerotty/internal/input"
	"github.com/LXXero/xerotty/internal/launchipc"
	"github.com/LXXero/xerotty/internal/menu"
	"github.com/LXXero/xerotty/internal/platform"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/renderer"
	"github.com/LXXero/xerotty/internal/scrollback"
	"github.com/LXXero/xerotty/internal/sdlhack"
	"github.com/LXXero/xerotty/internal/sockpath"
	"github.com/LXXero/xerotty/internal/tabs"
	"github.com/LXXero/xerotty/internal/terminal"
	"github.com/LXXero/xerotty/internal/themes"
)

// disableMirror is a runtime kill switch for the macOS mouse-event
// mirror — set XEROTTY_DISABLE_MIRROR=1 to shut it off entirely. Kept
// because the mirror is the kind of fragile workaround we want a way
// to disable if a future SDL/cimgui-go upgrade fixes the underlying
// Cocoa event-drop bug and the mirror becomes net-negative.
var disableMirror = os.Getenv("XEROTTY_DISABLE_MIRROR") != ""

func init() {
	runtime.LockOSThread()
}

// App is the main application struct. It owns process-wide state
// (config, theme, base font metrics, font-reload flags) plus the
// slice of Windows in this process. One xerotty process can host
// multiple OS windows (Cmd+N / "new_window") — see
// docs/MULTI_WINDOW_REFACTOR.md for why this matters on macOS Dock
// coalescing.
type App struct {
	cfg             config.Config
	theme           renderer.Theme
	glowStop        chan struct{} // lava-lamp self-wake ticker (glow.go)
	baseFontSize    float32       // font size the atlas was built at
	baseCellW       float32       // cell width at base font size
	baseCellH       float32       // cell height at base font size
	pendingFontFace bool          // rebuild font atlas at start of next frame

	// forceOpaque overrides cfg.Appearance.Opacity to 1.0 while set.
	// Toggled by the "toggle_opacity" action (keybind / menu) so the
	// window can be made provably leak-free before a screenshot — a
	// translucent window blends whatever's behind it. Read each frame by
	// the per-Window opacity apply in Run().
	forceOpaque atomic.Bool

	// menuActivityFrame is the main-loop FrameCount on which some
	// Window opened its context menu. Opening a menu runs a blocking
	// popup that grabs the OS pointer, so by the time the NEXT Window's
	// frame() checks MouseFocusWindowID (same main frame — the popup
	// uses a separate ImGui context, FrameCount unchanged) the focus
	// has moved to it and it would open a second menu. Gating opens on
	// "FrameCount != menuActivityFrame" keeps it one-menu-per-frame.
	menuActivityFrame int

	// Daemon-source plumbing — only populated when cfg.Tabs.Source
	// == "daemon". hub owns the connection + frame router; each
	// Window builds its own tabs.Manager.SourceFactory via
	// installSourceFactory so multi-window setups route NewTab
	// calls to the right daemon window.
	//
	// daemonMu guards daemonHub + daemonHubName + guiMCP: read
	// from the guimcp server's request goroutines (ListTabs /
	// SourceFor) and written from init/reconnect paths
	// (activeDaemonHub re-dial). daemonHubName is the default
	// hub's host namespace ("local", or a cfg.Hosts name in
	// daemon:<name> mode) — so the aggregated MCP doesn't list a
	// remote-default daemon's tabs as both "local:" and "<host>:".
	daemonMu      sync.Mutex
	daemonHub     *daemonsource.Hub
	daemonHubName string

	// daemonAdoptQueue holds windows + their tabs the daemon
	// reported at Attach time, in the order they came back. Each
	// GUI Window that opens in daemon mode drains one entry —
	// adopting its tabs AND remembering its daemon-window ID so
	// future tab creates / geometry / focus updates target the
	// right server-side window. Empty queue → new GUI windows
	// spawn fresh daemon windows via SendWindowCreate.
	daemonAdoptQueue []daemonWindowSnapshot

	// remoteHubs is the lazy-built per-host registry of SSH-backed
	// daemon hubs. Keyed by RemoteHost.Name. Populated on the
	// first menu action / tab creation that targets a named host;
	// reused for subsequent tabs to that same host so a single SSH
	// connection serves many tabs.
	remoteHubsMu sync.Mutex
	remoteHubs   map[string]*remoteHubEntry

	// adhocHosts holds ad-hoc "Connect to host…" targets the user
	// typed this session — connections NOT backed by a [[hosts]]
	// bookmark. Keyed by the same string used as the hub registry
	// key + Tab.Host badge (the typed destination, e.g. "user@kh").
	// resolveRemote consults this so re-dials after an SSH drop can
	// recover the dest/args without a cfg entry. Main-thread only
	// (touched from the connect dialog + remoteHubFor, both on the
	// UI goroutine), so it shares no lock with remoteHubs.
	adhocHosts map[string]config.RemoteHost

	// windowSeq is a monotonic counter for assigning each Window's
	// stable ImGui ID (imguiName). It must NEVER be derived from
	// len(a.windows): closing a window removes it from a.windows but
	// does NOT make ImGui forget that ID's in-memory ImGuiWindow (its
	// last size lives on until the context is destroyed — NoSavedSettings
	// only suppresses the .ini, not the in-session state). A len-based
	// name reuses the ID of a previously-closed window, so the next
	// spawn's SetNextWindowSize(CondFirstUseEver) is treated as "not the
	// first use" and the window opens at the stale remembered size
	// instead of the configured cols×rows. A counter that only ever
	// increments keeps every spawn a fresh ImGui identity. Main is 0.
	windowSeq int

	// suppressInitialTab tells spawnWindowImpl to skip ALL tab
	// creation for the next spawn — used by spawnEmptyWindow when
	// the caller will adopt tabs into the window itself (remote
	// reattach). Reset to false right after.
	suppressInitialTab bool

	// spawnCWD, when non-empty, overrides the first tab's starting
	// directory for the next spawnWindowImpl call — used by forwarded
	// single-instance launches (internal/launchipc) so the new
	// window's shell opens in the INVOKING shell's directory, not
	// the GUI's. Reset to "" right after, like suppressInitialTab.
	spawnCWD string

	// spawnCmd, when non-nil, makes the NEXT window's (or the initial
	// window's) first tab run that command instead of the shell — the
	// `-e`/`-x` launch feature, carried in over launchipc or set before
	// the cold-start window. One-shot: consumed and cleared by the
	// first tab spawn so later tabs/windows get normal shells.
	spawnCmd *terminal.LaunchCmd

	// launchQueue holds forwarded single-instance launch requests
	// queued by the launch-socket accept goroutine and drained on the
	// main thread once per frame (drainLaunchRequests) — window/tab
	// creation touches ImGui/SDL state only the frame loop may own.
	launchMu    sync.Mutex
	launchQueue []launchipc.Request

	// pendingProposals is the propose-mode queue across ALL hubs,
	// each entry tagged with its originating hub + host name so
	// Approve/Drop resolves against the RIGHT daemon (not just the
	// local/default one). Guarded by proposalsMu (written from hub
	// router goroutines, read from the render thread).
	proposalsMu      sync.Mutex
	pendingProposals []guiProposal

	// Clients-menu cache: last-known attached-client snapshot per hub
	// display name ("local" + remote host names). The right-click
	// menu builds from this synchronously and fires an async refresh
	// on every open — a remote hub's round-trip must never block the
	// menu, so the first open after a change may show the previous
	// snapshot and the next open catches up.
	clientsMenuMu   sync.Mutex
	clientsMenuData map[string][]protocol.ClientInfo
	clientsMenuBusy atomic.Bool

	// Clipboard-push throttle: a remote PTY's OSC 52 GET is
	// answered server-side from the session clipboard, which is
	// only fresh if the GUI pushed recent clipboard contents. We
	// poll the OS clipboard on a slow timer and push on change so
	// "copied in another app → OSC 52 read in a remote shell"
	// returns current data, not just what xerotty itself copied.
	lastClipboardPush string
	lastClipboardPoll time.Time

	// guiMCP is the GUI's aggregating MCP server (one socket
	// covering every daemon hub). Started lazily the first time a
	// daemon hub comes up. nil in PTY mode.
	guiMCP *guimcp.Server

	windows []*Window // every OS window currently open in this process
	active  *Window   // the window with input focus, or windows[0] if none

	// focusedSources is a snapshot of the daemon Sources that are the
	// active tab of some window, republished once per frame on the
	// main thread. The guimcp Backend (a separate goroutine) reads it
	// via an atomic load instead of walking a.windows / each window's
	// tabs.Manager — both of which are main-thread-only structures
	// (public Tabs/ActiveIdx fields, no internal lock) that the UI
	// mutates without synchronization. Publishing a snapshot keeps
	// those structures single-threaded and gives MCP a race-free read.
	focusedSources atomic.Pointer[map[*daemonsource.Source]bool]

	// pendingTopo holds the latest topology snapshot per hub awaiting a
	// main-thread reconcile. The Hub fires SetTopologyCallback on its
	// router goroutine; GUI tab/window mutation must happen on the main
	// thread, so the callback just stashes here (latest wins) + wakes
	// the loop, and applyPendingTopology drains it each frame.
	topoMu      sync.Mutex
	pendingTopo map[*daemonsource.Hub]pendingTopoSnap

	// dragTab is the process-wide state for an in-progress drag of a
	// tab across windows. nil when no drag is happening. Set when the
	// user drags a tab down past the tab bar (lift-off threshold)
	// from any Window's tab bar; the source Window has already
	// RemoveTab'd the entry by then so the Terminal lives only in
	// dragTab.Term until release. The app resolves the final target
	// once per frame after all Windows have rendered.
	dragTab *tabDrag

	// prevOsLeftDown is the OS-level left mouse button state observed
	// at the end of the previous frame's mouse-mirror check. The mirror
	// injects events on TRANSITIONS of this value rather than diffing
	// against ImGui's IsMouseDown — IsMouseDown can lag behind the
	// real OS state during macOS focus shifts (the exact scenario the
	// mirror exists to compensate for), which causes the mirror to
	// skip injection when ImGui's view spuriously already matches a
	// half-applied OS state.
	prevOsLeftDown bool

	// anyWindowMovingThisFrame is set at the top of wrappedFrame
	// if any Window's NSWindow.frame.origin changed since last
	// frame (i.e. the OS is dragging the window). Read by the
	// mouse mirror's deferred-DOWN logic to abort an
	// about-to-be-injected synthetic DOWN whose source turns out
	// to be a window-drag gesture rather than a content click.
	anyWindowMovingThisFrame bool

	// idleWakeMs is the shortest time (ms) until the render loop next
	// NEEDS to wake while idle — reset to idleSafetyNetMs at the top of
	// wrappedFrame and lowered to the next cursor-blink toggle while
	// drawing a blinking cursor. Pushed to the platform via
	// SetIdleTimeout at the end of the frame so an idle screen parks at
	// ~0% CPU yet a blinking cursor keeps ticking.
	idleWakeMs int

	// mirrorPendingDown is set on the OS button-down edge — but
	// instead of injecting the synthetic DOWN immediately we wait
	// a few frames. If the window starts moving OR the cursor
	// moves substantially during the wait, the click was a drag
	// gesture (window drag or selection drag) and should be
	// aborted (the latter already had its DOWN delivered through
	// SDL; the former should never inject). Otherwise inject after
	// the timer expires.
	mirrorPendingDown       bool
	mirrorPendingPos        imgui.Vec2
	mirrorPendingGlobalX    int
	mirrorPendingGlobalY    int
	mirrorPendingFramesLeft int
}

// tabDrag is the in-flight state of a tab being dragged across
// Windows. The Terminal lives ONLY inside Term while drag is
// active — source Window's tabs.Manager has already RemoveTab'd
// it. On release, either the cursor's-over Window adopts the
// Terminal (intra-process Window-to-Window move) or a new Window
// gets spawned and adopts it (detach-to-new).
//
// LastFocus is only a fallback signal. On Wayland the compositor's
// implicit pointer grab can keep it stuck on the source Window, so a
// Wayland data-device drop target or geometry hit-test wins first.
type tabDrag struct {
	Term               terminal.Source
	Label              string  // for floating-preview rendering
	Title              string  // OSC title carried across the drag (AdoptTab starts empty)
	Host               string  // remote-host badge, ditto
	From               *Window // window the tab originated in; used to reject stale source focus
	LastFocus          uintptr // SDL_WindowID last seen under the cursor during the drag
	WaylandStarted     bool
	WaylandDropSeen    bool
	WaylandDropSurface uintptr
}

// New creates a new App with the given config. The initial Window
// struct is allocated here; its SDL_Window, GL context, tabs, and
// renderer are populated during Run() once the cimgui-go backend is
// initialized.
func New(cfg config.Config) *App {
	a := &App{cfg: cfg}
	// Tab-source modes:
	//   "" / "pty"      — in-process PTY (default)
	//   "daemon"        — local auto-spawned daemon
	//   "daemon:<name>" — remote daemon from cfg.Hosts[<name>]
	src := cfg.Tabs.Source
	switch {
	case src == "daemon":
		if err := a.initDaemonSource(); err != nil {
			fmt.Fprintf(os.Stderr, "xerotty: daemon mode requested but unavailable, falling back to in-process PTY: %v\n", err)
		}
	case strings.HasPrefix(src, "daemon:"):
		host := strings.TrimPrefix(src, "daemon:")
		if err := a.initRemoteDefaultSource(host); err != nil {
			fmt.Fprintf(os.Stderr, "xerotty: daemon:%s requested but unavailable, falling back to in-process PTY: %v\n", host, err)
		}
	}
	w := newWindow(a)
	a.windows = append(a.windows, w)
	a.active = w
	return a
}

// initRemoteDefaultSource is like initDaemonSource but uses a
// remote hub (lazily SSH-dialed via cfg.Hosts) as the app-wide
// daemonHub. Every new window's source factory routes through it,
// so default-tab creation lands on the remote box. Used when the
// user sets cfg.Tabs.Source = "daemon:<name>".
//
// Unlike the local daemon path, no daemon auto-spawn happens
// locally — the remote box is expected to have xerotty serve
// reachable (the SSH bridge auto-spawns its persistent daemon).
func (a *App) initRemoteDefaultSource(name string) error {
	entry, err := a.remoteHubFor(name)
	if err != nil {
		return err
	}
	// Move the remote's window snapshots into the app-wide adopt
	// queue. spawnWindowImpl drains it on each new GUI window so
	// multi-window remote layouts restore properly.
	a.remoteHubsMu.Lock()
	a.daemonAdoptQueue = append(a.daemonAdoptQueue, entry.reattachQueue...)
	entry.reattachQueue = nil
	a.remoteHubsMu.Unlock()

	// Key the default hub by the host name, NOT "local" — in
	// daemon:<name> mode the default daemon IS the remote, and
	// the aggregated MCP must list its tabs once (as "<host>:"),
	// not also as "local:".
	a.setDaemonHub(entry.hub, name)
	a.configureHubScrollback(entry.hub)
	return nil
}

// scrollbackWindower is implemented by daemon Sources running in
// sliding-window mode. The frame loop calls EnsureScrollbackWindow so
// the cached window tracks the viewport; in-process terminals (full
// local scrollback) don't implement it.
type scrollbackWindower interface {
	EnsureScrollbackWindow(from, to int)
}

// scrollbackSearcher is implemented by daemon Sources: the deep
// history they don't mirror is searched on the daemon. The reply
// carries ABSOLUTE scrollback line indices (see runScrollbackSearch).
type scrollbackSearcher interface {
	RequestScrollbackSearch(query string, caseSensitive, regex, wholeWord bool) uint64
	TakeSearchResults() (reqID uint64, matches []protocol.SearchMatch, truncated, ok bool)
}

// daemonSearchState tracks one tab's in-flight daemon search so the
// frame loop re-requests only when inputs change, and re-homes match
// coordinates as the scrollback total grows (without re-querying).
type daemonSearchState struct {
	query     string
	caseSens  bool
	regex     bool
	word      bool
	reqID     uint64
	absMatch  []protocol.SearchMatch // raw daemon results (absolute lines)
	haveRes   bool
	lastTotal int // scrollback total at last coordinate conversion
}

// ensureDaemonSearch drives server-side search for a windowed daemon
// tab: (re)issue the query when inputs change, adopt replies, and
// convert ABSOLUTE daemon line indices into the renderer's
// screen-relative match space (Line<0 scrollback, >=0 screen) — using
// the CURRENT total so matches stay correct as history grows, with no
// extra round-trips.
func (a *Window) ensureDaemonSearch(tabID int, s *scrollback.State, term terminal.Source, searcher scrollbackSearcher, visRows int) {
	ds := a.daemonSearch[tabID]
	if ds == nil || ds.query != s.Query || ds.caseSens != s.CaseSensitive ||
		ds.regex != s.UseRegex || ds.word != s.WholeWord {
		if ds == nil {
			ds = &daemonSearchState{}
			a.daemonSearch[tabID] = ds
		}
		ds.query, ds.caseSens, ds.regex, ds.word = s.Query, s.CaseSensitive, s.UseRegex, s.WholeWord
		ds.reqID = searcher.RequestScrollbackSearch(s.Query, s.CaseSensitive, s.UseRegex, s.WholeWord)
		ds.absMatch, ds.haveRes = nil, false
		s.SetMatches(nil, visRows) // clear stale highlights immediately
		s.SearchPending = true
		s.Truncated = false
	}
	if reqID, matches, truncated, ok := searcher.TakeSearchResults(); ok && reqID == ds.reqID {
		ds.absMatch, ds.haveRes, ds.lastTotal = matches, true, -1
		s.SearchPending = false
		s.Truncated = truncated
	}
	if ds.haveRes {
		if total := term.ScrollbackLen(); total != ds.lastTotal {
			ds.lastTotal = total
			conv := make([]scrollback.Match, len(ds.absMatch))
			for i, m := range ds.absMatch {
				conv[i] = scrollback.Match{Line: int(m.Line) - total, Col: int(m.Col), Len: int(m.Len)}
			}
			s.SetMatches(conv, visRows)
		}
	}
}

// configureHubScrollback sets a daemon hub's client-side scrollback
// strategy from the user's config. "unlimited" mode uses a sliding
// WINDOW — the GUI mirrors only a bounded window and fetches other
// ranges from the daemon on demand — because a full in-memory mirror
// of unlimited history is gigabytes per tab (the 124 GB-OOM bug). A
// fixed line count uses a plain full mirror capped at that count
// (bounded and small).
func (a *App) configureHubScrollback(hub *daemonsource.Hub) {
	if a.cfg.Scrollback.Mode == "unlimited" {
		hub.SetScrollbackWindowed()
		return
	}
	if n := a.cfg.Scrollback.Lines; n > 0 {
		hub.SetScrollbackCap(n)
	}
}

// remoteHubEntry pairs a per-host Hub with any windows+tabs the
// remote daemon reported at attach time but the GUI hasn't adopted
// yet. reattachQueue preserves the daemon's window grouping +
// per-window tab order + FocusedTabID so the user's remote layout
// reappears intact (not flattened into one undifferentiated list).
type remoteHubEntry struct {
	hub           *daemonsource.Hub
	reattachQueue []daemonWindowSnapshot
}

// pollClipboardForDaemons reads the OS clipboard at most once a
// second and, when it changed since the last push, broadcasts it
// to every daemon. Keeps the daemon-side session clipboard fresh
// enough that a remote PTY app's OSC 52 GET returns what's
// actually on the user's clipboard — including text copied in a
// different app — not just xerotty's own last copy. No-op when no
// daemon hub is active. Called from the render loop; the time
// gate keeps the SDL clipboard read off the per-frame hot path.
func (a *App) pollClipboardForDaemons() {
	now := time.Now()
	if now.Sub(a.lastClipboardPoll) < time.Second {
		return
	}
	a.lastClipboardPoll = now
	// Any hub (default OR ad-hoc remote) wants fresh clipboard for
	// OSC 52 GET — broadcastClipboard pushes to all of them. Don't
	// gate on the default hub alone, or PTY-default mode with
	// ad-hoc remote tabs would never push.
	if len(a.hubsByName()) == 0 {
		return
	}
	text, err := input.ClipboardRead()
	if err != nil || text == "" {
		return
	}
	if text == a.lastClipboardPush {
		return
	}
	a.lastClipboardPush = text
	a.broadcastClipboard(text)
}

// broadcastClipboard pushes the given text to every daemon Hub
// this app talks to (local + every remote in remoteHubs).
// Daemon's MCP get_clipboard returns the most recently received
// text per session; broadcasting keeps copy/paste consistent
// across local + remote tabs.
func (a *App) broadcastClipboard(text string) {
	if a.daemonHub != nil {
		_ = a.daemonHub.Client().SendClipboardData(text)
	}
	a.remoteHubsMu.Lock()
	hubs := make([]*daemonsource.Hub, 0, len(a.remoteHubs))
	for _, e := range a.remoteHubs {
		hubs = append(hubs, e.hub)
	}
	a.remoteHubsMu.Unlock()
	for _, h := range hubs {
		_ = h.Client().SendClipboardData(text)
	}
}

// cursorStyleName maps the vt cursor shape enum (0=block,
// 1=underline, 2=bar) to the renderer's cursor-style string.
// This is the vt/protocol enum, NOT raw DECSCUSR codes — the
// daemon + terminal both normalize to the vt enum before it
// reaches here.
func cursorStyleName(style uint8) string {
	switch style {
	case 1:
		return "underline"
	case 2:
		return "bar"
	default: // 0 = block
		return "block"
	}
}

// expandMenu walks the configured menu items and replaces magic
// placeholders with synthesized items:
//
//	"_remote_hosts" — expands into a "Remote Hosts" submenu
//	                  listing each cfg.Hosts entry with two child
//	                  items: "New tab on <host>" and "Reattach
//	                  <host>". If cfg.Hosts is empty the
//	                  placeholder collapses to nothing so the
//	                  menu doesn't show a useless empty entry.
//
// Other items pass through untouched. Default menu config
// includes the placeholder so users get host entries
// automatically once they add [[hosts]] to their config.
// refreshClientsMenu re-fetches each hub's attached-client list into
// clientsMenuData off the UI thread. One fetch in flight at a time;
// menu opens just read the cache. PostWake repaints an open menu's
// next frame... which rebuilds from config, so in practice the NEXT
// open shows the fresh data — fine for a diagnostic menu.
func (a *App) refreshClientsMenu() {
	if !a.clientsMenuBusy.CompareAndSwap(false, true) {
		return
	}
	hubs := a.hubsByName()
	go func() {
		defer a.clientsMenuBusy.Store(false)
		fresh := make(map[string][]protocol.ClientInfo, len(hubs))
		for name, hub := range hubs {
			infos, err := hub.Clients(2 * time.Second)
			if err != nil {
				// Old daemon / dead hub: leave the entry out rather
				// than show stale ghosts.
				continue
			}
			fresh[name] = infos
		}
		a.clientsMenuMu.Lock()
		a.clientsMenuData = fresh
		a.clientsMenuMu.Unlock()
		platform.PostWake()
	}()
}

// clientsSubmenu builds the Remote → Clients entries from the cached
// snapshot: one line per attached client per hub, click = kick. The
// requester's own GUI is labeled — kicking yourself just forces a
// reconnect, but you should know you're doing it.
func (a *App) clientsSubmenu() []config.MenuItem {
	a.clientsMenuMu.Lock()
	data := a.clientsMenuData
	a.clientsMenuMu.Unlock()
	names := make([]string, 0, len(data))
	for n := range data {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []config.MenuItem
	for _, name := range names {
		for _, ci := range data[name] {
			label := name + ": " + ci.ClientID
			if ci.LastPongAgoSec >= 10 {
				label += fmt.Sprintf(" (pong %ds ago)", ci.LastPongAgoSec)
			}
			if ci.You {
				label += " (you)"
			}
			out = append(out, config.MenuItem{
				Label:  "Disconnect " + label,
				Action: "kick_client:" + name + ":" + ci.ClientID,
			})
		}
	}
	if len(out) == 0 {
		out = append(out, config.MenuItem{Label: "(no clients — refreshing)"})
	}
	return out
}

func (a *App) expandMenu(items []config.MenuItem) []config.MenuItem {
	out := make([]config.MenuItem, 0, len(items))
	for _, item := range items {
		if item.Action == "_remote_hosts" {
			submenu := make([]config.MenuItem, 0, len(a.cfg.Hosts)*2+2)
			// Ad-hoc connect is always offered, even with no
			// [[hosts]] bookmarks — the GUI no longer requires a
			// config entry to reach a remote box.
			submenu = append(submenu, config.MenuItem{
				Label:  "Connect to host...",
				Action: "connect_remote",
			})
			// New tab / window on the host of the CURRENT remote tab —
			// "give me another shell on the box I'm already in". Act on
			// the active tab's host at dispatch time, so they need no
			// per-host entries and work for ad-hoc connections too.
			submenu = append(submenu, config.MenuItem{
				Label:  "New Tab (current host)",
				Action: "remote_new_tab",
			})
			submenu = append(submenu, config.MenuItem{
				Label:  "New Window (current host)",
				Action: "remote_new_window",
			})
			for _, h := range a.cfg.Hosts {
				submenu = append(submenu, config.MenuItem{Action: "separator"})
				submenu = append(submenu, config.MenuItem{
					Label:  "New tab on " + h.Name,
					Action: "new_tab_remote:" + h.Name,
				})
				submenu = append(submenu, config.MenuItem{
					Label:  "Reattach " + h.Name,
					Action: "attach_remote:" + h.Name,
				})
			}
			// Attached clients across every hub, with disconnect —
			// the "who else is on my session (and kick the dozing
			// laptop)" view. Built from cache; refreshed async.
			a.refreshClientsMenu()
			submenu = append(submenu, config.MenuItem{Action: "separator"})
			submenu = append(submenu, config.MenuItem{
				Label:   "Clients",
				Submenu: a.clientsSubmenu(),
			})
			out = append(out, config.MenuItem{
				Label:   "Remote",
				Submenu: submenu,
			})
			continue
		}
		// Recurse so submenus can include the placeholder too.
		if len(item.Submenu) > 0 {
			cp := item
			cp.Submenu = a.expandMenu(item.Submenu)
			out = append(out, cp)
			continue
		}
		// Shortcut labels derive from the live keybinds so they can
		// never drift from the actual bindings (see
		// config.ShortcutForAction). An explicit Shortcut in a user
		// config wins.
		if item.Shortcut == "" && item.Action != "" {
			item.Shortcut = config.ShortcutForAction(a.cfg.Keybinds, item.Action)
		}
		out = append(out, item)
	}
	return out
}

// windowIDForHub returns this Window's daemon-window ID on the
// given hub, lazily creating a daemon window via SendWindowCreate
// if no association exists yet. Returns 0 + false on failure
// (hub closed, no response within timeout). Callers use 0 as
// "use the daemon's default window" so a partial-failure mode is
// graceful.
//
// Solves the cross-hub ID confusion: when a window contains tabs
// daemonWindowForHub returns this Window's known daemon-window ID on
// the given hub WITHOUT creating one (0 if unknown). Read-only
// counterpart to windowIDForHub, used by topology placement to match
// a snapshot window to a GUI window.
func (w *Window) daemonWindowForHub(hub *daemonsource.Hub) uint32 {
	w.daemonWindowIDsMu.Lock()
	defer w.daemonWindowIDsMu.Unlock()
	if id, ok := w.daemonWindowIDs[hub]; ok {
		return id
	}
	if hub == w.app.daemonHub {
		return w.daemonWindowID
	}
	return 0
}

// from multiple daemons, focus/reorder operations need to send
// the WINDOW ID FOR THAT TAB'S DAEMON, not the primary one.
func (w *Window) windowIDForHub(hub *daemonsource.Hub) uint32 {
	if hub == nil {
		return 0
	}
	w.daemonWindowIDsMu.Lock()
	if id, ok := w.daemonWindowIDs[hub]; ok {
		w.daemonWindowIDsMu.Unlock()
		return id
	}
	// Local-daemon primary path: if the hub is the app's
	// daemonHub AND we already know a daemonWindowID, reuse it.
	if hub == w.app.daemonHub && w.daemonWindowID != 0 {
		if w.daemonWindowIDs == nil {
			w.daemonWindowIDs = make(map[*daemonsource.Hub]uint32)
		}
		w.daemonWindowIDs[hub] = w.daemonWindowID
		w.daemonWindowIDsMu.Unlock()
		return w.daemonWindowID
	}
	w.daemonWindowIDsMu.Unlock()

	// Otherwise mint a fresh daemon window on this hub. CreateWindow
	// correlates the reply by ReqID (router-demuxed), so a late ack
	// from a previous timed-out create can't be adopted here.
	id, err := hub.CreateWindow(0, 0, int32(w.width), int32(w.height))
	if err != nil {
		return 0
	}
	w.daemonWindowIDsMu.Lock()
	if w.daemonWindowIDs == nil {
		w.daemonWindowIDs = make(map[*daemonsource.Hub]uint32)
	}
	w.daemonWindowIDs[hub] = id
	w.daemonWindowIDsMu.Unlock()
	return id
}

// setLocalDaemonWindowID points this Window at a freshly-minted
// server-side window on the LOCAL/default daemon hub, keeping BOTH the
// legacy daemonWindowID field AND the per-hub daemonWindowIDs map in
// sync. Updating only the field (as the original reseat did) left the
// map holding the dead pre-restart window ID — and windowIDForHub
// PREFERS the map, so later focus/move sent the stale ID to the new
// daemon. It also primes the hub's default window so NewTab lands here.
func (w *Window) setLocalDaemonWindowID(id uint32) {
	w.daemonWindowID = id
	w.daemonWindowIDsMu.Lock()
	if w.daemonWindowIDs == nil {
		w.daemonWindowIDs = make(map[*daemonsource.Hub]uint32)
	}
	if w.app.daemonHub != nil {
		w.daemonWindowIDs[w.app.daemonHub] = id
	}
	w.daemonWindowIDsMu.Unlock()
	if w.app.daemonHub != nil {
		w.app.daemonHub.SetDefaultWindowID(id)
	}
}

// reseatNeedsMint decides whether a reseat episode must (re)mint a fresh
// daemon window. Factored out of frame() (which isn't unit-testable —
// it drives ImGui) so the second-restart-mid-reseat logic can be tested
// directly. Returns true when:
//   - nothing has been minted yet this episode (minted == false), OR
//   - the daemon instance CHANGED since the mint (a second restart while
//     the first reseat was still pending) — the prior window ID lived in
//     the dead intermediate daemon's id-space and is stale, so re-mint.
//
// When currentInstance is "" (pre-v7 / unknown) we can't detect a
// restart, so a minted window is kept as-is — the same safe degradation
// the rest of the instance-scoped logic uses.
func reseatNeedsMint(minted bool, mintedInstance, currentInstance string) bool {
	if !minted {
		return true
	}
	return currentInstance != "" && currentInstance != mintedInstance
}

// sendDaemonMoveTab persists a tab move to the owning daemon.
// Same-window reorder OR cross-window drag — both go through the
// same MsgWindowMoveTab carrying (TabID, ToWindowID, Index).
// No-op when the tab isn't daemon-backed.
//
// The destination window ID is looked up PER-HUB via
// windowIDForHub so a remote tab's reorder always goes to the
// remote daemon with the remote-side window ID — never the local
// daemon's window ID (which would refer to a different window
// or be invalid).
func (w *Window) sendDaemonMoveTab(tab *tabs.Tab, _ /*ignored*/ uint32, idx int32) {
	if tab == nil || tab.Terminal == nil {
		return
	}
	ds, ok := tab.Terminal.(*daemonsource.Source)
	if !ok {
		return
	}
	hub := w.app.findHubForClient(ds.HubClient())
	if hub == nil {
		return
	}
	id := w.windowIDForHub(hub)
	if id == 0 {
		return
	}
	_ = ds.HubClient().SendWindowMoveTab(ds.TabID(), id, idx)
}

// openRemoteTab spawns a NEW tab whose source is a daemon on a
// remote host. hostName is a hub-registry key: a cfg.Hosts bookmark
// name, an ad-hoc destination from the connect dialog, or a bare SSH
// dest — all resolved by remoteHubFor. For "show me what's already
// running over there" use openRemoteReattach instead. Both share
// the per-host Hub so they use one SSH connection.
func (w *Window) openRemoteTab(hostName string) error {
	entry, err := w.app.remoteHubFor(hostName)
	if err != nil {
		return err
	}
	cols, rows := w.gridSize()
	// Bind the new remote tab to THIS GUI window's dedicated
	// window on the remote daemon (lazily minted). Using 0
	// ("daemon default window") meant every GUI window's remote
	// tabs piled into the same remote window, so reorder/focus
	// later targeted the wrong window.
	winID := w.windowIDForHub(entry.hub)
	src, err := entry.hub.NewTabIn(winID, cols, rows, "", nil)
	if err != nil {
		return fmt.Errorf("hub.NewTabIn on %s: %w", hostName, err)
	}
	tab := w.tabs.AdoptTab(src)
	tab.Host = hostName
	w.tabSwitchReq = tab.ID
	return nil
}

// openRemoteWindow spawns a NEW GUI window and opens a fresh remote
// tab for hostName inside it. The window starts empty (no stray local
// PTY tab) and gets its own daemon-side window on the host's hub, so
// its tabs/focus/reorder stay independent of the spawning window's.
func (w *Window) openRemoteWindow(hostName string) error {
	nw := w.app.spawnEmptyWindow()
	if nw == nil {
		return fmt.Errorf("spawn window for %s failed", hostName)
	}
	return nw.openRemoteTab(hostName)
}

// openRemoteReattach drains UNADOPTED daemon windows the remote
// reported at attach time. Each call pulls the first pending
// window into THIS GUI window (adopting all its tabs, honoring
// the daemon's per-window focus); subsequent calls handle further
// pending windows by spawning new GUI windows. The typical UX is
// "open host kh" → see every shell you had open there in the
// same layout (number-of-windows + per-window-tab-grouping
// matches what kh's daemon remembers).
//
// No-op when nothing's pending falls through to openRemoteTab
// so the action isn't confusingly silent.
func (w *Window) openRemoteReattach(hostName string) error {
	entry, err := w.app.remoteHubFor(hostName)
	if err != nil {
		return err
	}
	cols, rows := w.gridSize()

	w.app.remoteHubsMu.Lock()
	pending := entry.reattachQueue
	entry.reattachQueue = nil
	w.app.remoteHubsMu.Unlock()

	if len(pending) == 0 {
		return w.openRemoteTab(hostName)
	}

	// First daemon-window's tabs land in the CURRENT GUI window.
	adoptIntoWindow(w, hostName, entry.hub, pending[0], cols, rows)

	// Extra daemon windows → spawn extra EMPTY GUI windows and
	// adopt into each. spawnEmptyWindow skips the default-tab
	// creation that plain spawnWindow does, so we don't leave a
	// stray local PTY tab in each new window.
	for _, snap := range pending[1:] {
		nw := w.app.spawnEmptyWindow()
		if nw == nil {
			continue
		}
		// Same detach-time geometry restore as the local reattach
		// path: the recorded size includes the tab bar, so the
		// adopted multi-tab window keeps its full grid. Only fresh
		// spawned windows — adoptIntoWindow itself must not resize
		// the user's CURRENT window (the pending[0] case above).
		if snap.Width > 0 && snap.Height > 0 {
			nw.width, nw.height = int(snap.Width), int(snap.Height)
			nw.pendingResize = true
			nw.restoredGeom = true
			nw.restoredWithBar = len(snap.Tabs) > 1
		}
		if snap.PosX != 0 || snap.PosY != 0 {
			nw.initialPosX, nw.initialPosY = float32(snap.PosX), float32(snap.PosY)
			nw.hasInitialPos = true
		}
		nCols, nRows := nw.gridSize()
		if nCols < 2 || nRows < 2 {
			nCols, nRows = cols, rows
		}
		adoptIntoWindow(nw, hostName, entry.hub, snap, nCols, nRows)
	}
	return nil
}

// adoptIntoWindow is shared by openRemoteReattach (and any future
// "pull this remote window into this GUI window" path). Wires the
// tabs from a daemonWindowSnapshot into the GUI window, sets the
// Host badge, restores per-window focus.
func adoptIntoWindow(w *Window, hostName string, hub *daemonsource.Hub, snap daemonWindowSnapshot, cols, rows int) {
	// Seed the per-hub window mapping so future tab creates /
	// focus / reorder for this hub target the SAME remote window
	// we just adopted from — not a freshly-minted one. Without
	// this, windowIDForHub would SendWindowCreate a new remote
	// window on first focus and the user's tabs would split
	// across two remote windows on reattach.
	if snap.WindowID != 0 {
		w.daemonWindowIDsMu.Lock()
		if w.daemonWindowIDs == nil {
			w.daemonWindowIDs = make(map[*daemonsource.Hub]uint32)
		}
		w.daemonWindowIDs[hub] = snap.WindowID
		w.daemonWindowIDsMu.Unlock()
	}
	var focusIdx = -1
	startIdx := len(w.tabs.Tabs)
	for i, ts := range snap.Tabs {
		src := hub.Adopt(ts.ID, int(ts.Cols), int(ts.Rows))
		tab := w.tabs.AdoptTab(src)
		tab.Host = hostName
		if ts.Title != "" {
			tab.SetTitle(ts.Title)
		}
		if cols > 1 && rows > 1 {
			src.Resize(cols, rows)
		}
		if ts.ID == snap.FocusedTabID {
			focusIdx = startIdx + i
		}
	}
	if focusIdx >= 0 && focusIdx < len(w.tabs.Tabs) {
		w.tabs.ActiveIdx = focusIdx
		w.tabSwitchReq = w.tabs.Tabs[focusIdx].ID
	} else if last := w.tabs.Active(); last != nil {
		w.tabSwitchReq = last.ID
	}
}

// dialRemoteDaemon SSH-dials the given resolved host, completes the
// Hello + Attach handshake, starts the read loop, and returns the
// connected client + Attached snapshot. Takes the already-resolved host
// (not a name to re-resolve) so it's safe to call from the Hub's
// reconnect goroutine without racing the main-thread-only a.adhocHosts.
func (a *App) dialRemoteDaemon(name string, host config.RemoteHost) (*clientproto.Client, *protocol.Attached, error) {
	cli, err := clientproto.DialSSH(host.SSHDest, host.RemoteCmd, host.SSHArgs)
	if err != nil {
		return nil, nil, fmt.Errorf("dial ssh %s: %w", host.SSHDest, err)
	}
	if _, err := cli.Hello("xerotty-gui:" + name); err != nil {
		_ = cli.Close()
		return nil, nil, fmt.Errorf("hello to %s: %w", name, err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		_ = cli.Close()
		return nil, nil, fmt.Errorf("attach to %s: %w", name, err)
	}
	select {
	case attached := <-cli.Attached():
		return cli, attached, nil
	case <-time.After(5 * time.Second):
		_ = cli.Close()
		return nil, nil, fmt.Errorf("no Attached response from %s within 5s", name)
	}
}

// remoteHubFor returns the registry entry for the named host,
// lazily building it (SSH-dial + hello + attach) on first request.
// Subsequent calls reuse the same Hub so one SSH connection serves
// many tabs on that host. name is resolved by resolveRemote (a
// cfg.Hosts bookmark, a session ad-hoc target, or a bare SSH dest),
// so a config entry is NOT required. Returns an error if the name is
// blank or the SSH connection fails.
//
// The Attached frame is captured into the entry's reattachQueue so
// openRemoteReattach can later adopt any pre-existing remote tabs.
func (a *App) remoteHubFor(name string) (*remoteHubEntry, error) {
	a.remoteHubsMu.Lock()
	if e, ok := a.remoteHubs[name]; ok {
		// The cached hub self-heals (layer 4b): a dropped SSH path /
		// killed remote daemon is re-dialed by the hub's own reconnect
		// loop, keeping the same Sources. So always hand it back — never
		// tear it down + re-dial, which would orphan the live tabs.
		a.remoteHubsMu.Unlock()
		return e, nil
	}
	a.remoteHubsMu.Unlock()

	host, ok := a.resolveRemote(name)
	if !ok {
		return nil, fmt.Errorf("remote host %q: empty destination", name)
	}

	cli, attached, err := a.dialRemoteDaemon(name, host)
	if err != nil {
		return nil, err
	}
	hub := daemonsource.NewHub(cli)
	// Self-heal over SSH. Capture the resolved host (NOT a re-resolve
	// inside the closure) so the reconnect dialer never touches
	// a.adhocHosts off the main goroutine — SSH params don't change
	// mid-session anyway.
	hub.SetRedial(func() (*clientproto.Client, *protocol.Attached, error) {
		return a.dialRemoteDaemon(name, host)
	})
	hub.SeedRevision(attached.Revision)
	hub.SeedInstance(attached.InstanceID)
	a.wireHubCallbacks(name, hub)

	entry := &remoteHubEntry{hub: hub}
	// Group tabs by their daemon-side window so reattach preserves
	// the user's remote layout. Same shape used for the local
	// daemon's daemonAdoptQueue.
	tabByID := make(map[uint32]daemonTabSnapshot, len(attached.Tabs))
	for _, ti := range attached.Tabs {
		tabByID[ti.ID] = daemonTabSnapshot{
			ID:    ti.ID,
			Title: ti.Title,
			Cols:  ti.Cols,
			Rows:  ti.Rows,
		}
	}
	for _, wi := range attached.Windows {
		snap := daemonWindowSnapshot{
			WindowID:     wi.ID,
			PosX:         wi.PosX,
			PosY:         wi.PosY,
			Width:        wi.Width,
			Height:       wi.Height,
			FocusedTabID: wi.FocusedTabID,
		}
		for _, tid := range wi.TabIDs {
			if t, ok := tabByID[tid]; ok {
				snap.Tabs = append(snap.Tabs, t)
				delete(tabByID, tid)
			}
		}
		if len(snap.Tabs) > 0 {
			entry.reattachQueue = append(entry.reattachQueue, snap)
		}
	}
	if len(tabByID) > 0 {
		// Sweep orphans (tabs not in any window) into a synthetic
		// snapshot so they're not silently dropped.
		var orphans []daemonTabSnapshot
		for _, t := range tabByID {
			orphans = append(orphans, t)
		}
		entry.reattachQueue = append(entry.reattachQueue, daemonWindowSnapshot{Tabs: orphans})
	}

	a.remoteHubsMu.Lock()
	if a.remoteHubs == nil {
		a.remoteHubs = make(map[string]*remoteHubEntry)
	}
	a.remoteHubs[name] = entry
	a.remoteHubsMu.Unlock()
	return entry, nil
}

// resolveRemote maps a hub-registry key to the SSH connection params
// to dial it. Three sources, in priority order:
//
//  1. a [[hosts]] bookmark whose Name matches — the configured path.
//  2. an ad-hoc target the user typed via "Connect to host…" this
//     session (a.adhocHosts).
//  3. fallback: treat the key ITSELF as a bare SSH destination
//     (normalized) with the default remote command. This is what
//     makes `ssh user@host`-style connections work without any
//     config entry — the GUI no longer requires a bookmark.
//
// ok is false only for an empty/blank key, which can't be dialed.
func (a *App) resolveRemote(name string) (config.RemoteHost, bool) {
	for i := range a.cfg.Hosts {
		if a.cfg.Hosts[i].Name == name {
			return a.cfg.Hosts[i], true
		}
	}
	if h, ok := a.adhocHosts[name]; ok {
		return h, true
	}
	dest := normalizeSSHDest(name)
	if dest == "" {
		return config.RemoteHost{}, false
	}
	return config.RemoteHost{Name: name, SSHDest: dest}, true
}

// normalizeSSHDest cleans a user-typed destination into something
// ssh(1) accepts. Plain `ssh user@host:2222` does NOT work — bare
// `host:port` is parsed as a hostname, not a port — but the URI form
// `ssh://[user@]host:port` does. So a trailing numeric `:port` with
// no scheme is rewritten to the ssh:// form; everything else (plain
// "user@host", an ~/.ssh/config alias, an already-ssh:// URI) passes
// through untouched.
func normalizeSSHDest(in string) string {
	s := strings.TrimSpace(in)
	if s == "" || strings.Contains(s, "://") {
		return s
	}
	// Only treat the LAST colon as a port separator, and only when
	// what follows is all digits — leaves IPv6 literals and aliases
	// containing colons alone.
	if i := strings.LastIndex(s, ":"); i > 0 && i < len(s)-1 {
		port := s[i+1:]
		allDigits := true
		for _, r := range port {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return "ssh://" + s
		}
	}
	return s
}

// daemonTabSnapshot is one tab the daemon reported at attach time —
// queued for adoption by the next GUI window that opens.
type daemonTabSnapshot struct {
	ID    uint32
	Title string
	Cols  uint16
	Rows  uint16
}

// daemonWindowSnapshot pairs a daemon-side window ID + geometry hint
// with the tabs that live in it. Drained one-per-GUI-Window during
// the reattach restore path. FocusedTabID is the daemon's record of
// which tab was front; the GUI honors it during adoption so reattach
// puts focus where the user left it, not on whatever happens to be
// last in the slice.
type daemonWindowSnapshot struct {
	WindowID     uint32
	PosX         int32
	PosY         int32
	Width        int32
	Height       int32
	Tabs         []daemonTabSnapshot
	FocusedTabID uint32
}

// dialLocalDaemon ensures the local daemon is reachable (auto-spawning
// it if needed), completes the Hello + Attach handshake, starts the
// client read loop, and returns the connected client + its Attached
// snapshot. Shared by the initial connect and the Hub's reconnect
// dialer (SetRedial), so both paths are identical — a reconnect after a
// daemon restart re-spawns and re-attaches exactly like first launch.
// Attach uses NewIfMissing=false: the GUI creates tabs itself via
// NewTab, it doesn't want the daemon's default-tab-on-empty behavior.
func (a *App) dialLocalDaemon() (*clientproto.Client, *protocol.Attached, error) {
	cli, err := daemonsource.EnsureLocalDaemon(a.cfg.Tabs.DaemonSocket)
	if err != nil {
		return nil, nil, err
	}
	if _, err := cli.Hello("xerotty-gui"); err != nil {
		_ = cli.Close()
		return nil, nil, fmt.Errorf("hello: %w", err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		_ = cli.Close()
		return nil, nil, fmt.Errorf("attach: %w", err)
	}
	select {
	case attached := <-cli.Attached():
		return cli, attached, nil
	case <-time.After(5 * time.Second):
		_ = cli.Close()
		return nil, nil, fmt.Errorf("attach: no response from daemon")
	}
}

// initDaemonSource ensures a local xerotty daemon is running,
// connects to it, attaches, and wires the resulting Hub into App so
// tabs.Manager.NewTab routes through it. Errors here are non-fatal
// — caller logs + falls back to PTY tabs.
func (a *App) initDaemonSource() error {
	cli, attached, err := a.dialLocalDaemon()
	if err != nil {
		return err
	}
	// Group tabs by their daemon-side window so the GUI can spawn
	// one Window per daemon Window on reattach. tabs.Tab info is
	// keyed by ID; window info has TabIDs ordering.
	tabByID := make(map[uint32]daemonTabSnapshot, len(attached.Tabs))
	for _, ti := range attached.Tabs {
		tabByID[ti.ID] = daemonTabSnapshot{
			ID:    ti.ID,
			Title: ti.Title,
			Cols:  ti.Cols,
			Rows:  ti.Rows,
		}
	}
	for _, wi := range attached.Windows {
		snap := daemonWindowSnapshot{
			WindowID:     wi.ID,
			PosX:         wi.PosX,
			PosY:         wi.PosY,
			Width:        wi.Width,
			Height:       wi.Height,
			FocusedTabID: wi.FocusedTabID,
		}
		for _, tid := range wi.TabIDs {
			if t, ok := tabByID[tid]; ok {
				snap.Tabs = append(snap.Tabs, t)
				delete(tabByID, tid)
			}
		}
		if len(snap.Tabs) > 0 {
			a.daemonAdoptQueue = append(a.daemonAdoptQueue, snap)
		}
	}
	// Sweep any orphan tabs (in tabByID but not in any window's
	// TabIDs) into a synthetic window snapshot so they aren't lost.
	if len(tabByID) > 0 {
		var orphans []daemonTabSnapshot
		for _, t := range tabByID {
			orphans = append(orphans, t)
		}
		a.daemonAdoptQueue = append(a.daemonAdoptQueue, daemonWindowSnapshot{
			Tabs: orphans,
		})
	}
	hub := daemonsource.NewHub(cli)
	// Self-heal: a dropped/restarted local daemon is re-dialed by the
	// hub itself (auto-spawning via EnsureLocalDaemon if the process
	// died), keeping the same Source objects so the frozen tabs come
	// back to life in place. dialLocalDaemon is idempotent + safe to
	// call from the hub's router goroutine (no shared mutable state).
	hub.SetRedial(a.dialLocalDaemon)
	// Seed the topology-revision gate from the attach snapshot so
	// later MsgTopologyChanged broadcasts are applied only when newer.
	hub.SeedRevision(attached.Revision)
	// Seed the daemon identity so the first reconnect can distinguish a
	// same-daemon resync from a restarted-daemon one (tombstone scoping).
	hub.SeedInstance(attached.InstanceID)
	a.configureHubScrollback(hub)
	a.wireHubCallbacks("local", hub)
	a.setDaemonHub(hub, "local")
	// NOTE: tabSourceFactory is set per-Window by
	// installSourceFactory() so multi-window setups don't all
	// share Hub.defaultWindowID. App-level default is nil;
	// installSourceFactory falls back to terminal.New if no
	// daemon-window association exists yet.
	return nil
}

// --- guimcp.Backend implementation ---
//
// The GUI runs an aggregating MCP server (internal/guimcp) so an
// agent gets ONE socket covering every daemon the GUI talks to.
// These two methods are how that server enumerates + resolves
// tabs across the local hub + every remote hub.

// getDaemonHub returns the default hub + its host name under the
// lock. Used by the guimcp request goroutines (which run
// concurrently with activeDaemonHub's re-dial writes).
func (a *App) getDaemonHub() (*daemonsource.Hub, string) {
	a.daemonMu.Lock()
	defer a.daemonMu.Unlock()
	return a.daemonHub, a.daemonHubName
}

// setDaemonHub publishes the default hub + name under the lock.
func (a *App) setDaemonHub(hub *daemonsource.Hub, name string) {
	a.daemonMu.Lock()
	a.daemonHub = hub
	a.daemonHubName = name
	a.daemonMu.Unlock()
}

// hubsByName returns the daemon hubs the GUI is connected to,
// keyed by host namespace. The default hub is keyed by its OWN
// name (daemonHubName) — "local" normally, but the host name in
// daemon:<name> mode. That dedupes against the remoteHubs entry
// for the same hub: in daemon:kh mode the kh hub is BOTH the
// default and remoteHubs["kh"], and keying both by "kh" collapses
// them into one map entry instead of listing the same tabs as
// "local:" and "kh:".
func (a *App) hubsByName() map[string]*daemonsource.Hub {
	out := map[string]*daemonsource.Hub{}
	hub, name := a.getDaemonHub()
	if hub != nil {
		if name == "" {
			name = "local"
		}
		out[name] = hub
	}
	a.remoteHubsMu.Lock()
	for rname, e := range a.remoteHubs {
		out[rname] = e.hub
	}
	a.remoteHubsMu.Unlock()
	return out
}

// ListTabs implements guimcp.Backend: every tab across every hub
// with namespaced IDs + triage metadata (cwd, foreground proc,
// closed state, focused flag).
func (a *App) ListTabs() []guimcp.TabRef {
	focused := a.focusedDaemonTabIDs() // set of source pointers that are focused
	var refs []guimcp.TabRef
	for name, hub := range a.hubsByName() {
		for _, src := range hub.Sources() {
			refs = append(refs, guimcp.TabRef{
				NSID:       guimcp.MakeNSID(name, src.TabID()),
				Host:       name,
				Title:      src.Title(),
				Cols:       src.Width(),
				Rows:       src.Height(),
				CWD:        src.GetCWD(),
				Foreground: src.ForegroundProcessName(),
				Closed:     src.IsClosed(),
				ExitCode:   src.ChildExitCode(),
				Focused:    focused[src],
				LastOutput: src.LastOutput(),
				LastInput:  src.LastInput(),
			})
		}
	}
	return refs
}

// focusedDaemonTabIDs returns the set of daemon Sources that are the
// active tab of some GUI window — so list_tabs can flag which tabs the
// user is actually looking at. Called on the guimcp goroutine; it
// reads the snapshot published by publishFocusedSources on the main
// thread rather than walking a.windows / tabs.Manager directly (those
// are mutated unsynchronized by the UI thread). A momentarily stale
// answer is fine — this is advisory metadata.
func (a *App) focusedDaemonTabIDs() map[*daemonsource.Source]bool {
	if m := a.focusedSources.Load(); m != nil {
		return *m
	}
	return map[*daemonsource.Source]bool{}
}

// publishFocusedSources recomputes the focused-Source set and stores
// it for the guimcp goroutine to read. MUST be called on the main
// thread (it walks a.windows and each window's tabs.Manager). The
// stored map is treated as immutable after Store — readers never
// mutate it — so the atomic swap is a safe hand-off.
func (a *App) publishFocusedSources() {
	out := map[*daemonsource.Source]bool{}
	for _, w := range a.windows {
		if w.tabs == nil {
			continue
		}
		if t := w.tabs.Active(); t != nil && t.Terminal != nil {
			if ds, ok := t.Terminal.(*daemonsource.Source); ok {
				out[ds] = true
			}
		}
	}
	a.focusedSources.Store(&out)
}

// SourceFor implements guimcp.Backend: resolve "<host>:<tabid>"
// to the Source on that host's hub.
func (a *App) SourceFor(nsID string) (*daemonsource.Source, bool) {
	// Split on the LAST colon, not the first: an ad-hoc host key can
	// itself contain a colon (e.g. "user@host:2222"), and the tab ID
	// is always the trailing numeric segment. strings.Cut (first
	// colon) would mis-parse "user@host:2222:5" as host="user@host".
	i := strings.LastIndex(nsID, ":")
	if i < 0 {
		return nil, false
	}
	host, idStr := nsID[:i], nsID[i+1:]
	id64, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		return nil, false
	}
	hub, ok := a.hubsByName()[host]
	if !ok {
		return nil, false
	}
	for _, src := range hub.Sources() {
		if src.TabID() == uint32(id64) {
			return src, true
		}
	}
	return nil, false
}

// CreateTab implements guimcp.Backend: open (or find, when name is
// non-empty) a tab on the named host's daemon. Runs on the guimcp
// goroutine — safe because it only touches the hub (thread-safe wire
// client), never the GUI. The GUI tab materializes via the daemon's
// MsgTopologyChanged broadcast → applyPendingTopology on the main
// thread, the same route as a tab created by any other client.
func (a *App) CreateTab(host, name string, cols, rows int) (string, bool, error) {
	hubs := a.hubsByName()
	if host == "" {
		if _, n := a.getDaemonHub(); n != "" {
			host = n
		} else {
			host = "local"
		}
	}
	hub, ok := hubs[host]
	if !ok || hub == nil {
		return "", false, fmt.Errorf("no daemon hub for host %q", host)
	}
	src, reused, err := hub.NewNamedTab(name, cols, rows, "")
	if err != nil {
		return "", false, err
	}
	return guimcp.MakeNSID(host, src.TabID()), reused, nil
}

// CloseTab implements guimcp.Backend: daemon-side close of a
// namespaced tab. The GUI tab reaps through the normal vanish path
// (markVanished -> CheckClosed) once the daemon broadcasts the close.
func (a *App) CloseTab(nsID string) error {
	src, ok := a.SourceFor(nsID)
	if !ok {
		return fmt.Errorf("tab not found: %s", nsID)
	}
	src.Close()
	return nil
}

// guiProposal tags a daemon-reported proposal with the hub +
// host it came from so the GUI gate resolves against the correct
// daemon. Without the tag, a remote (kh) proposal's Approve would
// have gone to the local/default hub — wrong daemon, wrong tab.
type guiProposal struct {
	hub  *daemonsource.Hub
	host string
	info protocol.ProposalInfo
}

// wireHubCallbacks installs the hub-level (session-global)
// callbacks: OSC 52 clipboard writes → local OS clipboard, and
// propose-mode queue updates → the approval banner. Called for
// every hub (local + remote); name is the host namespace
// ("local" or a cfg.Hosts name) used for display + so each hub's
// proposal list replaces only its own entries.
func (a *App) wireHubCallbacks(name string, hub *daemonsource.Hub) {
	hub.SetClipboardSetCallback(func(text string) {
		_ = input.ClipboardWrite(text)
	})
	hub.SetProposalsCallback(func(infos []protocol.ProposalInfo) {
		a.proposalsMu.Lock()
		// Drop this HOST's previous entries (by namespace, not hub
		// pointer), keep other hosts', then append the fresh list.
		// Filtering by host (not pointer) means a reconnected host
		// — which gets a brand-new *Hub — still clears its stale
		// rows: the new hub's callback fires under the same name,
		// so an empty list correctly empties the host's rows
		// instead of leaving orphans tagged with the dead pointer.
		kept := a.pendingProposals[:0:0]
		for _, gp := range a.pendingProposals {
			if gp.host != name {
				kept = append(kept, gp)
			}
		}
		for _, info := range infos {
			kept = append(kept, guiProposal{hub: hub, host: name, info: info})
		}
		a.pendingProposals = kept
		a.proposalsMu.Unlock()
		// Wake the (event-driven) render loop so the approval banner
		// appears/updates without waiting out the idle timeout.
		platform.PostWake()
	})
	// Topology snapshots (a tab/window created/closed/moved by another
	// client or an MCP agent) → reconcile the GUI tab bar. Stash the
	// latest and wake the loop; the actual GUI mutation runs on the
	// main thread in applyPendingTopology.
	hub.SetTopologyCallback(func(s *protocol.TopologyChanged) {
		a.topoMu.Lock()
		if a.pendingTopo == nil {
			a.pendingTopo = make(map[*daemonsource.Hub]pendingTopoSnap)
		}
		a.pendingTopo[hub] = pendingTopoSnap{host: name, snap: s}
		a.topoMu.Unlock()
		platform.PostWake()
	})
	a.ensureGUIMCP()
}

// pendingTopoSnap is a queued topology snapshot + the hub's display
// host, awaiting main-thread reconcile.
type pendingTopoSnap struct {
	host string
	snap *protocol.TopologyChanged
}

// applyPendingTopology drains queued topology snapshots and reconciles
// the GUI tab bar against each. MUST run on the main thread (it mutates
// windows / tabs.Manager). Called once per frame.
//
// Scope: it ADDS GUI tabs for daemon tabs that newly appeared (created
// by another client or an MCP agent), and RELOCATES a visible tab when
// a snapshot shows it in a different daemon window than the GUI has it
// (so later local focus/reorder targets the right daemon window).
// Removal of vanished tabs is handled elsewhere — applyTopology marks
// the vanished Source closed (markVanished) and the per-frame
// CheckClosed + reap drops it (forced even under on_child_exit=hold).
func (a *App) applyPendingTopology() {
	// Don't reconcile mid-drag: a cross-window drag is actively moving
	// a tab between managers, and relocating underneath it would
	// corrupt the drag. Leave the (latest-wins) snapshot queued and
	// process it on a later frame once the drag completes.
	if a.dragTab != nil {
		return
	}
	a.topoMu.Lock()
	if len(a.pendingTopo) == 0 {
		a.topoMu.Unlock()
		return
	}
	pend := a.pendingTopo
	a.pendingTopo = make(map[*daemonsource.Hub]pendingTopoSnap)
	a.topoMu.Unlock()
	for hub, ps := range pend {
		a.reconcileDaemonTabs(hub, ps.host, ps.snap)
	}
}

// reconcileDaemonTabs adds GUI tabs for newly-appeared daemon tabs and
// relocates visible tabs that moved to a different daemon window —
// preserving each window's active tab (snapshot FocusedTabID is
// attach-time metadata, never used to hijack a live client's focus).
func (a *App) reconcileDaemonTabs(hub *daemonsource.Hub, host string, snap *protocol.TopologyChanged) {
	cli := hub.Client()
	// Which daemon tab IDs already have a GUI tab on this hub?
	present := make(map[uint32]bool)
	for _, w := range a.windows {
		if w.tabs == nil {
			continue
		}
		for _, t := range w.tabs.Tabs {
			if ds, ok := t.Terminal.(*daemonsource.Source); ok && ds.HubClient() == cli {
				present[ds.TabID()] = true
			}
		}
	}
	// daemon windowID for each tab in the snapshot, for placement.
	winOfTab := make(map[uint32]uint32, len(snap.Tabs))
	for _, wi := range snap.Windows {
		for _, id := range wi.TabIDs {
			winOfTab[id] = wi.ID
		}
	}
	for _, ti := range snap.Tabs {
		if present[ti.ID] {
			// Already shown — but maybe in the wrong GUI window if a
			// remote actor moved it. Relocate if so.
			a.relocateDaemonTab(hub, ti.ID, winOfTab[ti.ID])
			continue
		}
		target := a.targetWindowForDaemonTab(hub, winOfTab[ti.ID])
		if target == nil {
			// No GUI window represents this hub yet — a window that
			// adopts the hub will pick the tab up. Skip for now.
			continue
		}
		src := hub.Adopt(ti.ID, int(ti.Cols), int(ti.Rows))
		// Resize the freshly-adopted source to the GUI window it's
		// joining. The daemon minted the tab at its own dimensions
		// (ti.Cols×ti.Rows — e.g. an MCP agent's NewTab default), which
		// rarely match this window's grid. Without this the tab's
		// contents stay at the daemon size until the next manual window
		// resize triggers resizeTerminals() and snaps them straight.
		// Every other adopt path (spawn, first-frame attach, cross-window
		// drag) already does this; this remote-created-tab path was the
		// one that didn't.
		if cols, rows := target.gridSize(); cols > 1 && rows > 1 {
			src.Resize(cols, rows)
		}
		// Preserve the target window's active tab across the add.
		prevActiveID := -1
		if at := target.tabs.Active(); at != nil {
			prevActiveID = at.ID
		}
		tab := target.tabs.AdoptTab(src) // AdoptTab focuses the new tab…
		if host != "" && host != "local" {
			tab.Host = host
		}
		if ti.Title != "" {
			tab.SetTitle(ti.Title)
		}
		// …so restore the prior active tab — don't steal focus from a
		// live client just because another client made a tab.
		if prevActiveID >= 0 {
			for i, t := range target.tabs.Tabs {
				if t.ID == prevActiveID {
					target.tabs.ActiveIdx = i
					break
				}
			}
		}
		present[ti.ID] = true
	}
}

// targetWindowForDaemonTab picks the GUI window a newly-appeared
// daemon tab should join: the window mapped to the tab's daemon-side
// window, else any window already showing this hub's tabs, else the
// active window. Returns nil if no window represents the hub yet.
func (a *App) targetWindowForDaemonTab(hub *daemonsource.Hub, daemonWinID uint32) *Window {
	var hubWindow *Window
	for _, w := range a.windows {
		if w.tabs == nil {
			continue
		}
		if daemonWinID != 0 && w.daemonWindowForHub(hub) == daemonWinID {
			return w
		}
		if hubWindow == nil {
			for _, t := range w.tabs.Tabs {
				if ds, ok := t.Terminal.(*daemonsource.Source); ok && ds.HubClient() == hub.Client() {
					hubWindow = w
					break
				}
			}
		}
	}
	if hubWindow != nil {
		return hubWindow
	}
	return a.active
}

// relocateDaemonTab moves an already-visible daemon tab to the GUI
// window mapped to its current daemon-side window, when a snapshot
// shows it moved (by another client / MCP agent). Without this the tab
// stays in its old GUI window and later local focus/reorder sends the
// stale daemon window ID. No-op when the placement is already correct,
// the daemon window has no GUI window, or the tab isn't found.
// Preserves both windows' active tabs (a remote move must not steal a
// live client's focus).
func (a *App) relocateDaemonTab(hub *daemonsource.Hub, tabID, daemonWinID uint32) {
	if daemonWinID == 0 {
		return
	}
	cli := hub.Client()
	// A locally-initiated move may still be in flight (pending drain)
	// — the snapshot predates it. Relocating now would yank the tab
	// out from under the user's drag; the daemon will catch up when
	// the move lands and the next snapshot agrees with the GUI.
	for _, w := range a.windows {
		if ds, ok := w.pendingDaemonMove.(*daemonsource.Source); ok &&
			ds.HubClient() == cli && ds.TabID() == tabID {
			return
		}
	}
	// Locate the tab's current GUI window + index.
	var srcWin *Window
	srcIdx := -1
	for _, w := range a.windows {
		if w.tabs == nil {
			continue
		}
		for i, t := range w.tabs.Tabs {
			if ds, ok := t.Terminal.(*daemonsource.Source); ok && ds.HubClient() == cli && ds.TabID() == tabID {
				srcWin, srcIdx = w, i
				break
			}
		}
		if srcWin != nil {
			break
		}
	}
	if srcWin == nil {
		return
	}
	if srcWin.daemonWindowForHub(hub) == daemonWinID {
		return // already in the right GUI window
	}
	// Destination GUI window must already map to this daemon window;
	// if the GUI doesn't represent it, leave the tab put.
	var dstWin *Window
	for _, w := range a.windows {
		if w.tabs != nil && w != srcWin && w.daemonWindowForHub(hub) == daemonWinID {
			dstWin = w
			break
		}
	}
	if dstWin == nil {
		return
	}
	moved := srcWin.tabs.Tabs[srcIdx]
	host, title := moved.Host, moved.Title()
	dstPrevActive := -1
	if at := dstWin.tabs.Active(); at != nil {
		dstPrevActive = at.ID
	}
	// RemoveTab preserves srcWin's active tab; AdoptTab focuses the
	// moved tab in dstWin, so restore dstWin's prior active after.
	srcWin.tabs.RemoveTab(srcIdx)
	newTab := dstWin.tabs.AdoptTab(moved.Terminal)
	newTab.Host = host
	if title != "" {
		newTab.SetTitle(title)
	}
	if dstPrevActive >= 0 {
		for i, t := range dstWin.tabs.Tabs {
			if t.ID == dstPrevActive {
				dstWin.tabs.ActiveIdx = i
				break
			}
		}
	}
	// If relocating emptied the source window, schedule it for reap.
	if srcWin.tabs.Count() == 0 {
		srcWin.pendingClose = true
	}
}

// ensureGUIMCP starts the GUI's aggregating MCP server once, the
// first time any daemon hub connects. Socket lives alongside the
// daemon sockets with a .gui.mcp.sock suffix. Best-effort: a
// failure (e.g. socket in use by another xerotty) just logs.
func (a *App) ensureGUIMCP() {
	a.daemonMu.Lock()
	if a.guiMCP != nil {
		a.daemonMu.Unlock()
		return
	}
	sock := guiMCPSocketPath()
	srv := guimcp.New(a, sock)
	a.guiMCP = srv
	a.daemonMu.Unlock()
	go func() {
		fmt.Fprintf(os.Stderr, "xerotty: aggregating MCP on %s\n", sock)
		if err := srv.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "xerotty: guimcp: %v\n", err)
		}
	}()
}

// guiMCPSocketPath picks the aggregating MCP socket path. Delegates
// to sockpath so `xerotty mcp` (the stdio bridge, headless-safe)
// derives the exact same path — bind side and dial side can't drift.
func guiMCPSocketPath() string {
	return sockpath.GUIMCPSocket()
}

// installSourceFactory builds the tabs.Manager.SourceFactory for a
// Window. In daemon mode the closure does NOT capture a hub pointer —
// it calls a.activeDaemonHub() per invocation. The hub itself
// self-heals across connection drops (layer 4b), so the pointer is
// stable; the per-invocation lookup just keeps the factory honest if
// the default hub is ever swapped wholesale (e.g. mode reconfig).
//
// Falls back to in-process PTY when daemon mode isn't active OR
// activeDaemonHub returns nil.
func (a *App) installSourceFactory(w *Window) {
	// OSC 52 clipboard hooks — injected so tabs.Manager can wire
	// them onto each source without the tabs package importing the
	// SDL-backed clipboard. Set on every manager regardless of
	// source mode (PTY tabs handle OSC 52 locally; daemon tabs
	// route through their hub).
	w.tabs.ClipboardSetFn = func(text string) {
		_ = input.ClipboardWrite(text)
	}
	w.tabs.ClipboardGetFn = func() string {
		s, _ := input.ClipboardRead()
		return s
	}
	if a.daemonHub == nil {
		w.tabs.SourceFactory = nil // tabs.NewTab uses terminal.New
		return
	}
	w.tabs.SourceFactory = func(cols, rows int, cwd string, launch *terminal.LaunchCmd) (terminal.Source, error) {
		hub := a.activeDaemonHub()
		if hub == nil {
			return nil, fmt.Errorf("daemon hub unavailable")
		}
		// Snapshot the WindowID at call time, not closure-create
		// time, so re-installs after daemonWindowID changes pick
		// up the new value. 0 → daemon's default window.
		return hub.NewTabIn(w.daemonWindowID, cols, rows, cwd, launch)
	}
}

// activeDaemonHub returns the default daemon hub, or nil in PTY mode /
// when startup init never produced one. The hub self-heals across
// connection drops (layer 4b): its reconnect loop re-dials in place
// keeping the same Sources, so callers no longer re-init on a dead
// client. During the brief dial gap the hub's current client is the
// closed one, so a NewTabIn through it returns a transient error rather
// than hanging — acceptable; the user retries.
func (a *App) activeDaemonHub() *daemonsource.Hub {
	hub, _ := a.getDaemonHub()
	return hub
}

// reapClosedWindows removes Windows the user closed this frame. All
// Windows are equal — no "main" — so the cleanup is uniform. The
// loop in wrappedFrame quits the process if a.windows becomes empty
// after the reap (last visible Window gone = app exit).
func (a *App) reapClosedWindows() {
	survivors := a.windows[:0]
	for _, w := range a.windows {
		if !w.pendingClose {
			survivors = append(survivors, w)
			continue
		}
		// Tear down the Window's terminals and renderer. Use
		// Detach (not Close) so daemon-backed tabs survive — the
		// user closing a GUI window means "stop showing me", not
		// "kill the shell". For in-process PTY tabs Detach is
		// equivalent to Close anyway (PTY can't outlive the
		// process).
		if w.tabs != nil {
			for _, tab := range w.tabs.Tabs {
				tab.Terminal.Detach()
			}
		}
		if w.renderer != nil && w.renderer.Glyphs != nil {
			w.renderer.Glyphs.Close()
			w.renderer.Glyphs = nil
			w.renderer.InvalidateCellCache()
		}
	}
	a.windows = survivors
	// If the active Window was reaped, point at any surviving one.
	if a.active != nil {
		found := false
		for _, w := range a.windows {
			if w == a.active {
				found = true
				break
			}
		}
		if !found {
			a.active = a.bestFocusReplacement()
		}
	}
}

func (a *App) bestFocusReplacement() *Window {
	var fallback *Window
	for i := len(a.windows) - 1; i >= 0; i-- {
		if !a.windows[i].pendingClose {
			fallback = a.windows[i]
			break
		}
	}

	bestRank := int(^uint(0) >> 1)
	var best *Window
	for _, win := range a.windows {
		if win.pendingClose || win.imViewport == nil {
			continue
		}
		handle := win.imViewport.PlatformHandle()
		if handle == 0 {
			continue
		}
		rank := platform.CocoaWindowZRank(handle)
		if rank >= 0 && rank < bestRank {
			bestRank = rank
			best = win
		}
	}
	if best != nil {
		return best
	}
	return fallback
}

// mouseOverOwnedContent returns true iff the OS-level cursor is
// currently inside the content rect of any visible Window in this
// App. Used by the macOS mouse-mirror to gate synthetic DOWN
// injection: we only inject if the user clicked on something this
// app owns, otherwise a click on the desktop or another app would
// generate a phantom terminal selection here.
//
// Primary signal: ImGui's MouseHoveredViewport, which is the
// platform-reported viewport-under-cursor (set each frame by the
// SDL backend). If ImGui has a hovered viewport and it's one of
// ours, return true.
//
// Fallback: iterate our Window list and bounds-check the OS-level
// cursor against each viewport's Pos/Size. We never used to need
// the fallback before realizing MouseHoveredViewport can briefly
// be 0 right after a focus shift (the very moment the mirror is
// trying to compensate for).
// mouseOverOwnedContentPos: like mouseOverOwnedContent, but also
// returns the mouse pos we determined to be "inside one of our
// viewports" — used by the mirror to feed ImGui a valid AddMousePosEvent
// when ImGui's own MousePos is sentinel (-FLT_MAX) during a focus
// shift. Without that, a synthetic DOWN lands at -FLT_MAX, the hit
// test misses every widget, and the click does nothing.
func (a *App) mouseOverOwnedContentPos() (bool, imgui.Vec2) {
	ok := a.mouseOverOwnedContent()
	if !ok {
		return false, imgui.Vec2{}
	}
	// Recompute the in-viewport position so callers can feed it to
	// ImGui. Prefer ImGui's tracked MousePos when valid; fall back
	// to SDL's OS-level pos (raw or scaled — whichever fits a
	// viewport).
	mp := imgui.MousePos()
	const noMouseSentinel = -3.4e+37
	if mp.X > noMouseSentinel {
		return true, mp
	}
	gx, gy := sdlhack.GlobalMousePos()
	rawPos := imgui.Vec2{X: float32(gx), Y: float32(gy)}
	fbScale := imgui.CurrentIO().DisplayFramebufferScale().X
	if fbScale < 1 {
		fbScale = 1
	}
	scaledPos := imgui.Vec2{X: rawPos.X / fbScale, Y: rawPos.Y / fbScale}
	for _, w := range a.windows {
		if w.imViewport == nil {
			continue
		}
		pos := w.imViewport.Pos()
		size := w.imViewport.Size()
		if rawPos.X >= pos.X && rawPos.X < pos.X+size.X &&
			rawPos.Y >= pos.Y && rawPos.Y < pos.Y+size.Y {
			return true, rawPos
		}
		if scaledPos.X >= pos.X && scaledPos.X < pos.X+size.X &&
			scaledPos.Y >= pos.Y && scaledPos.Y < pos.Y+size.Y {
			return true, scaledPos
		}
	}
	return true, rawPos // shouldn't reach (mouseOverOwnedContent said ok), but be safe
}

func (a *App) mouseOverOwnedContent() bool {
	// Primary: ImGui's MouseHoveredViewport (platform-reported
	// viewport the cursor is over).
	hovered := imgui.CurrentIO().MouseHoveredViewport()
	if hovered != 0 {
		for _, w := range a.windows {
			if w.imViewport != nil && w.imViewport.ID() == hovered {
				return true
			}
		}
	}
	// Bounds-check the mouse pos against each viewport rect. Use
	// ImGui's MousePos if it has a real value, otherwise fall back
	// to SDL_GetGlobalMouseState. ImGui sets MousePos to -FLT_MAX
	// when the app has no mouse focus (e.g. mid-Cocoa-focus-shift),
	// which is *exactly* when the mirror needs to compensate.
	mp := imgui.MousePos()
	const noMouseSentinel = -3.4e+37 // approximates -FLT_MAX comparison
	if mp.X <= noMouseSentinel {
		gx, gy := sdlhack.GlobalMousePos()
		mp = imgui.Vec2{X: float32(gx), Y: float32(gy)}
	}
	// SDL on macOS sometimes returns physical pixels for
	// SDL_GetGlobalMouseState even though viewport.Pos/Size are in
	// logical pixels — observed in practice as (~2× the expected
	// values) on Retina. Compute a scale-corrected pos too so the
	// bounds check accepts either unit if it lands inside a viewport.
	fbScale := imgui.CurrentIO().DisplayFramebufferScale().X
	if fbScale < 1 {
		fbScale = 1
	}
	mpScaled := imgui.Vec2{X: mp.X / fbScale, Y: mp.Y / fbScale}
	for _, w := range a.windows {
		if w.imViewport == nil {
			continue
		}
		pos := w.imViewport.Pos()
		size := w.imViewport.Size()
		inRaw := mp.X >= pos.X && mp.X < pos.X+size.X &&
			mp.Y >= pos.Y && mp.Y < pos.Y+size.Y
		inScaled := mpScaled.X >= pos.X && mpScaled.X < pos.X+size.X &&
			mpScaled.Y >= pos.Y && mpScaled.Y < pos.Y+size.Y
		if inRaw || inScaled {
			return true
		}
	}
	return false
}

// startMouseMirrorWakePoller keeps the event-driven run loop responsive to
// macOS mouse-button edges even when SDL drops the underlying Cocoa event.
// It only pushes a wake event; the actual ImGui injection still happens on
// the main thread from Window.frame().
func (a *App) startMouseMirrorWakePoller() func() {
	if runtime.GOOS != "darwin" || disableMirror {
		return func() {}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)

		// Adaptive cadence: 30ms only briefly after a button edge
		// (when a second edge could plausibly follow); 150ms when the
		// button state has been stable. A permanent 30ms ticker kept
		// Go's timer machinery hot forever for a poller whose entire
		// job is catching the RARE click SDL drops — the mirror's own
		// defer window is ~15 frames, so 150ms detection latency for
		// the first edge changes nothing user-visible.
		prev := sdlhack.LeftButtonPhysicalDown()
		fast := 0
		for {
			d := 150 * time.Millisecond
			if fast > 0 {
				d = 30 * time.Millisecond
				fast--
			}
			select {
			case <-time.After(d):
				down := sdlhack.LeftButtonPhysicalDown()
				if down != prev {
					prev = down
					fast = 40 // ~1.2s of fast polling after an edge
					platform.PostWake()
				}
			case <-stop:
				return
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

// spawnWindowAdopting spawns a new Window like spawnWindow, except
// its first tab adopts the given already-running Terminal instead
// of starting a fresh shell. Used by cross-Window tab drag when the
// user drops the floating tab outside any existing Window.
func (a *App) spawnWindowAdopting(d *tabDrag) {
	a.spawnWindowImpl(d.Term)
	// The freshly spawned window's adopted tab is the active one.
	// Same identity transfer as adoptDraggedTab (see there for why).
	if len(a.windows) > 0 {
		w := a.windows[len(a.windows)-1]
		if tab := w.tabs.Active(); tab != nil && tab.Terminal == d.Term {
			tab.Host = d.Host
			if d.Title != "" {
				tab.SetTitle(d.Title)
			}
		}
	}
}

// spawnWindow adds a new Window to this App in the same process. The
// secondary Window shares the cimgui-go SDL backend + ImGui context
// + GL textures with the main Window, but has its own tabs manager,
// renderer, glyph cache, and per-window UI state. The render loop in
// Run() then wraps its frame body in an ImGui top-level window that
// multi-viewport auto-promotes to its own OS window.
func (a *App) spawnWindow() {
	a.spawnWindowImpl(nil)
}

// SetPendingLaunch makes the initial window's first tab run the given
// command instead of the shell — the cold-start side of the `-e`/`-x`
// feature, used when no already-running instance was available to
// forward to. Empty argv is a no-op (normal shell). shell runs argv
// joined via `$SHELL -c` (`-x`); otherwise argv is exec'd directly
// (`-e`). Call before Run.
func (a *App) SetPendingLaunch(argv []string, shell bool) {
	if len(argv) == 0 {
		return
	}
	a.spawnCmd = &terminal.LaunchCmd{Argv: argv, Shell: shell}
}

// spawnEmptyWindow creates a new GUI window with NO starting tab.
// Used by remote reattach which adopts tabs into the window itself
// — the normal spawn path would create a local/default PTY tab
// first, leaving an unwanted stray tab. Returns the new Window
// (or nil if spawn failed) so the caller can adopt into it.
func (a *App) spawnEmptyWindow() *Window {
	a.suppressInitialTab = true
	a.spawnWindowImpl(nil)
	a.suppressInitialTab = false
	if len(a.windows) == 0 {
		return nil
	}
	return a.windows[len(a.windows)-1]
}

func (a *App) spawnWindowImpl(adopt terminal.Source) {
	if len(a.windows) == 0 {
		return // can't spawn before Run() has set up the main Window
	}
	main := a.windows[0]
	if main.renderer == nil {
		return // main Window isn't fully initialized yet
	}

	w := newWindow(a)
	// Stable ImGui ID — display title is computed per-frame via the
	// "###" separator so it can reflect the active tab's title without
	// invalidating the window's identity. Use a monotonic counter, NOT
	// len(a.windows): a closed window leaves its ImGui-side size cached,
	// so a reused name would make this new window open at that stale
	// size. See the windowSeq doc on App.
	a.windowSeq++
	w.imguiName = fmt.Sprintf("xerottywin%d", a.windowSeq)
	// Force the configured geometry onto this Window's first rendered
	// frame (CondAlways) rather than CondFirstUseEver. The monotonic
	// name above already prevents ID reuse, but this guarantees the
	// spawn lands at the computed width/height even if any ImGui-side
	// size state for this ID somehow survives — the size is one-shot
	// (pendingResize clears after the first Begin), so user resizing
	// afterward still works.
	w.pendingResize = true
	// Inherit the spawning Window's font size + cell metrics — Cmd+N
	// from a zoomed window opens a new window at the same zoom, just
	// like iTerm2. From then on the two diverge if user Cmd+= on
	// either independently.
	parent := a.active
	if parent == nil {
		parent = main
	}
	w.fontSize = parent.fontSize
	w.cellW = parent.cellW
	w.cellH = parent.cellH
	// Cascade the new Window's initial position so it doesn't open
	// exactly on top of its parent. Use the parent's current OS-window
	// position plus a small offset — same pattern macOS/iTerm2 use for
	// new-window placement.
	// Cascade by ~30% of the parent's size on each axis. xfce4-terminal
	// itself does no explicit cascade (it leaves placement to the WM —
	// gtk_window_move is only called for parsed --geometry strings,
	// confirmed in terminal-app.c upstream), so there's no "match xfce4"
	// number to copy. Fixed-pixel offsets (the old 30 / 60) look fine
	// on small windows and lost on large ones; scaling by parent size
	// keeps the offset visually consistent across zoom / configured cols
	// × rows. 30% is enough that the two title bars don't overlap and
	// the new window's content area starts well clear of the parent's,
	// without going so far that the new window lands off-screen for
	// users near a display edge.
	if parent.imViewport != nil {
		pp := parent.imViewport.Pos()
		ps := parent.imViewport.Size()
		w.initialPosX = pp.X + ps.X*0.30
		w.initialPosY = pp.Y + ps.Y*0.30
		w.hasInitialPos = true
	}
	// Geometry: configured Window.Columns × Window.Rows, NOT the
	// parent's width/height. New Windows always start with a single
	// tab so there's no tab bar; copying the parent's dimensions
	// (which on a multi-tab parent include tabBarH worth of vertical
	// space the new Window won't use) would manifest as the new
	// Window opening one row taller than configured. tabBarH starts
	// at 0 and is recomputed each frame in frame() based on tab
	// count.
	w.tabBarH = 0
	cfgCols, cfgRows := a.cfg.Window.Columns, a.cfg.Window.Rows
	if cfgCols < 2 {
		cfgCols = 80
	}
	if cfgRows < 2 {
		cfgRows = 24
	}
	padX2 := float32(a.cfg.Appearance.Padding) * 2
	w.width = int(math.Ceil(float64(float32(cfgCols)*w.cellW + padX2 + cellSafetyMarginH + cellOriginInsetX)))
	w.height = int(math.Ceil(float64(float32(cfgRows)*w.cellH + padX2 + cellSafetyMarginV)))
	// (Old: w.backend = main.backend — no per-Window backend now.
	// Every Window shares the process-wide platform layer and uses
	// ImGui multi-viewport for its OS window.)
	_ = main

	// Per-window renderer — shares the same font handles but gets its
	// own Metrics / scrollbar / glyph cache so per-Window theming etc.
	// stays possible later.
	pad := float32(a.cfg.Appearance.Padding)
	w.renderer = renderer.New(
		a.theme,
		renderer.CellMetrics{Width: w.cellW, Height: w.cellH},
		parent.renderer.Font, w.fontSize,
	)
	w.renderer.FontBold = parent.renderer.FontBold
	w.renderer.OffsetX = pad
	w.renderer.OffsetY = w.tabBarH + pad
	w.renderer.BoldIsBright = a.cfg.Appearance.BoldIsBright

	// New glyph cache for this Window (own textures via the shared
	// TextureManager).
	if fontsys.Default != nil {
		primaryPath := renderer.ResolveFontPath(&a.cfg)
		if primaryPath != "" {
			fbScale := imgui.CurrentIO().DisplayFramebufferScale().X
			if fbScale <= 0 {
				fbScale = 1
			}
			// Cache uses this Window's own fontSize so glyphs are
			// rasterized at the right physical size (each Window can
			// have a different zoom).
			if c, err := glyphcache.New(fontsys.Default, platform.Textures(), primaryPath, w.fontSize, fbScale); err == nil {
				w.renderer.Glyphs = c
				w.renderer.InvalidateCellCache()
			}
		}
	}

	// New tabs manager with a single starting tab. cfg.Tabs.InheritCWD
	// makes the new Window's shell start in the parent Window's active
	// tab's CWD — Cmd+N from inside ~/src/foo gives a new Window also
	// in ~/src/foo, matching iTerm/Terminal.app's behavior.
	w.tabs = tabs.NewManager(&a.cfg)
	a.installSourceFactory(w)
	cols, rows := w.gridSize()

	// Daemon-side window association. Three cases:
	//   1. adopt != nil   — cross-Window tab drag, no fresh daemon
	//                       window needed; the dragged source keeps
	//                       its existing daemon-window membership
	//                       (TODO: emit SendWindowMoveTab here when
	//                       the source is daemon-backed).
	//   2. adoptQueue !=  — reattach restore: pop the next daemon
	//      empty (daemon)   window snapshot, claim its ID, adopt
	//                       its tabs.
	//   3. fresh daemon   — SendWindowCreate to mint a new daemon
	//      window mode      window; Hub.SetDefaultWindowID makes
	//                       NewTab put new tabs there.
	if a.suppressInitialTab {
		// Empty window — caller (remote reattach) adopts tabs
		// itself. Skip all tab creation. The window still gets a
		// daemon-window association lazily via windowIDForHub when
		// the first tab is adopted.
	} else if adopt != nil {
		if cols > 1 && rows > 1 {
			adopt.Resize(cols, rows)
		}
		newTab := w.tabs.AdoptTab(adopt)
		// Cross-window tab drag: persist the move to the owning
		// daemon NOW (minting this window's daemon window
		// synchronously). Deferring this left a window where the
		// daemon's topology disagreed with the GUI — new tabs
		// routed to the old daemon window and the next broadcast
		// yanked the dragged tab back to its old group.
		w.persistDaemonTabMove(adopt)
		_ = newTab
	} else if len(a.daemonAdoptQueue) > 0 && a.daemonHub != nil {
		snap := a.daemonAdoptQueue[0]
		a.daemonAdoptQueue = a.daemonAdoptQueue[1:]
		w.daemonWindowID = snap.WindowID
		a.installSourceFactory(w)
		a.daemonHub.SetDefaultWindowID(snap.WindowID)
		// Restore the recorded detach-time geometry (which includes
		// the tab bar) over the cols×rows estimate; pendingResize is
		// already set, so the next Begin applies it. Recorded
		// position beats the cascade offset for the same reason.
		if snap.Width > 0 && snap.Height > 0 {
			w.width, w.height = int(snap.Width), int(snap.Height)
			w.restoredGeom = true
			w.restoredWithBar = len(snap.Tabs) > 1
		}
		if snap.PosX != 0 || snap.PosY != 0 {
			w.initialPosX, w.initialPosY = float32(snap.PosX), float32(snap.PosY)
			w.hasInitialPos = true
		}
		// Recompute the grid from the RESTORED geometry: cols,rows
		// above were measured before snap.Width/Height was applied,
		// so they size the adopted sources wrong. The per-frame size
		// sync only calls resizeTerminals() when the detected window
		// size DIFFERS from w.width/w.height — but pendingResize
		// opens the OS window at exactly the restored size, so
		// detected==stored and no resize ever fires. That left the
		// content not filling to the bottom until the user manually
		// resized (the only thing that makes detected != stored).
		aCols, aRows := w.gridSize()
		var focusIdx = -1
		for i, ts := range snap.Tabs {
			src := a.daemonHub.Adopt(ts.ID, int(ts.Cols), int(ts.Rows))
			tab := w.tabs.AdoptTab(src)
			if ts.Title != "" {
				tab.SetTitle(ts.Title)
			}
			if aCols > 1 && aRows > 1 {
				src.Resize(aCols, aRows)
			}
			if ts.ID == snap.FocusedTabID {
				focusIdx = i
			}
		}
		// Honor the daemon's record of which tab was front, not
		// the AdoptTab default of "last one wins". Reattach
		// shouldn't surprise the user with a different focus
		// than they left.
		if focusIdx >= 0 && focusIdx < len(w.tabs.Tabs) {
			w.tabs.ActiveIdx = focusIdx
			w.tabSwitchReq = w.tabs.Tabs[focusIdx].ID
		}
		// (Geometry pushes are owned by the per-frame pushGeometry
		// dedupe now — no one-shot push here; it would just re-send
		// the restored values.)
	} else if a.daemonHub != nil {
		// Fresh daemon window for this GUI window. CreateWindow
		// correlates the reply by ReqID (router-demuxed).
		if id, err := a.daemonHub.CreateWindow(0, 0, int32(w.width), int32(w.height)); err == nil {
			w.daemonWindowID = id
			a.daemonHub.SetDefaultWindowID(w.daemonWindowID)
		}
		// else: daemon didn't respond; new tabs land in its default
		// window. Non-fatal.
		// A forwarded single-instance launch carries the invoking
		// shell's directory; it wins over InheritCWD, which is about
		// Cmd+N from inside the GUI inheriting the parent tab.
		cwd := a.spawnCWD
		if cwd == "" && a.cfg.Tabs.InheritCWD && parent != nil {
			if parentTab := parent.tabs.Active(); parentTab != nil && parentTab.Terminal != nil {
				cwd = parentTab.Terminal.GetCWD()
			}
		}
		if _, err := w.tabs.NewTabCmd(cols, rows, cwd, a.spawnCmd); err != nil {
			return
		}
	} else {
		// A forwarded single-instance launch carries the invoking
		// shell's directory; it wins over InheritCWD, which is about
		// Cmd+N from inside the GUI inheriting the parent tab.
		cwd := a.spawnCWD
		if cwd == "" && a.cfg.Tabs.InheritCWD && parent != nil {
			if parentTab := parent.tabs.Active(); parentTab != nil && parentTab.Terminal != nil {
				cwd = parentTab.Terminal.GetCWD()
			}
		}
		if _, err := w.tabs.NewTabCmd(cols, rows, cwd, a.spawnCmd); err != nil {
			return
		}
	}

	// Skip the main-Window-specific first-frame init (CreateWindow,
	// SetWindowSize, etc.) — multi-viewport handles the platform
	// window creation for us when the Begin/End wrapper runs.
	w.ready = true
	// Flag this Window for OS-focus override once its viewport exists.
	// Without this, the first frame's `focused` calc (driven by ImGui's
	// IsWindowFocused) returns the spawning Window because OS focus
	// hasn't transitioned yet — clobbering our manual a.active=w
	// assignment below and routing the next keybind to the wrong
	// Window. The override is cleared once we've raised the new SDL
	// window in wrappedFrame's post-loop block.
	w.pendingFocus = true

	a.windows = append(a.windows, w)
	a.active = w
}

// initialWindowSize returns the pixel dimensions for the SDL window based on
// the configured columns/rows and an estimate of cell metrics. The estimate
// is corrected on the first frame once the font atlas is measured.
func (w *Window) initialWindowSize() (int, int) {
	px := renderer.PixelSize(&w.app.cfg)
	estCellW := px * 0.6
	estCellH := px * 1.2
	cols, rows := w.app.cfg.Window.Columns, w.app.cfg.Window.Rows
	if cols < 2 {
		cols = 80
	}
	if rows < 2 {
		rows = 24
	}
	pad := float32(w.app.cfg.Appearance.Padding) * 2
	// Add cellSafetyMargin so the eventual gridSize() after window creation
	// computes back to the same cols/rows we requested.
	wp := int(math.Ceil(float64(float32(cols)*estCellW + pad + cellSafetyMarginH + cellOriginInsetX)))
	hp := int(math.Ceil(float64(float32(rows)*estCellH + pad + cellSafetyMarginV)))
	return wp, hp
}

// idleSafetyNetMs bounds how long the render loop parks when nothing is
// animating (no blinking cursor). It's a safety net, NOT a real timer:
// every async state change (PTY/daemon data, topology, proposals, …)
// wakes the loop immediately, so this only catches a wake we forgot —
// 1 fps idle is ~0% CPU but never lets the UI freeze for over a second.
const idleSafetyNetMs = 1000

// writeScreenshot captures the main window's framebuffer to a PNG.
func writeScreenshot(path string) error {
	pix, w, h, ok := platform.CollectCapture()
	if !ok {
		return fmt.Errorf("framebuffer capture did not complete")
	}
	img := &image.RGBA{Pix: pix, Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
	// Readback alpha can be <255 where the terminal background is
	// translucent; force opaque so diffs compare color, not blend.
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0xFF
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// Run starts the application main loop.
func (a *App) Run() error {
	w := a.active // initial Window allocated by New()

	// Load theme
	theme, err := themes.Load(a.cfg.Appearance.Theme)
	if err != nil {
		theme = renderer.DefaultTheme()
	}
	applyColorOverrides(&theme, &a.cfg)
	a.theme = theme

	w.width, w.height = w.initialWindowSize()

	// Reattach restore: this (main) Window adopts the first queued
	// daemon-window snapshot, so prefer that snapshot's recorded
	// geometry over the configured cols×rows estimate. The recorded
	// size is the live size at detach — INCLUDING the tab bar — so a
	// multi-tab window comes back with its full grid instead of
	// losing a row to the bar (the cols×rows estimate assumes a
	// fresh window with no bar).
	if len(a.daemonAdoptQueue) > 0 {
		snap := a.daemonAdoptQueue[0]
		if snap.Width > 0 && snap.Height > 0 {
			w.width, w.height = int(snap.Width), int(snap.Height)
			w.restoredGeom = true
			w.restoredWithBar = len(snap.Tabs) > 1
		}
		if snap.PosX != 0 || snap.PosY != 0 {
			w.initialPosX, w.initialPosY = float32(snap.PosX), float32(snap.PosY)
			w.hasInitialPos = true
		}
	}

	// platform.Init creates the SDL3 window + GL context and brings up
	// Dear ImGui with the SDL3 + OpenGL3 backends. Replaces cimgui-go's
	// backend.CreateBackend + sdlbackend.NewSDLBackend + CreateWindow
	// chain — single call, no hidden carrier window (the OS window IS
	// the main window).
	// renderer="gpu" from config opts into the SDL_GPU backend for
	// launches that can't carry env vars (Finder / app menus). Env
	// always wins so a terminal A/B can override the config.
	if os.Getenv("XEROTTY_GPU") == "" {
		switch strings.ToLower(a.cfg.Renderer) {
		case "gpu", "sdlgpu", "sdl_gpu":
			os.Setenv("XEROTTY_GPU", "1")
		}
	}
	if err := platform.Init("xerotty", w.width, w.height); err != nil {
		return fmt.Errorf("platform.Init: %w", err)
	}

	// Font reload runs BEFORE every frame's NewFrame. The alternative
	// (doing it inside our frame() callback after NewFrame already
	// snapshotted the current font) leaves a freed font in ImGui's
	// per-frame state and asserts when Render dereferences it.
	platform.SetBeforeRenderHook(a.beforeRender)

	// Hidden-carrier model: the SDL_Window platform.Init created holds
	// the GL context but isn't user-visible. Every Window — including
	// the first — pops out as its own OS window via ImGui multi-
	// viewport. Same approach as the SDL2 design; the platform layer
	// just needs an explicit hide call now.
	platform.HideMainWindow()
	w.imguiName = "xerottywin0"
	// The first visible terminal is also a multi-viewport pop-out, so it
	// needs the same explicit native focus handoff as later Cmd+N windows.
	// Otherwise macOS can leave keyboard focus on the hidden carrier until
	// the user clicks the terminal once.
	w.pendingFocus = true

	// Set background color from theme (ABGR → RGBA for the GL clear).
	bgR := float32((theme.Background>>0)&0xFF) / 255.0
	bgG := float32((theme.Background>>8)&0xFF) / 255.0
	bgB := float32((theme.Background>>16)&0xFF) / 255.0
	platform.SetBgColor(imgui.NewVec4(bgR, bgG, bgB, 1.0))
	// (Old SetTargetFPS(120) removed — the platform's WaitEventTimeout
	// loop is event-driven and self-paces to the display refresh.)

	// Keep multi-viewport enabled so child ImGui windows (preferences, etc.)
	// can be dragged out as native OS windows. The SDL backend already enables
	// this during init — re-asserting the bit defends against future changes
	// to that default. Coordinate-space conversions for mouse/scrollbar/draw
	// lists below use vp.Pos() to translate between desktop-absolute and
	// window-local pixels.
	io := imgui.CurrentIO()
	io.SetConfigFlags(io.ConfigFlags() | imgui.ConfigFlagsViewportsEnable)

	// Load font into atlas (must be after CreateWindow, before first frame)
	font, fontBold := renderer.LoadFont(&a.cfg)

	// Approximate metrics until first frame measures real ones.
	// baseFontSize is in pixels — that's what ImGui's atlas stores and what
	// gets passed to AddTextFontPtr each frame.
	pxSize := renderer.PixelSize(&a.cfg)
	a.baseFontSize = pxSize
	w.fontSize = pxSize
	w.cellW = pxSize * 0.6
	w.cellH = pxSize

	// Create renderer (metrics will be updated on first frame; the
	// OS-backed glyph cache is also built on first frame, once
	// DisplayFramebufferScale has been populated by ImGui's NewFrame).
	w.renderer = renderer.New(theme, renderer.CellMetrics{
		Width: w.cellW, Height: w.cellH,
	}, font, pxSize)
	w.renderer.FontBold = fontBold
	pad := float32(a.cfg.Appearance.Padding)
	w.renderer.OffsetX = pad
	w.renderer.OffsetY = w.tabBarH + pad
	w.renderer.BoldIsBright = a.cfg.Appearance.BoldIsBright

	// Tab manager (terminal creation deferred to first frame for accurate metrics)
	w.tabs = tabs.NewManager(&a.cfg)
	a.installSourceFactory(w)

	// Single-instance launch socket: later `xerotty` runs in this
	// session forward "open a window/tab" here instead of spawning a
	// second GUI (see internal/launchipc + cmd/xerotty). Best-effort:
	// a bind failure (or an older GUI already owning the socket) just
	// means no forwarding — never a failed launch.
	if cleanup, err := launchipc.Listen(a.enqueueLaunch); err == nil {
		defer cleanup()
	}

	// macOS-only: hook an SDL event watch so a real frame still renders
	// while AppKit's live-resize tracking mode holds the run loop.
	// Without this the OS just stretches the previous GL framebuffer
	// (the "image stretch" effect) until the user releases the drag.
	// No-op on other platforms.
	//
	// The wrapped frame body brackets each call with
	// liveResizeMainFrame{Begin,End}: while the main loop is inside its
	// frame body, the watch must not drive its own NewFrame (would
	// double-NewFrame and assert). When the main loop is between
	// frames — including blocked in SDL_WaitEventTimeout — the flag is
	// clear and the watch is free to render.
	//
	// Iterate every Window each frame. The main Window renders into
	// cimgui-go's primary SDL_Window via the main viewport. Secondary
	// Windows render inside an ImGui top-level window with NoDocking
	// + ConfigFlagsViewportsEnable, which makes multi-viewport auto-
	// promote each into its own OS window. Same SDL/GL/ImGui context,
	// same NSApplication on macOS — that's the trick that gives us
	// one Dock icon for N OS windows.
	wrappedFrame := func() {
		liveResizeMainFrameBegin()
		defer liveResizeMainFrameEnd()
		// Start each frame assuming nothing animates: the loop may park
		// up to idleSafetyNetMs. The cursor-blink draw lowers this to
		// the next toggle when a focused cursor is blinking; the safety
		// net (not an infinite block) bounds any missed wake.
		a.idleWakeMs = idleSafetyNetMs
		// Lava lamp pacing rides the idle timeout (see glowIdleWakeMs)
		// instead of a dedicated Go ticker.
		if ms := a.glowIdleWakeMs(); ms > 0 && ms < a.idleWakeMs {
			a.idleWakeMs = ms
		}
		// Apply any forwarded single-instance launches before the
		// window walk so a "new window" request renders this frame.
		a.drainLaunchRequests()
		// Keep the daemons' window-geometry hints tracking the live
		// size/position (deduped to changes; values are last frame's,
		// which is fine). Reattach restores from these.
		for _, win := range a.windows {
			win.pushGeometry()
		}
		// (Modifier resync is in beforeRender — it has to run BEFORE
		// imgui.NewFrame consumes the input queue, otherwise the
		// re-asserted modifier state only takes effect next frame
		// and the current frame's KEY_DOWN edge sees a stale view.)
		// Detect "any window is currently being dragged" via the
		// live AppKit NSWindow.frame.origin, not ImGui's viewport.Pos
		// (which lags 1-2 frames during a continuous drag because
		// SDL_EVENT_WINDOW_MOVED + ImGui::UpdatePlatformWindows only
		// sync the position on the next frame).
		a.anyWindowMovingThisFrame = platform.CocoaAnyWindowMoved()
		// Track which Window has keyboard/click focus across this
		// frame. Input handling (keys, mouse-down, wheel, selection)
		// is gated to a.active in frame() — otherwise typing would
		// forward to every Window's PTY.
		var focused *Window
		for _, win := range a.windows {
			// Override the auto-derived viewport flags so the
			// platform window gets normal OS decorations (native
			// title bar, close/min/max). Without this override,
			// ImGui sees NoTitleBar on the ImGui window and
			// propagates NoDecoration to the platform viewport,
			// which makes SDL2 create the NSWindow as
			// SDL_WINDOW_BORDERLESS.
			windowClass := imgui.NewWindowClass()
			windowClass.SetViewportFlagsOverrideClear(imgui.ViewportFlagsNoDecoration)
			// NoAutoMerge: each xerotty Window must be its OWN OS
			// viewport, never merged into another window's surface.
			// Without this, a spawned window whose cascaded position
			// lands within an existing window's rect gets merged by
			// ImGui and renders INSIDE that window (e.g. "New Window"
			// from a remote-reattach window drew the new terminal on
			// top of the remote one). Mirrors the prefs dialog, which
			// already forces this for the same reason.
			windowClass.SetViewportFlagsOverrideSet(imgui.ViewportFlagsNoAutoMerge)
			imgui.SetNextWindowClass(windowClass)
			windowClass.Destroy()
			// Position outside the main viewport the first time so
			// the auto-merge logic pops it out; after that ImGui
			// remembers per-window position. spawnWindow sets
			// hasInitialPos with a cascaded offset from the parent's
			// current OS position so new windows don't stack on top
			// of the parent.
			posX, posY := float32(100), float32(100)
			if win.hasInitialPos {
				posX, posY = win.initialPosX, win.initialPosY
				win.hasInitialPos = false
			}
			imgui.SetNextWindowPosV(
				imgui.Vec2{X: posX, Y: posY},
				imgui.CondFirstUseEver, imgui.Vec2{X: 0, Y: 0})
			sizeCond := imgui.CondFirstUseEver
			if win.pendingResize {
				sizeCond = imgui.CondAlways
				win.pendingResize = false
			}
			imgui.SetNextWindowSizeV(
				imgui.Vec2{X: float32(win.width), Y: float32(win.height)},
				sizeCond)
			// NoDocking: don't merge into the main viewport.
			// NoSavedSettings: don't pollute imgui.ini with per-
			//   window geometry; we manage that ourselves.
			// NoTitleBar / NoScrollbar / NoBackground / NoCollapse:
			//   the OS chrome (native title bar via the WindowClass
			//   override above) is the user-facing chrome; the
			//   ImGui window inside is just a transparent host
			//   for our terminal draw list.
			// NoMove: with NoTitleBar set, ImGui makes the entire window
			// drag-to-move. That makes clicks on the scrollbar (or any
			// terminal cell) start a window move. The OS title bar
			// (from the WindowClass viewport-flag override above)
			// handles window movement; the wrapper must not.
			flags := imgui.WindowFlagsNoDocking |
				imgui.WindowFlagsNoSavedSettings |
				imgui.WindowFlagsNoTitleBar |
				imgui.WindowFlagsNoMove |
				imgui.WindowFlagsNoResize |
				imgui.WindowFlagsNoScrollbar |
				imgui.WindowFlagsNoScrollWithMouse |
				imgui.WindowFlagsNoBackground |
				imgui.WindowFlagsNoCollapse |
				imgui.WindowFlagsNoBringToFrontOnFocus
			// Strip the wrapper's own padding / border so it
			// occupies exactly the OS window's content rect with
			// no offset. Without these, ImGui's default 8px
			// WindowPadding + 1px WindowBorderSize push the
			// rendering subtly inside the wrapper, which makes
			// the tab bar (positioned at viewport.Pos) appear to
			// overlap the terminal's first row by a few px.
			imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, imgui.Vec2{X: 0, Y: 0})
			imgui.PushStyleVarFloat(imgui.StyleVarWindowBorderSize, 0)
			// "<displayTitle>###<stableID>" — text after ### is the
			// stable ImGui ID, text before is the display name (which
			// becomes the OS NSWindow title via ImGui's
			// Platform_SetWindowTitle callback). So the OS title
			// tracks the active tab without invalidating the ImGui
			// window's identity / saved state.
			beginName := win.titleForWindow() + "###" + win.imguiName
			// Belt-and-suspenders alongside the post-loop
			// platform.RaiseWindow: when focus has been requested for
			// this Window, ask ImGui to focus it during this Begin.
			// ImGui's platform layer translates that into the OS focus
			// call for the popped-out viewport, which on macOS
			// pre-flights the makeKeyAndOrderFront: that RaiseWindow
			// does later.
			if win.pendingFocus {
				imgui.SetNextWindowFocus()
			}
			shouldRenderFrame := imgui.BeginV(beginName, nil, flags)
			// Pop the wrapper-only styles IMMEDIATELY after Begin so
			// they only affect the wrapper window (its content rect
			// is set by ImGui from the at-Begin-time style). Without
			// this early pop, the styles stay on the stack through
			// the entire frame body, and any popup/menu/dialog
			// opened inside frame() inherits WindowPadding=0 — making
			// the right-click context menu items have zero padding.
			imgui.PopStyleVarV(2)
			if shouldRenderFrame {
				win.imViewport = imgui.WindowViewport()
				// Apply window opacity (cfg.Appearance.Opacity) via
				// SDL_SetWindowOpacity — only when it changed, so the
				// first valid frame and live prefs-slider edits both
				// take effect without calling SDL every frame. The
				// SDL2→SDL3 migration dropped this wiring; see SPEC
				// "Window opacity support".
				op := a.cfg.Appearance.Opacity
				if a.forceOpaque.Load() {
					op = 1.0 // SIGUSR1 screenshot-safe override
				}
				if op != win.appliedOpacity {
					if h := win.imViewport.PlatformHandle(); h != 0 {
						applied := op
						if applied <= 0 || applied > 1 {
							applied = 1.0 // never hide/invalidate the window
						}
						platform.SetWindowOpacity(h, applied)
						win.appliedOpacity = op
					}
				}
				// Capture the wrapper's actual content origin (NOT
				// viewport.Pos — see contentOriginY comment on
				// Window). CursorScreenPos right after Begin is the
				// authoritative top-left of the area we're allowed
				// to draw into.
				cp := imgui.CursorScreenPos()
				win.contentOriginX = cp.X
				win.contentOriginY = cp.Y
				// RootAndChildWindows so a click on the tab bar /
				// search overlay (separate ImGui windows nested
				// inside the wrapper's frame()) still counts as
				// focusing this Window — otherwise tab-bar
				// interaction would drop input gating.
				if imgui.IsWindowFocusedV(imgui.FocusedFlagsRootAndChildWindows) {
					focused = win
				}
				win.frame()
			}
			imgui.End()
			// Use viewport.PlatformRequestClose to detect the OS
			// close button — without a TitleBar the `&open` bool
			// passed to BeginV doesn't reliably propagate the close
			// (no Begin-internal close button). PlatformRequestClose
			// is set by ImGui_ImplSDL2 directly from
			// SDL_WINDOWEVENT_CLOSE so it's the source of truth.
			if win.imViewport != nil && win.imViewport.PlatformRequestClose() {
				win.imViewport.SetPlatformRequestClose(false)
				if win.swallowOSCloseFrames > 0 {
					// Suppressed: our close_tab keybind just fired
					// and the OS-level Cmd+W close event is the
					// echo of that same keystroke. See the
					// swallowOSCloseFrames doc on Window for the
					// macOS Cmd+W double-fire rationale.
				} else {
					win.pendingClose = true
				}
			}
			if win.swallowOSCloseFrames > 0 {
				win.swallowOSCloseFrames--
			}
		}
		if focused != nil {
			a.active = focused
		}
		// Override the focus-from-ImGui result if focus was explicitly
		// requested for a Window (new Window spawn, or focus returning
		// from an auxiliary viewport such as preferences). Two timing
		// issues to handle here:
		//
		//   1. ImGui's UpdatePlatformWindows runs in platform_end_frame
		//      (i.e. AFTER wrappedFrame). So on the FIRST frame after
		//      spawnWindow, the new Window's imViewport exists but its
		//      PlatformHandle is still 0 — the SDL_Window hasn't been
		//      created yet. We can't raise a window that doesn't exist.
		//      Wait for the next frame, when PlatformHandle is non-zero.
		//   2. ImGui's IsWindowFocused trails the OS keyboard focus by
		//      a frame or two on macOS. Without overriding here, the
		//      spawning Window stays a.active and the next keybind
		//      (Cmd+T immediately after Cmd+N) dispatches there
		//      instead of the new Window.
		//
		// So we keep pendingFocus set until we can BOTH raise the new
		// SDL_Window AND mark a.active. Clearing it only on successful
		// raise also handles the "spawn but viewport never gets a
		// handle" failure case gracefully.
		for _, win := range a.windows {
			if !win.pendingFocus {
				continue
			}
			if win.imViewport == nil {
				continue
			}
			handle := win.imViewport.PlatformHandle()
			if handle == 0 {
				// Viewport's SDL_Window not yet created — try next frame.
				a.active = win
				focused = win
				continue
			}
			a.active = win
			focused = win
			// SDL_RaiseWindow alone leaves macOS with deferred
			// makeKey: until the next event tick — i.e. the user has
			// to move the mouse before keybinds route to the new
			// Window. CocoaFocusWindow does NSApp.activate +
			// NSWindow.makeKeyAndOrderFront: directly so firstResponder
			// transitions synchronously, this frame.
			platform.RaiseWindow(handle)
			platform.CocoaFocusWindow(handle)
			// macOS drops modifier KEY_UP events during NSWindow
			// focus transitions, so a Cmd held through Cmd+N
			// → Cmd+T leaves ImGui thinking Cmd was released —
			// breaking the immediately-following Cmd+T keybind.
			// Re-assert the actual OS modifier state.
			platform.ResyncModifiers()
			win.pendingFocus = false
			break
		}
		if a.active != nil && a.active.pendingClose {
			// Active Window is being closed this frame — pick
			// another so input has somewhere to land for the
			// rest of the frame body, and pull OS focus to it so
			// keybinds in the remaining Window work immediately
			// (without this, macOS leaves keyboard focus on the
			// dying window's prior peer and the user has to click
			// the survivor before keys route to it).
			a.active = nil
			if win := a.bestFocusReplacement(); win != nil {
				a.active = win
				focused = win
				if win.imViewport != nil {
					if h := win.imViewport.PlatformHandle(); h != 0 {
						platform.RaiseWindow(h)
						platform.CocoaFocusWindow(h)
						platform.ResyncModifiers()
					}
				}
			}
		}
		// Process any closes deferred during the render pass. If
		// every Window is gone after the reap, the process quits.
		a.reapClosedWindows()
		if len(a.windows) == 0 {
			platform.Quit()
		}
		a.updateTabDragDrop()
		// Reconcile any topology snapshots from other clients / MCP
		// agents into the GUI tab bar (add newly-created tabs).
		a.applyPendingTopology()
		// Republish the focused-Source snapshot for the guimcp
		// goroutine now that this frame's window/tab mutations are
		// settled (see publishFocusedSources).
		a.publishFocusedSources()
		// Tell the render loop how long it may park before the next
		// forced frame (next blink toggle, or the safety net).
		platform.SetIdleTimeout(a.idleWakeMs)
	}
	installLiveResizeWatch(bgR, bgG, bgB, wrappedFrame, a.beforeRender)

	// Wake the platform loop when any PTY produces new data. Without
	// this the loop sleeps in WaitEventTimeout until the next timeout
	// (cursor blink etc.) and the user sees up to ~500ms latency on
	// incoming bytes.
	terminal.Wake = platform.PostWake
	// Same for daemon-backed tabs: the Hub router applies cell/cursor/
	// title/bell frames on its own goroutine; without this the now
	// event-driven loop wouldn't repaint remote output until its next
	// idle timeout.
	daemonsource.Wake = platform.PostWake

	stopMouseWakePoller := a.startMouseMirrorWakePoller()
	defer stopMouseWakePoller()

	// Screenshot mode (--screenshot): run a fixed number of frames
	// so layout/fonts settle, capture the framebuffer, write a PNG,
	// and exit. The CI visual-regression harness builds on this.
	platform.SetDamageEnabled(os.Getenv("XEROTTY_NO_DAMAGE") == "" && os.Getenv("XEROTTY_SCREENSHOT") == "")
	shotPath := os.Getenv("XEROTTY_SCREENSHOT")
	shotFrames := 8
	if n, err := strconv.Atoi(os.Getenv("XEROTTY_SCREENSHOT_FRAMES")); err == nil && n > 0 {
		shotFrames = n
	}
	frameN := 0

	// Main loop — event-driven via SDL_WaitEventTimeout inside
	// platform.Frame. CPU goes to kernel-sleep when nothing's
	// happening; PTY arrival, mouse, keys, timers all wake it.
	// beforeRender was registered above via platform.SetBeforeRenderHook
	// and fires automatically before each NewFrame inside Frame().
	for platform.Frame(wrappedFrame) {
		if shotPath == "" {
			continue
		}
		frameN++
		platform.PostWake() // keep frames flowing while settling
		if frameN == shotFrames-1 {
			// Arm: the NEXT frame is read back pre-swap. Target the
			// active terminal window's viewport — under multi-viewport
			// it's its own OS window, not the main scaffolding one.
			var winID uintptr
			if a.active != nil && a.active.imViewport != nil {
				winID = uintptr(a.active.imViewport.PlatformHandle())
			}
			platform.RequestCapture(winID, 8192, 8192)
		}
		if frameN < shotFrames {
			continue
		}
		if err := writeScreenshot(shotPath); err != nil {
			fmt.Fprintf(os.Stderr, "xerotty: screenshot: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(shotPath)
		break
	}
	platform.Shutdown()

	// Cleanup: detach from all tabs in every Window before exiting.
	// Daemon-backed tabs survive (the daemon process keeps the
	// shells alive); in-process PTY tabs get torn down (Detach is
	// Close for them).
	for _, win := range a.windows {
		if win.tabs == nil {
			continue
		}
		for _, tab := range win.tabs.Tabs {
			tab.Terminal.Detach()
		}
	}

	return nil
}

// cellSafetyMarginH / cellSafetyMarginV reserve extra pixels of gutter
// between the cell grid and the window edge, beyond the user-visible
// Appearance.Padding. Two reasons:
//
//  1. Glyph AA spill: CalcTextSize returns the font's advance width, but
//     font hinting / anti-aliasing can nudge glyph edge pixels slightly
//     past cellW. Without margin, when the floor-fitted cell count
//     produces a tight right-edge gutter, the rightmost glyph's AA edge
//     renders over the window boundary and appears clipped.
//
//  2. macOS rounded NSWindow corners (~5 px radius on all four corners)
//     mask any content rendering at the very edge. The bottom row's
//     leftmost/rightmost glyphs and the top row's corners get clipped
//     if cells run flush to the window bounds.
//
// Together they translate to "more padding on macOS than other
// platforms" — same reason iTerm uses noticeable inset.
//
// Must be added BACK to the window size in any code that wants to fit a
// specific cols×rows grid (e.g. font zoom resize), otherwise the next
// gridSize call will floor away one cell.
var (
	cellSafetyMarginH = func() float32 {
		if runtime.GOOS == "darwin" {
			return 12 // 8 for glyph AA + 4 for right corner radius
		}
		return 8
	}()
	cellSafetyMarginV = func() float32 {
		if runtime.GOOS == "darwin" {
			return 6 // covers bottom corner radius
		}
		return 0
	}()
	// cellOriginInsetX is added to OffsetX on macOS so the LEFT column
	// also clears the rounded window corner. cellSafetyMarginH alone
	// only reserves right-edge gutter; without this, the bottom-left
	// glyph's lower pixels get clipped by the corner mask.
	cellOriginInsetX = func() float32 {
		if runtime.GOOS == "darwin" {
			return 4
		}
		return 0
	}()
)

func (w *Window) gridSize() (cols, rows int) {
	pad := float32(w.app.cfg.Appearance.Padding) * 2 // padding on both sides
	availW := float32(w.width) - pad - cellSafetyMarginH - cellOriginInsetX
	availH := float32(w.height) - w.tabBarH - pad - cellSafetyMarginV
	cols = int(availW / w.cellW)
	rows = int(availH / w.cellH)
	if cols < 2 {
		cols = 2
	}
	if rows < 2 {
		rows = 2
	}
	return
}

// measureCell returns the cell width/height for the current font. When
// the OS-backed glyph cache is active, it uses the cache's primary-only
// metrics — that's just the user's chosen monospace font, no influence
// from any merged fallback. Falls back to ImGui's atlas-based
// MeasureCell when no cache is available (Linux until fontsys is
// implemented there).
//
// Cell height is ascent + descent (no leading) — terminals traditionally
// pack rows tightly. Cell width is the primary font's M advance.
//
// Returns the FLOAT advance straight from the font. Callers ceil
// `baseCellW * scale` (or `baseCellW * 1` at base zoom) when storing
// into the layout `cellW`. Storing baseCellW pre-ceil is what makes
// font-zoom scale linearly: at 1.0833× zoom of a 7.2 px advance you
// want ceil(7.8)=8, not ceil(8 * 1.0833)=10. (The latter is what we
// get if ceil happens at measure time and then again after scaling —
// the ceiling compounds and cells drift wider than the font wants on
// every zoom step.)
func (w *Window) measureCell() renderer.CellMetrics {
	if w.renderer != nil && w.renderer.Glyphs != nil {
		lm := w.renderer.Glyphs.LineMetrics()
		adv := w.renderer.Glyphs.PrimaryAdvance()
		h := lm.Ascent + lm.Descent
		if adv > 0 && h > 0 {
			return renderer.CellMetrics{Width: adv, Height: h}
		}
	}
	return renderer.MeasureCell()
}

// ceilCell ceils the cell metrics to whole logical pixels for the
// layout `cellW`/`cellH`. AppKit's setContentResizeIncrements rounds
// to integer points and gridSize()'s int(W/cellW) needs to match;
// integer cellW makes both deterministic. Ceil (vs round) keeps the
// cell at least as wide/tall as the font wants — glyphs never clip,
// box-drawing rects tile with no gaps, wide chars never overlap.
func ceilCell(w, h float32) (float32, float32) {
	return float32(math.Ceil(float64(w))), float32(math.Ceil(float64(h)))
}

func (w *Window) resizeTerminals() {
	cols, rows := w.gridSize()
	for _, tab := range w.tabs.Tabs {
		tab.Terminal.Resize(cols, rows)
	}
}

// beforeRender runs before every NewFrame — both via cimgui-go's
// SetBeforeRenderHook in the main loop and via the macOS live-resize
// watch in liveresize_darwin.go. Has to live BEFORE NewFrame because
// font reloads invalidate ImGui's per-frame font pointer; doing the
// reload mid-frame would assert in Render.
//
// Font reloads apply process-wide: every Window picks up the new
// font, glyph cache, and metrics. Each Window's renderer keeps its
// own *renderer.Renderer instance (so per-window theming stays
// possible later) but they all share the new Font/FontBold and a
// freshly-built glyph cache.
func (a *App) beforeRender() {
	// Resync modifier state every frame BEFORE imgui.NewFrame consumes
	// the input queue. macOS drops phantom KEY_UPs for held modifiers
	// during NSWindow focus transitions; SDL's modifier view inherits
	// that lie. NSEvent.modifierFlags reads the IOHID hardware state,
	// which stays correct across transitions. Done here (not in
	// wrappedFrame) because AddKeyEvent queues for the NEXT NewFrame
	// — calling it after NewFrame means the key edge for the current
	// frame's KEY_DOWN still sees the stale modifier and the keybind
	// match fails.
	platform.ResyncModifiers()

	if !a.pendingFontFace {
		return
	}
	a.pendingFontFace = false
	font, fontBold, _ := renderer.ReloadFont(&a.cfg)
	a.baseFontSize = renderer.PixelSize(&a.cfg)
	for _, w := range a.windows {
		if w.renderer != nil {
			w.renderer.Font = font
			w.renderer.FontBold = fontBold
			if w.renderer.Glyphs != nil {
				w.renderer.Glyphs.Close()
				w.renderer.Glyphs = nil
				w.renderer.InvalidateCellCache()
			}
		}
	}
	if fontsys.Default != nil {
		primaryPath := renderer.ResolveFontPath(&a.cfg)
		if primaryPath != "" {
			fbScale := imgui.CurrentIO().DisplayFramebufferScale().X
			if fbScale <= 0 {
				fbScale = 1
			}
			for _, w := range a.windows {
				if w.renderer == nil {
					continue
				}
				// Each Window has its own font size — rebuild the
				// cache at the Window's current zoom, not the
				// app-level base.
				size := w.fontSize
				if size <= 0 {
					size = a.baseFontSize
				}
				if c, err := glyphcache.New(fontsys.Default, platform.Textures(), primaryPath, size, fbScale); err == nil {
					w.renderer.Glyphs = c
					w.renderer.InvalidateCellCache()
				}
			}
		}
	}
	// Each Window measures cells in its own frame() — every Window can
	// be at a different font zoom, so a single shared "re-measure now"
	// flag would be consumed by whichever Window framed first and
	// stamp those metrics as if they applied everywhere. Per-Window
	// flags let each Window run its own measureCell() against its own
	// glyph cache; the first to consume also sets the app-wide
	// baseCellW/H (scaled back from its local fontSize).
	for _, w := range a.windows {
		w.pendingRemeasure = true
	}
}

// updateTabDragDrop advances and finalizes an in-flight cross-window tab drag.
// It runs once per app frame after all Windows rendered, so drop resolution is
// deterministic instead of whichever Window happens to frame first.
func (a *App) updateTabDragDrop() {
	d := a.dragTab
	if d == nil {
		return
	}
	// Update last-seen focus while drag is in flight. We do this in
	// every frame, but don't rely on a source-window match by itself:
	// Wayland's implicit pointer grab can keep reporting the source
	// surface even after the cursor is physically over another window.
	if cur := platform.MouseFocusWindowID(); cur != 0 {
		d.LastFocus = cur
	}
	a.refreshWaylandTabDrop(d)

	mouseDown := imgui.IsMouseDown(imgui.MouseButtonLeft)
	if d.WaylandStarted && (d.WaylandDropSeen || !platform.WaylandDragActive()) {
		mouseDown = false
	}
	if mouseDown {
		return
	}

	target, wait := a.tabDropTarget(d)
	if wait {
		return
	}
	if target != nil {
		target.adoptDraggedTab(d)
		a.dragTab = nil
		return
	}

	a.spawnWindowAdopting(d)
	a.dragTab = nil
}

func (a *App) tabDropTarget(d *tabDrag) (*Window, bool) {
	a.refreshWaylandTabDrop(d)
	if d.WaylandStarted && !d.WaylandDropSeen {
		if target := platform.WaylandDropTarget(); target != 0 && !platform.WaylandDragActive() {
			d.WaylandDropSeen = true
			d.WaylandDropSurface = target
		} else if platform.WaylandDragActive() {
			return nil, true
		}
	}

	if d.WaylandDropSeen {
		if d.WaylandDropSurface == 0 {
			return nil, false
		}
		for _, w := range a.windows {
			if platform.WindowWLSurfacePtr(w.sdlWindowHandle()) == d.WaylandDropSurface {
				return w, false
			}
		}
		return nil, false
	}

	mp := imgui.MousePos()
	mouseValid := validMousePos(mp)
	if mouseValid && platform.VideoDriver() != "wayland" {
		for i := len(a.windows) - 1; i >= 0; i-- {
			if a.windows[i].containsMousePos(mp) {
				return a.windows[i], false
			}
		}
	}

	if d.LastFocus != 0 {
		for _, w := range a.windows {
			if w.sdlWindowHandle() != d.LastFocus {
				continue
			}
			if w == d.From && (!mouseValid || !w.containsMousePos(mp)) {
				return nil, false
			}
			return w, false
		}
	}

	if mouseValid {
		for i := len(a.windows) - 1; i >= 0; i-- {
			if a.windows[i].containsMousePos(mp) {
				return a.windows[i], false
			}
		}
	}

	return nil, false
}

func (a *App) refreshWaylandTabDrop(d *tabDrag) {
	if !d.WaylandStarted || d.WaylandDropSeen {
		return
	}
	if platform.WaylandDropFired() {
		d.WaylandDropSeen = true
		d.WaylandDropSurface = platform.WaylandDropTarget()
	}
}

func (w *Window) adoptDraggedTab(d *tabDrag) {
	cols, rows := w.gridSize()
	if cols > 1 && rows > 1 {
		d.Term.Resize(cols, rows)
	}
	tab := w.tabs.AdoptTab(d.Term)
	// Carry identity across the drag: AdoptTab starts with an empty
	// title, and the fallback (foreground process name) can be
	// nonsense — Claude Code's binary is literally a file named
	// after its version, so dropped titles showed "2.1.172". Daemon
	// tabs would self-heal on the next TabState tick; PTY tabs never
	// would.
	tab.Host = d.Host
	if d.Title != "" {
		tab.SetTitle(d.Title)
	}
	w.persistDaemonTabMove(d.Term)
}

// persistDaemonTabMove pushes a tab's new GUI-window membership to
// its owning daemon, SYNCHRONOUSLY minting this window's daemon
// window if it doesn't have one yet. Called whenever a drag lands a
// daemon-backed tab in a window — without it the daemon's topology
// still shows the OLD window, and the next broadcast (e.g. from a
// new_tab) "corrects" the GUI by yanking the tab back, while the
// stale daemonWindowID==0 routed this window's new tabs into the
// daemon's default (old) window. Falls back to the per-frame
// pendingDaemonMove drain only when the mint fails (daemon busy).
func (w *Window) persistDaemonTabMove(src terminal.Source) {
	ds, ok := src.(*daemonsource.Source)
	if !ok {
		return // in-process PTY tab — nothing daemon-side to persist
	}
	hub := w.app.findHubForClient(ds.HubClient())
	if hub == nil {
		return
	}
	id := w.windowIDForHub(hub) // mints via CreateWindow when needed
	if id == 0 {
		w.pendingDaemonMove = src // daemon didn't answer; retry per frame
		return
	}
	if hub == w.app.daemonHub && w.daemonWindowID == 0 {
		// SourceFactory routes new tabs by daemonWindowID — set it so
		// "new tab" in this window lands HERE, not in the daemon's
		// default (the dragged-from) window.
		w.daemonWindowID = id
	}
	_ = ds.HubClient().SendWindowMoveTab(ds.TabID(), id, -1)
}

func validMousePos(mp imgui.Vec2) bool {
	const noMouseSentinel = -3.4e+37
	return mp.X > noMouseSentinel && mp.Y > noMouseSentinel
}

func (w *Window) containsMousePos(mp imgui.Vec2) bool {
	return validMousePos(mp) &&
		mp.X >= w.contentOriginX &&
		mp.X < w.contentOriginX+float32(w.width) &&
		mp.Y >= w.contentOriginY &&
		mp.Y < w.contentOriginY+float32(w.height)
}

// syncDaemonFocus checks if the active tab changed since last
// frame and, if so, broadcasts the new focus to the OWNING
// daemon for that tab. Critical detail: the focused tab might
// be on a remote daemon (via cfg.Hosts) — sending its tab ID
// to the local daemon would be wrong (mismatched ID spaces).
// Route through Source.HubClient() so each daemon gets focus
// updates for its own tabs only.
//
// Window IDs are scoped per-daemon too: a.daemonWindowID is only
// valid for the LOCAL daemon (the one we created the GUI window
// against). Remote tabs don't have a local-window association,
// so SendWindowFocusTab is skipped for them. A future polish
// could track per-(GUI window, remote daemon) window IDs.
//
// Cheap per-frame: one type-assert + a uint32 compare. Only sends
// when the focus changes so the wire stays quiet during normal
// tab use.
func (a *Window) syncDaemonFocus() {
	// Drain any deferred cross-window-drag move. Once the
	// destination window has a daemon-window ID on the dragged
	// tab's HUB (lazily minted by windowIDForHub), tell that
	// daemon the tab moved here.
	if a.pendingDaemonMove != nil {
		if ds, ok := a.pendingDaemonMove.(*daemonsource.Source); ok {
			if hub := a.app.findHubForClient(ds.HubClient()); hub != nil {
				if id := a.windowIDForHub(hub); id != 0 {
					_ = ds.HubClient().SendWindowMoveTab(ds.TabID(), id, -1)
					a.pendingDaemonMove = nil
				}
			} else {
				// Non-daemon tab — nothing to persist.
				a.pendingDaemonMove = nil
			}
		} else {
			a.pendingDaemonMove = nil
		}
	}
	tab := a.tabs.Active()
	var cur uint32
	var ds *daemonsource.Source
	if tab != nil && tab.Terminal != nil {
		// Only daemon-backed sources have a daemon tab ID. PTY
		// tabs don't (and shouldn't generate focus messages).
		var ok bool
		if ds, ok = tab.Terminal.(*daemonsource.Source); ok {
			cur = ds.TabID()
		}
	}
	if cur == a.lastSentFocusTabID {
		return
	}
	a.lastSentFocusTabID = cur
	if cur == 0 || ds == nil {
		return
	}
	cli := ds.HubClient()
	_ = cli.SendTabFocus(cur)
	// Window-focus needs the daemon-window ID FOR THAT TAB'S
	// HUB. windowIDForHub does the per-hub lookup (lazily
	// minting a remote-side daemon window on first use). Without
	// the per-hub map, a remote tab's focus would send the
	// local-daemon's window ID to the remote daemon —
	// mismatched ID spaces.
	hub := a.app.findHubForClient(cli)
	if hub != nil {
		if id := a.windowIDForHub(hub); id != 0 {
			_ = cli.SendWindowFocusTab(id, cur)
		}
	}
}

// findHubForClient maps a clientproto.Client back to its
// daemonsource.Hub by walking the registry (local + remote).
// Used for "I have a Source's Client; which hub does it belong
// to?" lookups during cross-hub focus + move operations.
func (a *App) findHubForClient(cli *clientproto.Client) *daemonsource.Hub {
	if a.daemonHub != nil && a.daemonHub.Client() == cli {
		return a.daemonHub
	}
	a.remoteHubsMu.Lock()
	defer a.remoteHubsMu.Unlock()
	for _, e := range a.remoteHubs {
		if e.hub.Client() == cli {
			return e.hub
		}
	}
	return nil
}

func (a *Window) frame() {
	a.syncDaemonFocus()
	a.app.pollClipboardForDaemons()

	// macOS: after the first click that shifts the Cocoa first-responder,
	// SDL2 stops receiving subsequent mouse-button NSEvents — neither
	// presses nor releases reach the SDL event queue, so ImGui sees no
	// up→down transitions and tab clicks vanish. Bypass the broken event
	// path by polling the OS-level mouse-button state directly each frame
	// and injecting synthetic events into ImGui whenever its view of the
	// button diverges from reality.
	//
	// Asymmetric: only inject DOWN when the cursor is inside the main
	// window's content rect AND we're not in a live-resize-driven frame.
	// AppKit consumes clicks on window frames, resize handles, and
	// popped-out viewport title bars without delivering them to SDL —
	// without these guards the OS button-down poll would manufacture a
	// fake terminal click out of the window-management gesture and
	// start a phantom selection drag. Releases always inject so a real
	// drag-then-release that ends outside content still clears state.
	// Mouse mirror only runs for the active Window — the underlying
	// ImGui IO is global per-context, so injecting events here once
	// per active frame is correct; injecting from every Window per
	// frame would fire the same mouse-down event N times.
	if runtime.GOOS == "darwin" && a == a.app.active && !disableMirror {
		osDown := sdlhack.LeftButtonPhysicalDown()
		// Transition-based mirror: inject events on every OS button
		// edge, NOT just when ImGui's view diverges from OS state.
		// Diffing against ImGui's view (the previous approach) misses
		// edges when ImGui happens to already have the right state
		// from a partial event delivery — but the click never registers
		// as "fresh" because MouseClicked needs a transition. Injecting
		// on every real OS transition guarantees ImGui sees the
		// transition even if a real event also got through.
		// macOS window-drag commit takes ~100-200ms. The delay
		// only affects SDL-dropped clicks; SDL-delivered clicks
		// skip the mirror entirely.
		const mirrorDeferFrames = 15
		// Cursor motion during defer means this is a drag gesture
		// (window drag or selection drag). Both should abort the
		// synthetic DOWN.
		const mirrorAbortMotionPx = 6
		dbg := os.Getenv("XEROTTY_DEBUG_MIRROR") != ""
		imguiSeesDown := imgui.IsMouseDown(imgui.MouseButtonLeft)
		pendingCursorMoved := func() (bool, int, int) {
			gx, gy := sdlhack.GlobalMousePos()
			dx := gx - a.app.mirrorPendingGlobalX
			dy := gy - a.app.mirrorPendingGlobalY
			return dx*dx+dy*dy >= mirrorAbortMotionPx*mirrorAbortMotionPx, dx, dy
		}
		abortPendingReason := func() string {
			// A click that deactivated the app belongs to ANOTHER
			// application — the IOHID-level button probe sees every
			// click on the desktop, and the rectangle test below
			// can't tell when a foreign window is stacked ON TOP of
			// ours. NSApp.isActive is the authoritative "that click
			// was not for us" signal (reported as: clicking another
			// app un-highlighted text / page-jumped the scrollbar
			// when the windows overlapped).
			if !platform.CocoaAppActive() {
				return "app deactivated"
			}
			if a.app.anyWindowMovingThisFrame {
				return "window moved"
			}
			if moved, dx, dy := pendingCursorMoved(); moved {
				return fmt.Sprintf("cursor moved by %d,%d", dx, dy)
			}
			if platform.CocoaEventOnChrome() {
				return "cursor on chrome"
			}
			if over, _ := a.app.mouseOverOwnedContentPos(); !over {
				return "cursor left owned content"
			}
			return ""
		}
		switch {
		case osDown && !a.app.prevOsLeftDown:
			// OS-level press edge. If ImGui already sees the button
			// down, SDL delivered the click through the normal path
			// (contentView) — the mirror has nothing to compensate
			// for and any injection here would be a duplicate edge
			// (ImGui's double-click detector then fires word-select,
			// which the user sees as "click highlights stuff").
			//
			// Only defer-and-inject when SDL DIDN'T deliver — that's
			// either a real-click-that-SDL-dropped (mirror's
			// original purpose) or a window-drag gesture (which we
			// detect via motion during the defer window and abort).
			if imguiSeesDown {
				if dbg {
					fmt.Fprintf(os.Stderr, "[mirror] DOWN edge: SDL already delivered -> skip\n")
				}
				break
			}
			over, inViewportPos := a.app.mouseOverOwnedContentPos()
			onChrome := platform.CocoaEventOnChrome()
			if !inLiveResizeWatch() && over && !onChrome && platform.CocoaAppActive() {
				gx, gy := sdlhack.GlobalMousePos()
				a.app.mirrorPendingDown = true
				a.app.mirrorPendingPos = inViewportPos
				a.app.mirrorPendingGlobalX = gx
				a.app.mirrorPendingGlobalY = gy
				a.app.mirrorPendingFramesLeft = mirrorDeferFrames
				if dbg {
					fmt.Fprintf(os.Stderr,
						"[mirror] DOWN edge: SDL dropped, over=%v onChrome=%v -> PENDING (%d frames)\n",
						over, onChrome, mirrorDeferFrames)
				}
			} else if dbg {
				fmt.Fprintf(os.Stderr,
					"[mirror] DOWN edge: SDL dropped + over=%v onChrome=%v inLR=%v -> skip\n",
					over, onChrome, inLiveResizeWatch())
			}
		case !osDown && a.app.prevOsLeftDown:
			// OS-level release edge.
			if a.app.mirrorPendingDown {
				// Fast release before the defer window expired —
				// either a real-click-that-SDL-dropped (inject so
				// terminal sees a click) or a window-drag tap (no
				// inject; abort). Run the same abort checks here as
				// the deferred path below; otherwise a quick release
				// can inject DOWN+UP before the pending abort block
				// gets a chance to see cursor/window movement.
				if reason := abortPendingReason(); reason != "" {
					if dbg {
						fmt.Fprintf(os.Stderr, "[mirror] UP edge while pending: skip (%s)\n", reason)
					}
				} else if imguiSeesDown {
					imgui.CurrentIO().AddMouseButtonEvent(int32(imgui.MouseButtonLeft), false)
					if dbg {
						fmt.Fprintf(os.Stderr, "[mirror] UP edge while pending: SDL caught DOWN, inject UP\n")
					}
				} else {
					mp := imgui.MousePos()
					const noMouseSentinel = -3.4e+37
					if mp.X <= noMouseSentinel {
						imgui.CurrentIO().AddMousePosEvent(a.app.mirrorPendingPos.X, a.app.mirrorPendingPos.Y)
					}
					imgui.CurrentIO().AddMouseButtonEvent(int32(imgui.MouseButtonLeft), true)
					imgui.CurrentIO().AddMouseButtonEvent(int32(imgui.MouseButtonLeft), false)
					if dbg {
						fmt.Fprintf(os.Stderr, "[mirror] UP edge while pending: INJECT DOWN+UP\n")
					}
				}
				a.app.mirrorPendingDown = false
			} else if imguiSeesDown {
				// ImGui still thinks the button is down → SDL
				// dropped the UP. Inject so the drag/click clears.
				imgui.CurrentIO().AddMouseButtonEvent(int32(imgui.MouseButtonLeft), false)
				if dbg {
					fmt.Fprintf(os.Stderr, "[mirror] UP edge: SDL dropped UP -> INJECT\n")
				}
			} else if dbg {
				fmt.Fprintf(os.Stderr, "[mirror] UP edge: SDL already delivered -> skip\n")
			}
		}
		// Process the deferred DOWN. Abort conditions:
		//   1. Any window started moving → window-drag gesture.
		//   2. Cursor moved ≥mirrorAbortMotionPx → drag gesture
		//      (window drag manifests as cursor motion BEFORE the
		//      NSWindow.frame.origin updates, so this catches it
		//      earlier than the window-moved check).
		//   3. Cursor is now on chrome or outside our owned content.
		//   4. SDL caught up and delivered DOWN to ImGui → the
		//      mirror is no longer needed; injecting now would
		//      double-up the edge and trigger word-select via
		//      ImGui's double-click detector.
		if a.app.mirrorPendingDown {
			mp := imgui.MousePos()
			const noMouseSentinel = -3.4e+37

			if reason := abortPendingReason(); reason != "" {
				if dbg {
					fmt.Fprintf(os.Stderr, "[mirror]   pending DOWN aborted (%s)\n", reason)
				}
				a.app.mirrorPendingDown = false
			} else if imguiSeesDown {
				if dbg {
					fmt.Fprintf(os.Stderr, "[mirror]   pending DOWN aborted (SDL caught up)\n")
				}
				a.app.mirrorPendingDown = false
			} else {
				a.app.mirrorPendingFramesLeft--
				if a.app.mirrorPendingFramesLeft <= 0 {
					if mp.X <= noMouseSentinel {
						imgui.CurrentIO().AddMousePosEvent(a.app.mirrorPendingPos.X, a.app.mirrorPendingPos.Y)
					}
					imgui.CurrentIO().AddMouseButtonEvent(int32(imgui.MouseButtonLeft), true)
					a.app.mirrorPendingDown = false
					if dbg {
						fmt.Fprintf(os.Stderr, "[mirror]   pending DOWN: INJECT (timer expired)\n")
					}
				}
			}
		}
		a.app.prevOsLeftDown = osDown
	}

	// First frame: measure font metrics and create terminal
	if !a.ready {
		a.ready = true

		// Build the OS-backed glyph cache now that ImGui's NewFrame has
		// populated DisplayFramebufferScale. Doing this earlier (in
		// Run() before the loop starts) gives a stale fbScale of 1 on
		// Retina, so glyphs would rasterize at half the physical
		// pixel size and look chunky until the user changed font in
		// prefs (which rebuilds the cache when fbScale is correct).
		if a.renderer.Glyphs == nil && fontsys.Default != nil {
			primaryPath := renderer.ResolveFontPath(&a.app.cfg)
			if primaryPath != "" {
				fbScale := imgui.CurrentIO().DisplayFramebufferScale().X
				if fbScale <= 0 {
					fbScale = 1
				}
				if c, err := glyphcache.New(fontsys.Default, platform.Textures(), primaryPath, a.app.baseFontSize, fbScale); err == nil {
					a.renderer.Glyphs = c
					a.renderer.InvalidateCellCache()
				}
			}
		}

		// Measure real cell dimensions now that the font atlas is built.
		// metrics carries the float advance straight from the font;
		// baseCellW/H stores it pre-ceil so font-zoom can scale it
		// linearly, layout cellW/H is the ceil'd integer used for the
		// grid + OS resize-snap.
		metrics := a.measureCell()
		if metrics.Width < 1 || metrics.Height < 1 {
			// Fallback if measurement fails — estimate from atlas pixel size
			px := renderer.PixelSize(&a.app.cfg)
			metrics = renderer.CellMetrics{Width: px * 0.6, Height: px * 1.2}
		}
		a.app.baseCellW = metrics.Width
		a.app.baseCellH = metrics.Height
		a.cellW, a.cellH = ceilCell(metrics.Width, metrics.Height)
		a.renderer.Metrics = renderer.CellMetrics{Width: a.cellW, Height: a.cellH}
		// (Resize-increment push happens lower, at the end of
		// frame() — by then the SDL_Window handle is valid on
		// repeat frames; this first-frame call would otherwise
		// hit SDL_GetWindowFromID(0) and no-op.)

		// Re-fit the window to the configured columns/rows now that we have
		// real cell metrics. The initial CreateWindow used estimated metrics,
		// so the actual window may be a few pixels off in each direction.
		cfgCols, cfgRows := a.app.cfg.Window.Columns, a.app.cfg.Window.Rows
		if cfgCols < 2 {
			cfgCols = 80
		}
		if cfgRows < 2 {
			cfgRows = 24
		}
		// Skip the re-fit when reattach restored the previous
		// session's recorded geometry: those are real pixels from a
		// live window — tab bar included — not an estimate.
		// "Correcting" them to a bare cols×rows grid here is exactly
		// the reattach lost-row bug (the bar then reappears for the
		// adopted tabs and eats a row).
		if !a.restoredGeom {
			pad := float32(a.app.cfg.Appearance.Padding) * 2
			// Add cellSafetyMargin so gridSize() computes back to cfgCols/cfgRows.
			desiredW := int(math.Ceil(float64(float32(cfgCols)*a.cellW + pad + cellSafetyMarginH + cellOriginInsetX)))
			desiredH := int(math.Ceil(float64(float32(cfgRows)*a.cellH + pad + a.tabBarH + cellSafetyMarginV)))
			if desiredW != a.width || desiredH != a.height {
				// Every Window's OS geometry is driven by ImGui multi-
				// viewport. Set width/height + pendingResize so the
				// Begin in wrappedFrame uses CondAlways next frame.
				a.width = desiredW
				a.height = desiredH
				a.pendingResize = true
				a.skipDisplaySync = 2
			}
		}

		// First-frame startup tab — no parent to inherit CWD from;
		// the shell uses xerotty's own CWD (launcher / cwd-at-launch).
		//
		// In daemon mode, drain the adoptQueue first. The first
		// entry's tabs become this Window's tabs (restoring the
		// previous session). Any extra entries in the queue get
		// spawned as additional Windows by App after this returns
		// so the multi-window layout survives reattach.
		if a.app.daemonHub != nil && len(a.app.daemonAdoptQueue) > 0 {
			snap := a.app.daemonAdoptQueue[0]
			a.app.daemonAdoptQueue = a.app.daemonAdoptQueue[1:]
			a.daemonWindowID = snap.WindowID
			a.app.installSourceFactory(a)
			a.app.daemonHub.SetDefaultWindowID(snap.WindowID)
			var focusIdx = -1
			for i, ts := range snap.Tabs {
				src := a.app.daemonHub.Adopt(ts.ID, int(ts.Cols), int(ts.Rows))
				tab := a.tabs.AdoptTab(src)
				if ts.Title != "" {
					tab.SetTitle(ts.Title)
				}
				src.Resize(cfgCols, cfgRows)
				if ts.ID == snap.FocusedTabID {
					focusIdx = i
				}
			}
			if focusIdx >= 0 && focusIdx < len(a.tabs.Tabs) {
				a.tabs.ActiveIdx = focusIdx
				a.tabSwitchReq = a.tabs.Tabs[focusIdx].ID
			}
		} else if a.app.daemonHub != nil {
			// Daemon mode, no existing tabs — mint a fresh daemon
			// window for this first GUI window (ReqID-correlated).
			if id, err := a.app.daemonHub.CreateWindow(0, 0, int32(a.width), int32(a.height)); err == nil {
				a.daemonWindowID = id
				a.app.daemonHub.SetDefaultWindowID(a.daemonWindowID)
			}
			// spawnCmd (set before Run by a cold-start `-e`/`-x`) makes
			// this initial tab run the command; one-shot, cleared so
			// later tabs/windows get normal shells.
			if _, err := a.tabs.NewTabCmd(cfgCols, cfgRows, "", a.app.spawnCmd); err != nil {
				platform.Quit()
				return
			}
			a.app.spawnCmd = nil
		} else {
			if _, err := a.tabs.NewTabCmd(cfgCols, cfgRows, "", a.app.spawnCmd); err != nil {
				platform.Quit()
				return
			}
			a.app.spawnCmd = nil
		}
		// More daemon windows in the queue → spawn extra GUI
		// windows for them. spawnWindow uses spawnWindowImpl which
		// drains adoptQueue further.
		for len(a.app.daemonAdoptQueue) > 0 {
			a.app.spawnWindow()
		}
		return
	}

	// Font-face swap is handled in the backend's BeforeRender hook so
	// it happens BEFORE NewFrame, never mid-frame. Doing it mid-frame
	// (the way we used to) corrupts ImGui's per-frame state — at
	// NewFrame time it captured the OLD font pointer, then our user
	// code Clear()s the atlas and frees that font, then EndFrame /
	// Render dereferences the dangling pointer and asserts. Hook is
	// installed in Run() right after the backend is created.

	// Re-measure cell metrics after a font face swap (atlas was rebuilt).
	// Each Window does its own measurement against its own glyph cache
	// because Windows can be at different zoom levels — if this Window
	// is zoomed 2x, metrics.Width is 2x what baseCellW should be, so
	// divide by this Window's scale before stamping the app-wide base.
	if a.pendingRemeasure {
		a.pendingRemeasure = false
		if metrics := a.measureCell(); metrics.Width >= 1 && metrics.Height >= 1 {
			scale := float32(1)
			if a.app.baseFontSize > 0 && a.fontSize > 0 {
				scale = a.fontSize / a.app.baseFontSize
			}
			if scale > 0 {
				a.app.baseCellW = metrics.Width / scale
				a.app.baseCellH = metrics.Height / scale
			}
			a.cellW, a.cellH = ceilCell(metrics.Width, metrics.Height)
			a.renderer.Metrics = renderer.CellMetrics{Width: a.cellW, Height: a.cellH}
			a.renderer.FontSize = a.fontSize
			a.renderer.InvalidateCellCache()
			a.resizeTerminals()
			// (Resize-increment refresh handled at end of frame() —
			// same retry-until-handle-valid path used by first-frame
			// init.)
		}
	}

	// Sync window dimensions from ImGui IO every frame — more reliable than
	// SetSizeChangeCallback which some WMs/compositors don't always trigger.
	// Skip for a few frames after we issue SetWindowSize: DisplaySize lags the
	// WM by 1-2 frames, so a fresh shrink request would otherwise be clobbered
	// by the stale (pre-resize) DisplaySize value.
	if a.skipDisplaySync > 0 {
		a.skipDisplaySync--
	} else {
		// Pull size from this Window's viewport: main Window reads
		// io.DisplaySize (which is just MainViewport().Size()) — but
		// secondary Windows have their own popped-out viewport whose
		// Size() reflects the user's resizing of THAT OS window,
		// independent of the main.
		var dx, dy float32
		if vp := a.viewport(); vp != nil {
			sz := vp.Size()
			dx, dy = sz.X, sz.Y
		}
		if int(dx) > 0 && int(dy) > 0 {
			newW, newH := int(dx), int(dy)
			if newW != a.width || newH != a.height {
				a.width = newW
				a.height = newH
				a.resizeTerminals()
				a.resizeTime = imgui.Time()
				a.resizeOverlay = true
				a.resizeOverlayText = "" // drag-resize: live cols×rows
			}
		}
	}

	// Resize reconciliation — level-triggered, not edge-triggered.
	// Everything above only resizes terminals on CHANGE events, and
	// missed edges have produced repeated "stuck at the wrong size
	// until the user wiggles the window" bugs: stale geometry at
	// reattach-adopt time, and resize requests racing daemon attach
	// and getting dropped (mac report: tab stuck ~80x24 on resume
	// until a manual resize "kicked" it). Enforce the invariant
	// directly instead: any tab whose ACTUAL grid differs from this
	// window's desired grid gets a (re-)request. Daemon resizes are
	// async — the shadow grid reports the old size until the daemon's
	// frames land — so requests are deduped per target size and
	// retried at most every half second while the mismatch persists.
	// Matched tabs cost two int compares per frame; a lost request
	// now self-heals in ≤0.5s instead of waiting for a human.
	if a.ready {
		cols, rows := a.gridSize()
		if cols > 2 && rows > 2 {
			now := imgui.Time()
			for _, tab := range a.tabs.Tabs {
				if tab.Closed || tab.Terminal == nil {
					continue
				}
				if tab.Terminal.Width() == cols && tab.Terminal.Height() == rows {
					delete(a.resizeReq, tab.ID)
					continue
				}
				if req, ok := a.resizeReq[tab.ID]; ok &&
					req.cols == cols && req.rows == rows && now-req.at < 0.5 {
					continue // request in flight — give it time to land
				}
				tab.Terminal.Resize(cols, rows)
				if a.resizeReq == nil {
					a.resizeReq = make(map[int]resizeRequest)
				}
				a.resizeReq[tab.ID] = resizeRequest{cols: cols, rows: rows, at: now}
			}
		}
	}

	// Handle scroll wheel: tab bar = switch tabs, Ctrl+scroll = zoom, plain scroll = scrollback
	// io.MouseWheel is global, so EVERY Window's frame() sees the same
	// delta — gate to the Window the cursor is actually over or the
	// wheel scrolls all Windows at once. The contentOrigin rect test
	// can't do this on Wayland (every viewport reports Pos (0,0) and
	// io.MousePos is surface-local), so use the OS pointer-focus
	// (MouseFocusWindowID), same as the context-menu open gate.
	myWheelWinID := uintptr(0)
	if vp := a.viewport(); vp != nil {
		myWheelWinID = vp.PlatformHandle()
	}
	wheelInThisWindow := myWheelWinID != 0 && platform.MouseFocusWindowID() == myWheelWinID
	wheel := imgui.CurrentIO().MouseWheel()
	if wheel != 0 && wheelInThisWindow {
		vpOffY := a.contentOriginY
		if a.tabBarH > 0 && imgui.MousePos().Y-vpOffY < a.tabBarH {
			// Mouse over tab bar — switch tabs
			if wheel > 0 {
				a.tabs.Prev()
			} else {
				a.tabs.Next()
			}
			if tab := a.tabs.Active(); tab != nil {
				a.tabSwitchReq = tab.ID
			}
		} else if imgui.IsKeyDown(imgui.ModCtrl) {
			// Ctrl+wheel zoom — per-window, mirrors Cmd+= / Cmd+-.
			if wheel > 0 {
				a.fontSize += 1
				a.updateFontMetrics()
			} else if a.fontSize > 6 {
				a.fontSize -= 1
				a.updateFontMetrics()
			}
		} else if tab := a.tabs.Active(); tab != nil {
			scrollLines := a.app.cfg.Scrollback.ScrollSpeed
			if scrollLines <= 0 {
				scrollLines = 3
			}
			if tab.Terminal.IsAltScreen() {
				// Alternate-scroll: full-screen apps (mutt, less, vim)
				// own the alt screen and have no terminal scrollback, so
				// the wheel must drive ARROW KEYS into the app instead of
				// our (empty) scrollback — exactly xterm's alternateScroll.
				// Without this, scrolling in mutt did nothing. DECCKM picks
				// the application vs normal cursor-key encoding.
				seq := altScrollSeq(wheel > 0, tab.Terminal.AppCursorMode())
				for i := 0; i < scrollLines; i++ {
					_, _ = tab.Terminal.Write(seq)
				}
			} else {
				s := a.getScroll(tab.ID)
				if wheel > 0 {
					s.ScrollUp(scrollLines, tab.Terminal.ScrollbackLen())
				} else {
					s.ScrollDown(scrollLines)
				}
			}
		}
	}

	// Mouse-selection and link hover are GEOMETRICALLY scoped — they
	// only fire when the cursor's actual position lands in this
	// Window's content rect. handleMouseSelection has its own
	// inTerminal check (computed from MousePos vs contentOrigin /
	// width / height) and link detection bails when (col, row) is
	// outside (cols, rows). No focus-based gate needed; in fact
	// gating on a.app.active broke selection entirely because
	// IsWindowFocusedV is unreliable under multi-viewport on
	// Wayland — wrapper windows often never register as focused,
	// so a.app.active never matched and selection / middle-click /
	// right-click all silently no-op'd.
	a.handleMouseSelection()

	// Detect links under mouse cursor. Gate the whole pass on
	// Links.Enabled — when off, hoveredLink stays nil so the underline
	// decoration in the renderer disappears and the click handlers
	// below are no-ops by virtue of `hoveredLink != nil` checks.
	a.hoveredLink = nil
	if a.app.cfg.Links.Enabled {
		if tab := a.tabs.Active(); tab != nil {
			mousePos := imgui.MousePos()
			col := int((mousePos.X - a.renderer.OffsetX) / a.cellW)
			row := int((mousePos.Y - a.renderer.OffsetY) / a.cellH)
			cols, rows := a.gridSize()
			if col >= 0 && col < cols && row >= 0 && row < rows {
				scrollOff := 0
				if s, ok := a.scroll[tab.ID]; ok {
					scrollOff = s.Offset
				}
				a.hoveredLink = detectLinkAt(tab.Terminal, col, row, scrollOff)

				// Ctrl+click opens link
				if a.hoveredLink != nil && a.app.cfg.Links.CtrlClick && imgui.IsKeyDown(imgui.ModCtrl) && imgui.IsMouseClickedBool(imgui.MouseButtonLeft) {
					openURL(a.hoveredLink.URL, a.app.cfg.Links.Opener)
				}
			}
		}
	}

	// Drain data notifications
	a.tabs.DrainData()

	// Scroll handling on new output:
	// - If at live position: stay at bottom (auto-scroll)
	// - If scrolled back:
	//     scroll_on_output=true  → snap to bottom (gnome-terminal default)
	//     scroll_on_output=false → freeze viewport by adjusting offset
	//                              for new scrollback lines (xterm/iTerm default)
	scrollOnOutput := a.app.cfg.Scrollback.ScrollOnOutput
	for _, tab := range a.tabs.Tabs {
		if tab.Dirty {
			tab.Dirty = false
			s := a.getScroll(tab.ID)
			sbLen := tab.Terminal.ScrollbackLen()
			if s.IsScrolled() && s.PrevSBLen > 0 {
				if scrollOnOutput {
					s.Reset()
				} else {
					delta := sbLen - s.PrevSBLen
					if delta > 0 {
						s.Offset += delta
					}
				}
			}
			// Clamp: scrollback can SHRINK below the current offset —
			// a clear, an alt-screen switch, or a resize, all common
			// during a big output burst. Without this the offset
			// strands the viewport off the bottom showing blank rows
			// (contentIdx goes negative) until the user manually
			// scrolls — the "won't stay scrolled to the bottom" bug. A
			// shrink to 0 (full clear) pins offset to 0 = live bottom.
			if s.Offset > sbLen {
				s.Offset = sbLen
			}
			s.PrevSBLen = sbLen
		}
	}

	// Check for closed tabs and handle on_child_exit policy
	a.tabs.CheckClosed()
	// Track WHY tabs closed this frame. A daemon-side VANISH splits two
	// ways that the Count()==0 block treats very differently: a daemon
	// RESTART (process died + came back, took the shells) must keep the
	// window alive and reseat, whereas a remote close (another client /
	// MCP agent closed the last tab on a still-LIVE daemon) is a
	// legitimate close. Both still force-remove the dead tab here.
	daemonRestartVanished := false
	otherClose := false
	for i := len(a.tabs.Tabs) - 1; i >= 0; i-- {
		tab := a.tabs.Tabs[i]
		if !tab.Closed {
			continue
		}
		// A daemon tab that vanished from the topology (closed by
		// another client / MCP agent, or lost when the daemon process
		// restarted) is GONE — there's no local child to "hold", so
		// force-remove it regardless of on_child_exit. Only a
		// restart-vanish counts toward the reseat decision below; a
		// remote close leaves daemonRestartVanished false so the window
		// closes normally.
		if ds, ok := tab.Terminal.(*daemonsource.Source); ok && ds.IsVanished() {
			if ds.VanishedByRestart() {
				daemonRestartVanished = true
			}
			a.tabs.CloseTab(i)
			continue
		}
		switch a.app.cfg.Tabs.OnChildExit {
		case "close":
			a.tabs.CloseTab(i)
			otherClose = true
		case "hold":
			// Keep tab open — user can close manually
		case "hold_on_error":
			if tab.Terminal.ChildExitCode() == 0 {
				a.tabs.CloseTab(i)
				otherClose = true
			}
			// Non-zero exit: keep tab open so user can see output
		default:
			a.tabs.CloseTab(i)
			otherClose = true
		}
	}

	// No tabs left in this Window. Normally that closes the Window (and,
	// if it's the last one, quits the app). But if the window emptied
	// ONLY because the daemon PROCESS restarted and took its shells with
	// it (not the user closing them, and not another client closing the
	// last tab on a still-live daemon), closing the last window would
	// call platform.Quit() and silently exit the whole GUI. A daemon is
	// local infrastructure, not a quit request, so instead keep the
	// Window alive and reseat a fresh tab on the reconnected daemon ("the
	// server came back, here's a new shell"). The persistent flag retries
	// across frames while the hub is still mid-redial; a genuine
	// user/child-exit/remote-close empty still closes.
	if a.tabs.Count() == 0 && (a.daemonReseatPending || (daemonRestartVanished && !otherClose)) && a.app.daemonHub != nil {
		a.daemonReseatPending = true
		cols, rows := a.gridSize()
		if cols < 2 || rows < 2 {
			cols, rows = 80, 24
		}
		// The old daemonWindowID belonged to the dead daemon; mint a
		// fresh daemon window for this GUI window before adding the tab
		// (mirrors first-frame init). Do this AT MOST ONCE per episode:
		// CreateWindow is a synchronous hub RPC, and re-minting every
		// retry frame (when NewTab fails after CreateWindow succeeds)
		// leaked a daemon window per tick. setLocalDaemonWindowID keeps
		// the legacy field, the per-hub map, and the hub default window
		// all consistent so later focus/move use the NEW window ID.
		//
		// The mint-once is scoped to the CURRENT daemon instance: if a
		// SECOND restart happens mid-reseat, the previously-minted window
		// ID belongs to the dead intermediate daemon, so reseatNeedsMint
		// forces a re-mint on the new one (see its doc).
		curInstance := a.app.daemonHub.InstanceID()
		if reseatNeedsMint(a.daemonReseatMinted, a.daemonReseatInstance, curInstance) {
			if id, err := a.app.daemonHub.CreateWindow(0, 0, int32(a.width), int32(a.height)); err == nil {
				a.setLocalDaemonWindowID(id)
				a.daemonReseatMinted = true
				a.daemonReseatInstance = curInstance
			}
		}
		// Only retry the cheap part (NewTab) each frame — but ONLY once we
		// actually hold a window minted for the CURRENT daemon instance.
		// If CreateWindow failed this frame (daemon mid-redial), the mint
		// is either absent (first episode) or stale (a second restart left
		// daemonReseatInstance pointing at the dead intermediate daemon).
		// Running NewTab with that stale daemonWindowID would silently land
		// the tab in the daemon's DEFAULT window (session.go falls back for
		// unknown IDs). So skip NewTab and retry next frame — the window
		// stays open+empty with daemonReseatPending set, never pendingClose.
		if a.daemonReseatMinted && a.daemonReseatInstance == curInstance {
			if _, err := a.tabs.NewTab(cols, rows, ""); err == nil {
				// Got a tab; episode over — resume normal rendering.
				a.daemonReseatPending = false
				a.daemonReseatMinted = false
				a.daemonReseatInstance = ""
			}
		}
		if a.tabs.Count() == 0 {
			// Hub still mid-reconnect — render an empty frame and retry
			// next frame. Crucially do NOT set pendingClose.
			imgui.CurrentIO().ClearInputKeys()
			return
		}
		// Fell through with a fresh tab; render it this frame.
	} else if a.tabs.Count() == 0 {
		// The reap pass in wrappedFrame removes this Window; if it was
		// the last Window the process exits.
		a.pendingClose = true
		// Clear ImGui's input key state so any key the user was
		// holding at close time (Ctrl-D / Enter / etc. — i.e. the
		// keystroke that caused the last tab to exit) doesn't stay
		// stuck "down" in ImGui's state. SDL3 drops KEYUP events
		// during the Cocoa focus shift that closing a window
		// triggers; without this clear, focus transfers to the
		// surviving Window with the key still registered as held,
		// and ImGui's auto-repeat fires it again and again — which
		// is how closing one window cascades into closing them all.
		imgui.CurrentIO().ClearInputKeys()
		return
	}

	// Process queued key events — only the active Window forwards
	// keystrokes to its PTY, so typing doesn't end up in N PTYs at
	// once.
	if a == a.app.active {
		a.processKeys()
	}

	// Update tab bar height based on tab count and current font size.
	// FrameHeight = FontSize + FramePadding.Y*2 — the visible tab item
	// rect. +2 covers the underline separator drawn just inside Max.y.
	// With inline tab bar, this is the EXACT bar height — no separate
	// window with extra internal padding.
	oldTabBarH := a.tabBarH
	if a.tabs.Count() > 1 {
		a.tabBarH = imgui.FrameHeight() + 2
	} else {
		a.tabBarH = 0
	}
	pad := float32(a.app.cfg.Appearance.Padding)
	// Use the wrapper's actual content origin (captured in
	// wrappedFrame). With inline tab bar, the BarRect ends at
	// contentOrigin.Y + FrameHeight; cells render `pad` px below
	// that, naturally tight against the visible tab bar.
	vpOffX, vpOffY := a.contentOriginX, a.contentOriginY
	// Snap render offsets to whole pixels so glyphs don't get sub-pixel-drifted
	// between rows. Without this, half-block characters (▀ ▄) and continuous
	// lines (─ │) can develop visible gaps between rows because their AA
	// rendering shifts at fractional positions.
	a.renderer.OffsetX = float32(math.Floor(float64(vpOffX + pad + cellOriginInsetX)))
	a.renderer.OffsetY = float32(math.Floor(float64(vpOffY + a.tabBarH + pad)))
	// When tab bar visibility changes (1↔2 tabs), grow/shrink the SDL window
	// vertically by the tab bar's height so the terminal grid keeps the same
	// rows. Without this, gridSize() loses ~tabBarH/cellH rows and the user
	// sees the terminal shrink (e.g. 80x24 → 80x22) instead of the window
	// expanding to accommodate the bar. Skip in fullscreen — the WM ignores
	// SetWindowSize there and we can't grow past the display.
	if a.tabBarH != oldTabBarH {
		if a.restoredWithBar && oldTabBarH == 0 {
			// Reattach-restored height already includes the bar (it
			// is the live detach-time size of a multi-tab window), so
			// the bar's first appearance after adoption must not grow
			// the window again — that double-count compounded by one
			// bar height per restart. One-shot: real 1↔2 transitions
			// after this compensate as usual.
			a.restoredWithBar = false
		} else if !a.fullscreen {
			delta := int(math.Ceil(float64(a.tabBarH - oldTabBarH)))
			if delta != 0 {
				newH := a.height + delta
				a.height = newH
				a.pendingResize = true
				a.skipDisplaySync = 2
			}
		}
		a.resizeTerminals()
	}

	// Occluded windows (fully covered / hidden — see
	// platform.WindowOccluded) skip ALL visual work: glow quads,
	// the cell-layer Draw (including rebuilds while a hidden tab
	// streams output), cursor, scrollbar. Their viewport render +
	// swap is also skipped C-side. The EXPOSED event wakes the loop
	// and the next frame repaints with current emulator content, so
	// nothing on screen is ever stale. With a dozen stacked terminal
	// windows this is the difference between paying for 14 and
	// paying for the ones actually visible.
	occluded := false
	if h := a.sdlWindowHandle(); h != 0 {
		occluded = platform.WindowOccluded(h)
		// Damage: only windows marked dirty (or addressed by an SDL
		// event) render + swap this frame. The glow animates every
		// visible window, so lava-on marks unconditionally.
		//
		if !occluded {
			// Content is NEVER throttled: a tab's streaming output
			// repaints its window the moment it changes (and only
			// that window — that's the damage win). Only the
			// decorative glow coalesces in background windows:
			// glow.background_fps caps the lamp's animation rate for
			// windows that are neither focused nor hovered, so a
			// dozen idle lava windows stop costing full tick rate.
			dirty := a.windowVisuallyDirty()
			if !dirty && a.app.cfg.Appearance.Glow.Enabled {
				dirty = true
				if bgFPS := a.app.cfg.Appearance.Glow.BackgroundFPS; bgFPS > 0 &&
					!a.hasOSFocus() && platform.MouseFocusWindowID() != h {
					now := imgui.Time()
					if now-a.lastBgMark < 1.0/float64(bgFPS) {
						dirty = false
					} else {
						a.lastBgMark = now
					}
				}
			}
			if dirty {
				platform.MarkViewportDirty(h)
			}
		}
	}

	// Lava-lamp glow layer: under everything (cells, tab bar). The
	// renderer skips default-background cells, so the blobs show
	// through all "empty" terminal area while colored runs float on
	// top. Drawn from the same content origin the cells use so it
	// tracks the window in multi-viewport space.
	if a.app.cfg.Appearance.Glow.Enabled && !occluded {
		drawGlow(a.bgDrawList(), a.contentOriginX, a.contentOriginY,
			float32(a.width), float32(a.height), &a.app.theme, &a.app.cfg.Appearance.Glow)
	}

	// Render terminal cells FIRST into the wrapper's window drawlist,
	// then the tab bar items after. ImGui adds draw commands in call
	// order so the later commands (tab bar items, decorations) layer
	// on top of the earlier ones (terminal cells). Without this
	// ordering the cells would draw over the tab bar's bottom edge
	// and clip the first terminal row when both share a drawlist.
	if tab := a.tabs.Active(); tab != nil && !occluded {
		drawList := a.bgDrawList()
		if drawList != nil {
			scrollOff := 0
			if s, ok := a.scroll[tab.ID]; ok {
				scrollOff = s.Offset
			}
			// Sliding-window scrollback (daemon tabs in unlimited
			// mode): make sure the cached window covers the scrollback
			// rows about to render; fetch from the daemon otherwise.
			// No-op for in-process tabs and non-windowed Sources.
			if win, ok := tab.Terminal.(scrollbackWindower); ok && scrollOff > 0 {
				_, visRows := a.gridSize()
				sbLen := tab.Terminal.ScrollbackLen()
				from := sbLen - scrollOff
				win.EnsureScrollbackWindow(from, from+visRows)
			}
			// Feed the selection into the renderer so selected cells
			// draw with SelectionFg/SelectionBg inline (text stays
			// visible) instead of an opaque rect painted over the
			// glyphs, which buried the selected text.
			if a.sel.active {
				r1, c1, r2, c2 := a.sel.normalize()
				cols, _ := a.gridSize()
				a.renderer.SetSelection(true, r1, c1, r2, c2, cols)
			} else {
				a.renderer.SetSelection(false, 0, 0, 0, 0, 0)
			}
			a.renderer.Draw(tab.Terminal, drawList, scrollOff)

			// Draw link underline on hover
			if a.hoveredLink != nil {
				lh := a.hoveredLink
				y := a.renderer.OffsetY + float32(lh.Row)*a.cellH + a.cellH - 1
				x1 := a.renderer.OffsetX + float32(lh.StartCol)*a.cellW
				x2 := a.renderer.OffsetX + float32(lh.EndCol+1)*a.cellW
				drawList.AddLine(
					imgui.Vec2{X: x1, Y: y},
					imgui.Vec2{X: x2, Y: y},
					a.renderer.Theme.Foreground,
				)
			}

			// Only show cursor when at live position (not scrolled
			// back) AND the terminal hasn't hidden it (DECTCEM).
			if scrollOff == 0 && tab.Terminal.CursorVisible() {
				// Cursor SHAPE: when the foreground app explicitly set
				// the cursor via DECSCUSR (styleSet), honor it; else
				// use the user's config preference. Same for blink.
				styleStr := a.app.cfg.Appearance.CursorStyle
				cfgBlink := a.app.cfg.Appearance.CursorBlink
				if ts, tblink, styleSet := tab.Terminal.CursorStyle(); styleSet {
					styleStr = cursorStyleName(ts)
					cfgBlink = tblink
				}
				showCursor := true
				if cfgBlink {
					rate := float64(a.app.cfg.Appearance.BlinkRate) / 1000.0
					if rate <= 0 {
						rate = 0.53
					}
					now := imgui.Time()
					showCursor = int(now/rate)%2 == 0
					// This (active-tab) cursor is blinking, so the render
					// loop must wake to toggle it. Lower the idle wait to
					// the next toggle — at most 2 renders/blink-period
					// instead of the old full frame cap.
					nextToggle := float64(int(now/rate)+1) * rate
					// Clamp to a 1ms floor: the interval is always
					// positive, so a sub-millisecond remainder must NOT
					// round down to 0 and get ignored — otherwise the
					// loop falls back to the 1000ms safety net and the
					// cursor stays un-toggled for up to a second.
					ms := int((nextToggle - now) * 1000)
					if ms < 1 {
						ms = 1
					}
					if ms < a.app.idleWakeMs {
						a.app.idleWakeMs = ms
					}
				}
				if showCursor {
					pos := tab.Terminal.Emulator().CursorPosition()
					a.renderer.DrawCursor(struct{ X, Y int }{pos.X, pos.Y},
						styleStr, drawList)
				}
			}

			// Search highlights. EnsureSearch re-scans only when the
			// query/options/content changed (RenderGeneration), on a
			// background goroutine — the old design re-scanned the
			// ENTIRE scrollback synchronously EVERY FRAME, which on
			// large disk-backed histories froze the UI outright.
			// MatchIdx preservation across content refreshes lives in
			// EnsureSearch.
			if s, ok := a.scroll[tab.ID]; ok && s.Searching && s.Query != "" {
				_, visRows := a.gridSize()
				if s.OnAsyncDone == nil {
					s.OnAsyncDone = platform.PostWake
				}
				// Windowed daemon tabs can't scan deep history locally
				// (they only mirror a window) — search runs on the
				// daemon. In-process and non-windowed tabs scan locally.
				if searcher, ok := tab.Terminal.(scrollbackSearcher); ok {
					a.ensureDaemonSearch(tab.ID, s, tab.Terminal, searcher, visRows)
				} else {
					s.EnsureSearch(tab.Terminal, visRows, tab.Terminal.RenderGeneration())
				}
				if a.searchJumpOnResult && !s.SearchPending {
					a.searchJumpOnResult = false
					s.ScrollToCurrentMatch(visRows)
				}
				if len(s.Matches) > 0 {
					a.drawSearchHighlights(s, drawList)
				}
			} else if a.daemonSearch != nil {
				// Search overlay closed for this tab — drop its daemon
				// search state so reopening with the same query re-runs.
				delete(a.daemonSearch, tab.ID)
			}

			// Scrollbar
			a.drawScrollbar(tab, scrollOff, drawList)
		}
	}

	// Tab bar — rendered AFTER terminal cells so it visually layers
	// on top of them when they share the wrapper's drawlist.
	a.renderTabBar()

	// Propose-mode approval gate (daemon mode only; no-op when
	// the queue is empty).
	a.renderProposalGate()

	// Search overlay
	a.renderSearchOverlay()

	// Context menu — open on right-click. We detect the click manually because
	// the terminal isn't an ImGui window (it renders into the main viewport's
	// BackgroundDrawList), so HoveredWindow is nil over the terminal area.
	//
	// That same lack-of-window breaks hover-while-held: when ImGui sees a
	// mouse-down whose HoveredWindow is nil and no popup is open yet, it
	// stamps MouseDownOwned[btn]=false for the click, which then forces
	// HoveredWindow back to nil every subsequent frame the button is held.
	// Items in the popup never highlight on hover, and at EndFrame the
	// MouseClicked[1] && HoveredId == 0 path runs ClosePopupsOverWindow and
	// shuts the menu we just opened.
	//
	// Reclaim ownership right after OpenPopup: flip MouseDownOwned to true
	// so the next frame keeps HoveredWindow tracking the popup, and pin a
	// non-zero HoveredID for this frame so the right-click-on-empty-space
	// close path skips us. Result: terminal-style hover preview while the
	// right button is held; a separate click still confirms.
	// Gate to the active Window: ImGui's IsMouseClicked / OpenPopup are
	// global, so without this every Window's frame() would see the click,
	// each call OpenPopupStr("##contextmenu") with the same ID (last write
	// wins), and the wrong Window's renderContextMenu would receive the
	// chosen action. Result: right-click "New Tab" in Window 1 actually
	// created a tab in the last-iterated Window. Same reason processKeys
	// gates on a.app.active.
	// Geometric scope: only the Window whose content rect contains the
	// cursor opens its context menu. (Focus-based gating fails on
	// Wayland multi-viewport — wrappers often don't register as
	// focused. Mouse-position is unambiguous: exactly one Window
	// contains the cursor at any moment.)
	//
	// We don't use ImGui's BeginPopup / OpenPopupStr machinery
	// because that ties the menu lifecycle to the host viewport's
	// focus — once the cursor crosses the terminal edge ImGui
	// considers the popup "closed by click-out" and snaps it shut,
	// even though the user is just moving toward the menu. Instead
	// just record the position; renderContextMenu draws via BeginV
	// and we control when it closes.
	mp := imgui.MousePos()
	// Which Window the cursor is physically over, by OS-level pointer
	// focus. This is the authoritative discriminator on Wayland, where
	// every viewport reports Pos (0,0) and io.MousePos is surface-local
	// — so a contentOrigin-based rect test is true for EVERY Window and
	// ImGui's focus flag lags the compositor (verified: a right-click
	// opened the menu on whichever Window ImGui happened to mark
	// focused, not the one clicked). MouseFocusWindowID goes through OS
	// pointer-focus and is reliable on X11/Wayland/Cocoa.
	myWinID := uintptr(0)
	if vp := a.viewport(); vp != nil {
		myWinID = vp.PlatformHandle()
	}
	mouseOverThisWindow := myWinID != 0 && platform.MouseFocusWindowID() == myWinID
	// Only OPEN the menu via right-click; never re-open or reposition
	// while it's already open. If the user right-clicks the terminal
	// with the menu showing, the click counts as a click-outside and
	// dismisses the menu via menu.Render's close-on-click path. If we
	// re-set contextMenuOpenedFrame here, allowCloseClick would be
	// false for that frame and the menu would silently reposition
	// instead of closing.
	rightClicked := imgui.IsMouseClickedBool(imgui.MouseButtonRight)
	// Open the menu only on the Window the cursor is over — exactly
	// where the user right-clicked. Only one Window can be under the
	// pointer at a time, so a click that dismisses one Window's menu
	// can never reopen a menu on a different Window.
	menuBusyThisFrame := int(imgui.FrameCount()) == a.app.menuActivityFrame
	if mouseOverThisWindow && rightClicked && !a.contextMenuOpen && !menuBusyThisFrame {
		a.contextMenuOpen = true
		a.app.menuActivityFrame = int(imgui.FrameCount())
		a.contextMenuX = mp.X
		a.contextMenuY = mp.Y
		// Remember which frame we opened on — renderContextMenu skips
		// its close-on-click-outside check for that frame so the very
		// right-click that opened the menu doesn't immediately close
		// it (IsMouseClickedBool stays true for the rest of this
		// frame).
		a.contextMenuOpenedFrame = int(imgui.FrameCount())
		// (Previously called SDL_CaptureMouse here for global click
		// delivery, but under sdl2-compat → SDL3 it's a silent no-op
		// on X11 anyway and observed under VNC to interfere with
		// MenuItem's hover-based activation. Click-inside-xerotty +
		// Escape are the only reliable dismiss paths until SDL3's
		// proper popup window primitive is wired in.)
	}
	a.renderContextMenu()

	// Unsafe paste confirmation dialog
	a.renderPasteDialog()

	// Tab rename dialog
	a.renderRenameDialog()

	// "Connect to host…" ad-hoc remote dialog
	a.renderConnectDialog()

	// Preferences dialog
	a.renderPreferences()

	// Resize overlay
	a.renderResizeOverlay()

	// "Reconnecting…" badge when the active tab's daemon connection is
	// down and re-dialing (layer 4b). The last frame stays frozen
	// underneath; this just signals it's stalled, not dead.
	a.renderReconnectingOverlay()

	// Push cell-width resize increments to the OS window each time
	// the cell dimensions change (font zoom, metric refresh) AND
	// once at startup when this Window's real popped-out SDL_Window
	// replaces the hidden carrier/main viewport handle. Retry when
	// either the handle or the cell size changes, but only cache the
	// state after the native call succeeds.
	cellW := int(a.cellW)
	cellH := int(a.cellH)
	if cellW > 0 && cellH > 0 {
		if h := a.sdlWindowHandle(); h != 0 &&
			(h != a.resizeIncrementSetWindow ||
				cellW != a.resizeIncrementSetCellW ||
				cellH != a.resizeIncrementSetCellH) {
			if platform.SetResizeIncrements(h, cellW, cellH) {
				a.resizeIncrementSetWindow = h
				a.resizeIncrementSetCellW = cellW
				a.resizeIncrementSetCellH = cellH
			}
		}
	}

	// Apply the embedded app icon to this Window's native SDL_Window
	// once it exists. Linux WM uses it for taskbar / Alt-Tab; macOS
	// reads from the .icns in the bundle (this is a no-op there).
	// Same retry-until-handle-valid story as SetResizeIncrements
	// above — first frame has no handle yet, subsequent frames do.
	if !a.iconApplied {
		if h := a.sdlWindowHandle(); h != 0 {
			applyWindowIcon(h)
			a.iconApplied = true
		}
	}
}

func (w *Window) isSearching() bool {
	if tab := w.tabs.Active(); tab != nil {
		if s, ok := w.scroll[tab.ID]; ok {
			return s.Searching
		}
	}
	return false
}

func (w *Window) popupActive() bool {
	// Note: prefDialog is a non-modal window. It manages its own focus
	// through ImGui's WantCaptureKeyboard, so it shouldn't gate terminal input.
	return w.renamingTab || w.pendingPaste != "" || w.connectingHost
}

// inputOwnedByDialog reports whether a preferences dialog on ANY window
// currently holds focus. processKeys runs only for a.active and the old
// gate checked just that window's prefDialog — so with prefs focused on
// Window A but Window B active, B still drained the global character
// queue into its PTY (the combo type-ahead / label keystrokes leaked
// behind the dialog). Scanning every window closes that multi-window
// gap. The focus check (not just .open) avoids over-gating: if prefs is
// open but the user clicked back to a terminal, the dialog isn't focused
// and terminal input flows normally.
func (a *App) inputOwnedByDialog() bool {
	for _, w := range a.windows {
		if w.prefDialog.open && w.prefDialog.focused {
			return true
		}
	}
	return false
}

func (w *Window) processKeys() {
	// Only the focused Window consumes keyboard input. ImGui's IsKeyPressed
	// is global, so without this gate every Window's frame() would see the
	// same keybind event and dispatchAction would fire in every Window —
	// e.g. Cmd+T would create a new tab in every open Window at once
	// instead of just the one the user is typing into.
	if w != w.app.active {
		return
	}
	tab := w.tabs.Active()
	searching := w.isSearching()
	searchInputFocused := searching && w.searchInputFocused
	popupOpen := w.popupActive()

	// Modal popups (rename, unsafe paste) eat all input.
	if popupOpen {
		return
	}

	// A prefs dialog on ANY window owns the keyboard — its InputText
	// label fields and the Add combo's type-ahead read ImGui's char
	// queue. Forwarding to the PTY here would leak every typed char into
	// the shell behind the dialog (and steal the combo's letters). Gate
	// at the app level (not just this active window's prefDialog) so the
	// multi-window case — prefs focused on Window A while Window B is
	// active — doesn't leak A's keystrokes into B's PTY. This single
	// early-return covers BOTH the translated-key path and the
	// character-queue path below. NOT gated on WantCaptureKeyboard: with
	// NavEnableKeyboard set (sdl3.cpp) that's true even on plain terminal
	// focus, which would suppress normal typing. The prefs window's own
	// EnsureTextInput keeps feeding chars to ImGui, so type-ahead works.
	if w.app.inputOwnedByDialog() {
		return
	}

	// Yield to ImGui only when a text-entry widget is actually wanting chars
	// (prefs InputText, etc). WantCaptureKeyboard is too broad — it also flips
	// true when a non-text window has plain focus, so e.g. clicking a tab or
	// recovering focus after SetWindowSize would silently swallow PTY input
	// even though nothing on screen needs the keys.
	if imgui.CurrentIO().WantTextInput() && !searchInputFocused {
		return
	}

	// Past the gates, the terminal owns keyboard input this frame —
	// re-assert SDL text input on this Window so typed characters keep
	// flowing. A dialog's InputText (rename/connect/search) closing
	// makes the ImGui backend SDL_StopTextInput this very window,
	// which would otherwise leave the terminal able to see mapped keys
	// (Enter, arrows) but not typed characters. No-op when already on.
	if vp := w.viewport(); vp != nil {
		platform.EnsureTextInput(vp.PlatformHandle())
	}

	// Poll ImGui key state (SDL backend's SetKeyCallback is not implemented).
	// Pass the active tab's DECCKM state so arrow keys render as `ESC O X`
	// in pagers (less, git diff) and other apps that request application
	// cursor mode.
	appCursor := false
	if tab != nil && tab.Terminal != nil {
		appCursor = tab.Terminal.AppCursorMode()
	}
	events := input.PollKeys(w.app.cfg.Keybinds, appCursor, input.KeyOptions{
		Backspace:  w.app.cfg.Keys.Backspace,
		Delete:     w.app.cfg.Keys.Delete,
		ShiftEnter: w.app.cfg.Keys.ShiftEnter,
		HomeEnd:    w.app.cfg.Keys.HomeEnd,
	})
	actionDispatched := false

	for _, ev := range events {
		// Escape closes search whenever search is open, even if the
		// InputText hasn't visibly grabbed focus yet — searchInputFocused
		// lags by a frame on re-open because IsItemFocused() is set
		// after the InputText is submitted, which happens AFTER
		// processKeys runs in the same frame. Gating on `searching`
		// (the synchronous flag from OpenSearch) makes the first
		// Escape on a re-opened search always close it.
		if searching && tab != nil && ev.Action == "" && len(ev.Bytes) == 1 && ev.Bytes[0] == 0x1b {
			s := w.getScroll(tab.ID)
			s.CloseSearch()
			// Inject a synthetic Escape-UP into ImGui's IO queue.
			// SDL can drop the real KEYUP during the Cocoa focus
			// shift triggered by InputText deactivating — leaving
			// ImGui with KeyEscape stuck "down" even though the
			// user released it. The next time the user presses
			// Escape (to close a re-opened search), ImGui sees no
			// edge transition (because it's "already down") and our
			// PollKeys returns no event. Force the up edge now so
			// the next real press is a clean transition.
			imgui.CurrentIO().AddKeyEvent(imgui.KeyEscape, false)
			searching = false
			searchInputFocused = false
			w.searchInputFocused = false
			continue
		}
		// Enter / Shift+Enter for next/prev match — these still gate
		// on searchInputFocused because if the user clicked the
		// terminal mid-search, Enter should go to the terminal.
		if searchInputFocused && tab != nil {
			s := w.getScroll(tab.ID)
			if ev.Action == "" && len(ev.Bytes) > 0 {
				switch ev.Bytes[0] {
				case '\r': // Enter — same as > (next match)
					s.NextMatch()
					if _, rows := w.gridSize(); rows > 0 {
						s.ScrollToCurrentMatch(rows)
					}
					w.searchFocusInput = true
					continue
				case '\n': // Shift+Enter — same as < (previous match)
					s.PrevMatch()
					if _, rows := w.gridSize(); rows > 0 {
						s.ScrollToCurrentMatch(rows)
					}
					w.searchFocusInput = true
					continue
				}
			}
		}

		if ev.Action != "" {
			w.dispatchAction(ev.Action)
			actionDispatched = true
			continue
		}

		// Don't forward key bytes to terminal during search
		if searchInputFocused {
			continue
		}

		if len(ev.Bytes) > 0 && tab != nil {
			// scroll_on_keystroke: gnome-terminal-style "any keypress
			// snaps back to live position". When off, the user can
			// type into the prompt (still sends to PTY) while staying
			// scrolled back to inspect history.
			if w.app.cfg.Scrollback.ScrollOnKeystroke {
				if s, ok := w.scroll[tab.ID]; ok {
					s.Reset()
				}
			}
			w.sel.clear() // typing clears selection
			tab.Terminal.Write(ev.Bytes)
		}
	}

	// Don't forward text input when: searching, a keybind just fired,
	// ImGui wants keyboard, or Ctrl is held (avoids leaking chars from
	// Ctrl+key combos). On macOS the cimgui ModSuper flag carries the
	// physical Ctrl key (ImGui's ConfigMacOSXBehaviors swaps the two), so
	// check both flags to suppress leaks for both physical Ctrl combos
	// and Cmd shortcuts.
	ctrlHeld := imgui.IsKeyDown(imgui.ModCtrl) ||
		(runtime.GOOS == "darwin" && imgui.IsKeyDown(imgui.ModSuper))
	if searchInputFocused || actionDispatched || imgui.CurrentIO().WantTextInput() || ctrlHeld {
		return
	}

	// Read text input from ImGui's character queue (SDL_TEXTINPUT events)
	if tab != nil {
		io := imgui.CurrentIO()
		chars := io.InputQueueCharacters()
		altHeld := imgui.IsKeyDown(imgui.ModAlt)
		if chars.Size > 0 {
			// scroll_on_keystroke: same gate as the translated-key
			// path above. Without checking here, plain letters /
			// numbers (which come through InputQueueCharacters, not
			// the special-key dispatcher) would always snap back to
			// live and the pref would do nothing for typing.
			if w.app.cfg.Scrollback.ScrollOnKeystroke {
				if s, ok := w.scroll[tab.ID]; ok {
					s.Reset()
				}
			}
			w.sel.clear()
			for _, ch := range chars.Slice() {
				if ch > 0 && ch < 0x10FFFF {
					buf := make([]byte, 5)
					var n int
					// Alt+key → ESC key (Meta mapping)
					if altHeld {
						buf[0] = 0x1b
						n = 1 + encodeRune(buf[1:], rune(ch))
					} else {
						n = encodeRune(buf, rune(ch))
					}
					tab.Terminal.Write(buf[:n])
				}
			}
		}
	}
}

// encodeRune encodes a rune to UTF-8 bytes.
func encodeRune(buf []byte, r rune) int {
	switch {
	case r < 0x80:
		buf[0] = byte(r)
		return 1
	case r < 0x800:
		buf[0] = byte(0xC0 | (r >> 6))
		buf[1] = byte(0x80 | (r & 0x3F))
		return 2
	case r < 0x10000:
		buf[0] = byte(0xE0 | (r >> 12))
		buf[1] = byte(0x80 | ((r >> 6) & 0x3F))
		buf[2] = byte(0x80 | (r & 0x3F))
		return 3
	default:
		buf[0] = byte(0xF0 | (r >> 18))
		buf[1] = byte(0x80 | ((r >> 12) & 0x3F))
		buf[2] = byte(0x80 | ((r >> 6) & 0x3F))
		buf[3] = byte(0x80 | (r & 0x3F))
		return 4
	}
}

func (w *Window) dispatchAction(action string) {
	// Action namespace "new_tab_remote:<host>" opens a NEW tab on
	// the named host. "attach_remote:<host>" adopts every existing
	// remote tab into this window — for the "show me what I had
	// open on kh" UX. Both share the per-host Hub (one SSH
	// connection serves all tabs). Failures log + drop.
	if strings.HasPrefix(action, "new_tab_remote:") {
		host := action[len("new_tab_remote:"):]
		if err := w.openRemoteTab(host); err != nil {
			fmt.Fprintf(os.Stderr, "xerotty: new_tab_remote %s: %v\n", host, err)
		}
		return
	}
	if strings.HasPrefix(action, "attach_remote:") {
		host := action[len("attach_remote:"):]
		if err := w.openRemoteReattach(host); err != nil {
			fmt.Fprintf(os.Stderr, "xerotty: attach_remote %s: %v\n", host, err)
		}
		return
	}
	if action == "connect_remote" {
		w.openConnectDialog()
		return
	}
	// "kick_client:<hub>:<clientID>" force-disconnects an attached
	// client (Remote → Clients). ClientIDs may themselves contain
	// colons ("xerotty-gui:xryzen"), so split off the hub name only.
	if strings.HasPrefix(action, "kick_client:") {
		rest := action[len("kick_client:"):]
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) != 2 {
			return
		}
		hub := w.app.hubsByName()[parts[0]]
		if hub == nil {
			fmt.Fprintf(os.Stderr, "xerotty: kick_client: no hub %q\n", parts[0])
			return
		}
		if err := hub.KickClient(parts[1]); err != nil {
			fmt.Fprintf(os.Stderr, "xerotty: kick_client %s: %v\n", rest, err)
		}
		// Re-fetch soon so the menu reflects the kick on next open.
		w.app.refreshClientsMenu()
		return
	}
	// "remote_new_tab" / "remote_new_window" act on the host of the
	// CURRENTLY active tab — a new tab/window on the same remote box
	// you're looking at. No-op (with a note) when the active tab is
	// local; the plain new_tab/new_window actions cover the local case.
	if action == "remote_new_tab" || action == "remote_new_window" {
		t := w.tabs.Active()
		if t == nil || t.Host == "" {
			fmt.Fprintf(os.Stderr, "xerotty: %s: active tab is not on a remote host\n", action)
			return
		}
		var err error
		if action == "remote_new_tab" {
			err = w.openRemoteTab(t.Host)
		} else {
			err = w.openRemoteWindow(t.Host)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "xerotty: %s %s: %v\n", action, t.Host, err)
		}
		return
	}
	switch action {
	case "new_tab":
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
	case "close_tab":
		w.tabs.CloseActive()
		// macOS's NSWindow performClose: default-binds to Cmd+W and
		// fires AFTER our keybind handler, so without swallowing the
		// "close one tab" keypress also closes the whole window. The
		// flag is checked in the PlatformRequestClose path below. ~5
		// frames is enough to cover the latency between the keybind
		// firing and SDL3 surfacing the OS-level close event.
		w.swallowOSCloseFrames = 5
	case "new_window":
		// Single-process multi-window: append a new Window to this
		// App's slice. The render loop in Run() picks it up next
		// frame and wraps it in an ImGui top-level window that
		// multi-viewport auto-promotes to its own OS window. Same
		// NSApplication on macOS = one Dock icon for N windows;
		// same WM_CLASS on Linux = one taskbar group. See
		// docs/MULTI_WINDOW_REFACTOR.md for the architectural why.
		w.app.spawnWindow()
	case "next_tab":
		w.tabs.Next()
		if t := w.tabs.Active(); t != nil {
			w.tabSwitchReq = t.ID
		}
	case "prev_tab":
		w.tabs.Prev()
		if t := w.tabs.Active(); t != nil {
			w.tabSwitchReq = t.ID
		}
	case "copy":
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
	case "paste":
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
	case "paste_selection":
		text, err := input.PrimaryRead()
		if err == nil && text != "" {
			w.pasteText(text)
		}
	case "fullscreen":
		w.fullscreen = !w.fullscreen
		// Multi-window: target THIS Window's SDL_Window, not the
		// hidden carrier. SDL_GL_GetCurrentWindow() would silently
		// fullscreen the invisible carrier and look like a no-op.
		if h := w.sdlWindowHandle(); h != 0 {
			platform.SetFullscreen(h, w.fullscreen)
		}
	case "scroll_page_up":
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			_, rows := w.gridSize()
			s.PageUp(rows, tab.Terminal.ScrollbackLen())
		}
	case "scroll_page_down":
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			_, rows := w.gridSize()
			s.PageDown(rows)
		}
	case "scroll_top":
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			s.Offset = tab.Terminal.ScrollbackLen()
		}
	case "scroll_bottom":
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			s.Reset()
		}
	case "search":
		if tab := w.tabs.Active(); tab != nil {
			s := w.getScroll(tab.ID)
			s.OpenSearch()
			w.searchFocusInput = true
		}
	case "toggle_opacity":
		// Flip between the configured opacity and fully opaque. Opaque is
		// the screenshot-safe state: a translucent window blends whatever
		// is behind it, so a capture can leak other windows — toggle to
		// opaque before shooting. App-level so all windows flip together;
		// the per-Window opacity apply in Run() picks it up. PostWake
		// forces an immediate render so the change is visible at once.
		w.app.forceOpaque.Store(!w.app.forceOpaque.Load())
		platform.PostWake()
	case "font_size_up":
		// Per-window zoom — only this Window's font size changes.
		// Other Windows keep their own zoom level (iTerm2-style).
		w.fontSize += 1
		w.updateFontMetrics()
	case "font_size_down":
		if w.fontSize > 6 {
			w.fontSize -= 1
			w.updateFontMetrics()
		}
	case "font_size_reset":
		// Reset to the configured default for this Window only.
		w.fontSize = renderer.PixelSize(&w.app.cfg)
		w.updateFontMetrics()
	case "select_all":
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
	case "clear_scrollback":
		if tab := w.tabs.Active(); tab != nil {
			tab.Terminal.ClearScrollback()
			if s, ok := w.scroll[tab.ID]; ok {
				s.Reset()
			}
		}
	case "reset_terminal":
		if tab := w.tabs.Active(); tab != nil {
			// Send RIS (Reset to Initial State) escape sequence
			tab.Terminal.Write([]byte("\x1bc"))
			tab.Terminal.ClearScrollback()
			if s, ok := w.scroll[tab.ID]; ok {
				s.Reset()
			}
			w.sel.clear()
		}
	case "open_link":
		if w.hoveredLink != nil {
			openURL(w.hoveredLink.URL, w.app.cfg.Links.Opener)
		}
	case "copy_link":
		if w.hoveredLink != nil {
			input.ClipboardWrite(w.hoveredLink.URL)
		}
	case "rename_tab":
		if tab := w.tabs.Active(); tab != nil {
			w.renameBuffer = tab.DisplayTitle()
			w.renamingTab = true
			imgui.OpenPopupStr("Rename Tab")
		}
	case "preferences":
		w.openPreferences()
	default:
		// Check for parameterized actions
		if strings.HasPrefix(action, "goto_tab:") {
			nStr := strings.TrimPrefix(action, "goto_tab:")
			if n, err := strconv.Atoi(nStr); err == nil {
				w.tabs.GoTo(n)
				if t := w.tabs.Active(); t != nil {
					w.tabSwitchReq = t.ID
				}
			}
		} else if strings.HasPrefix(action, "set_theme:") {
			name := strings.TrimPrefix(action, "set_theme:")
			if t, err := themes.Load(name); err == nil {
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
		} else if strings.HasPrefix(action, "exec:") {
			ctx := w.menuContext()
			menu.ExecAction(action, ctx)
		}
	}
}

// altScrollSeq returns the cursor-key escape sequence for one
// alternate-scroll wheel step — arrow Up (up=true) or Down — encoded
// per DECCKM: application cursor keys (ESC O A/B) when appCursor is set
// (ncurses apps like mutt enable it via smkx), else normal (ESC [ A/B).
func altScrollSeq(up, appCursor bool) []byte {
	switch {
	case up && appCursor:
		return []byte("\x1bOA")
	case up:
		return []byte("\x1b[A")
	case appCursor:
		return []byte("\x1bOB")
	default:
		return []byte("\x1b[B")
	}
}

func (w *Window) getScroll(tabID int) *scrollback.State {
	if s, ok := w.scroll[tabID]; ok {
		return s
	}
	s := scrollback.New()
	w.scroll[tabID] = s
	return s
}

func (w *Window) updateFontMetrics() {
	// Per-window font size — Cmd+= / Cmd+- on one Window doesn't
	// touch the others. w.fontSize is initialized from cfg.Font.Size
	// at spawn; from then on each Window has its own zoom level.
	if w.fontSize <= 0 {
		w.fontSize = renderer.PixelSize(&w.app.cfg)
	}
	pxSize := w.fontSize
	if w.app.baseFontSize <= 0 {
		w.app.baseFontSize = pxSize
	}

	// Capture current grid dimensions BEFORE scaling so we can resize
	// the window to keep the same number of cols/rows.
	cols, rows := w.gridSize()

	// Scale cell metrics proportionally. Ceil AFTER scaling —
	// baseCellW/H is the pre-ceil float advance so `ceil(baseCellW *
	// scale)` is the same answer measureCell would give at this zoom
	// (no compounding of ceiling errors per step).
	scale := pxSize / w.app.baseFontSize
	w.cellW, w.cellH = ceilCell(w.app.baseCellW*scale, w.app.baseCellH*scale)
	w.renderer.Metrics = renderer.CellMetrics{Width: w.cellW, Height: w.cellH}
	w.renderer.FontSize = pxSize

	// Rebuild the glyph cache at the new pxSize. Terminal cells render
	// through r.Glyphs.Get → AddImageV at the cached texture's native
	// size, so the cache's pxSize IS the size glyphs render at —
	// scaling the cell width without rebuilding the cache leaves
	// glyphs frozen at the old size and cells grow around them (the
	// "space zooms but text doesn't" symptom). Old textures are queued
	// for GPU deletion via the TextureManager and are safe to drop
	// here because frame() runs the wheel handler before any
	// renderer.Draw / AddImageV calls reference them.
	if w.renderer.Glyphs != nil && fontsys.Default != nil {
		primaryPath := renderer.ResolveFontPath(&w.app.cfg)
		if primaryPath != "" {
			fbScale := imgui.CurrentIO().DisplayFramebufferScale().X
			if fbScale <= 0 {
				fbScale = 1
			}
			if c, err := glyphcache.New(fontsys.Default, platform.Textures(), primaryPath, pxSize, fbScale); err == nil {
				w.renderer.Glyphs.Close()
				w.renderer.Glyphs = c
				w.renderer.InvalidateCellCache()
			}
		}
	}

	// Resize window to maintain the same grid at the new cell size.
	// Set w.width/w.height immediately so this frame renders correctly;
	// the per-frame DisplaySize sync will correct them on the next
	// frame if the WM didn't honour the request.
	pad := float32(w.app.cfg.Appearance.Padding) * 2
	// Add back the cellSafetyMargin so the post-resize gridSize() returns the
	// SAME cols/rows we're trying to preserve. Without this, every zoom step
	// loses one row+col because gridSize subtracts the margin from available
	// space.
	// cellOriginInsetX must be included alongside cellSafetyMarginH so
	// gridSize (which subtracts BOTH) recomputes back to the same cols.
	// Without the inset on Darwin (where it's 4px to clear the NSWindow
	// corner radius), each zoom step floors away one column.
	newW := int(math.Ceil(float64(float32(cols)*w.cellW + pad + cellSafetyMarginH + cellOriginInsetX)))
	newH := int(math.Ceil(float64(float32(rows)*w.cellH + pad + w.tabBarH + cellSafetyMarginV)))
	// Every Window's OS geometry rides ImGui multi-viewport. Stash
	// the target size and flip pendingResize; wrappedFrame's Begin
	// uses CondAlways instead of CondFirstUseEver on the next frame,
	// which ImGui propagates to the platform window via
	// Platform_SetWindowSize.
	w.width = newW
	w.height = newH
	w.pendingResize = true
	w.skipDisplaySync = 2
	w.resizeTerminals()

	// Show overlay with the new zoom level. Percent is current pxSize
	// over the configured base, rounded — so default reads 100%, zoom-in
	// reads >100%, zoom-out reads <100%. skipDisplaySync prevents the
	// drag-resize trigger in frame() from clobbering this with cols×rows
	// for the next couple frames.
	percent := int(math.Round(float64(pxSize / w.app.baseFontSize * 100)))
	w.resizeOverlayText = fmt.Sprintf("%d%%", percent)
	w.resizeTime = imgui.Time()
	w.resizeOverlay = true
}

// renderProposalGate draws the propose-mode approval banner: a
// small overlay window listing each pending agent-proposed write
// with Approve / Drop buttons. Only the active Window renders it
// (the queue is session-global, one daemon). No-op when empty or
// not in daemon mode.
//
// Approve/Drop send a ProposalResolve over the daemon connection;
// the daemon applies/discards and broadcasts the updated queue,
// which refreshes a.pendingProposals via the hub callback.
func (w *Window) renderProposalGate() {
	// Render in the active window whenever ANY hub has pending
	// proposals — NOT gated on a default daemonHub. Ad-hoc remote
	// tabs (opened via the Remote menu in PTY-default mode) have a
	// remote hub but no default hub; their propose-mode writes
	// still need a visible gate. Each proposal carries its own
	// hub, so Approve/Drop works regardless.
	if w.app.active != w {
		return
	}
	w.app.proposalsMu.Lock()
	props := w.app.pendingProposals
	w.app.proposalsMu.Unlock()
	if len(props) == 0 {
		return
	}

	imgui.SetNextWindowBgAlpha(0.92)
	flags := imgui.WindowFlags(imgui.WindowFlagsNoCollapse |
		imgui.WindowFlagsAlwaysAutoResize |
		imgui.WindowFlagsNoSavedSettings |
		imgui.WindowFlagsNoFocusOnAppearing)
	if !imgui.BeginV("Agent proposals"+w.imguiSuffix(), nil, flags) {
		imgui.End()
		return
	}
	imgui.Text("AI agent wants to run (propose mode):")
	imgui.Separator()
	for i, gp := range props {
		p := gp.info
		// Unique widget IDs per (host, index) — two hubs can both
		// have a proposal at index 0.
		uid := fmt.Sprintf("##p%s%d%s", gp.host, p.Index, w.imguiSuffix())
		imgui.TextUnformatted(fmt.Sprintf("%s tab %d [%s]: %s", gp.host, p.TabID, p.Kind, p.Preview))
		imgui.SameLine()
		hub := gp.hub
		idx := p.Index
		if imgui.Button("Approve" + uid) {
			if hub != nil {
				_ = hub.ResolveProposal(idx, true)
			}
		}
		imgui.SameLine()
		if imgui.Button("Drop" + uid) {
			if hub != nil {
				_ = hub.ResolveProposal(idx, false)
			}
		}
		_ = i
	}
	imgui.End()
}

// humanizeAge renders an activity age compactly for the tab tooltip:
// "just now", "42s ago", "5m ago", "3h ago", "2d ago", "1w ago".
func humanizeAge(now, ts time.Time) string {
	if ts.IsZero() {
		return "unknown"
	}
	d := now.Sub(ts)
	switch {
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw ago", int(d.Hours()/(24*7)))
	}
}

func (w *Window) renderTabBar() {
	w.tabBarHovered = false
	if w.tabs.Count() <= 1 {
		return // Don't show tab bar with single tab
	}

	// Custom tab bar — every tab gets an equal share of the available
	// width (totalW / numTabs), iTerm/Terminal.app style. ImGui's
	// BeginTabBar doesn't have a stretch-to-fit option (only Shrink
	// and Scroll fitting policies), so we lay out tabs manually with
	// InvisibleButtons for hit detection and drawlist primitives for
	// the visuals. Rendered into the wrapper's WindowDrawList so the
	// tab visuals layer ON TOP of the terminal cells drawn earlier in
	// frame().
	originX := w.contentOriginX
	originY := w.contentOriginY
	totalW := float32(w.width)
	height := w.tabBarH
	numTabs := w.tabs.Count()
	tabW := totalW / float32(numTabs)

	style := imgui.CurrentStyle()
	col := func(c imgui.Col) uint32 { return imgui.ColorU32Col(c) }
	activeBg := col(imgui.ColTabSelected)
	hoverBg := col(imgui.ColTabHovered)
	inactiveBg := col(imgui.ColTab)
	textCol := col(imgui.ColText)
	underlineCol := col(imgui.ColTabSelectedOverline)

	drawList := imgui.WindowDrawList()
	closeBtnW := imgui.FrameHeight() * 0.6
	framePad := style.FramePadding()
	clickedIdx := -1
	closedIdx := -1
	now := time.Now()

	for i, tab := range w.tabs.Tabs {
		x0 := originX + float32(i)*tabW
		x1 := originX + float32(i+1)*tabW
		y0 := originY
		y1 := originY + height
		isActive := i == w.tabs.ActiveIdx

		// Becoming active clears any pending bell urgency — the
		// user is now looking at the tab that beeped.
		if isActive && tab.BellPending() {
			tab.SetBellPending(false)
		}

		// Activity view-tracking: the active tab is "seen" every frame;
		// a background tab whose last output is newer than when we last
		// saw it has UNSEEN output → the top-edge glow lights up (no
		// size change). Cleared the moment it becomes active again.
		if isActive {
			w.tabViewed[tab.ID] = now
		}
		lastOut := tab.Terminal.LastOutput()
		unviewed := !isActive && !lastOut.IsZero() && lastOut.After(w.tabViewed[tab.ID])

		// Whole-tab invisible button for hit detection. We use ONE
		// button rather than two (with the close X as a second
		// overlapping InvisibleButton), because ImGui's "first item
		// wins hover" rule means a later-submitted close-button
		// InvisibleButton can't override the tab's. Instead we
		// dispatch the click based on where the cursor was: in the
		// close-button rect → close; otherwise → switch.
		closeX0 := x1 - closeBtnW - framePad.X
		closeY0 := y0 + (height-closeBtnW)/2
		closeX1 := closeX0 + closeBtnW
		closeY1 := closeY0 + closeBtnW

		imgui.SetCursorScreenPos(imgui.Vec2{X: x0, Y: y0})
		tabID := fmt.Sprintf("##tab%d%s", tab.ID, w.imguiSuffix())
		// PressedOnClick → fires on mouse-DOWN instead of mouse-UP,
		// so tab switches and close-button hits feel snappy rather
		// than waiting for the release edge. Flag lives in
		// ButtonFlagsPrivate, hence the cast.
		clicked := imgui.InvisibleButtonV(tabID, imgui.Vec2{X: tabW, Y: height}, imgui.ButtonFlags(imgui.ButtonFlagsPressedOnClick))
		hovered := imgui.IsItemHovered()
		if hovered {
			w.tabBarHovered = true
		}
		// Hover reveals the exact activity ages (the glow is the
		// at-a-glance cue; this is the precise readout). DelayNormal
		// (0.4s) instead of BeginItemTooltip's DelayShort (0.15s) — the
		// short delay popped tooltips on every incidental pass over the
		// tab bar. Stationary means the clock only starts once the
		// mouse settles; NoSharedDelay stops tab-to-tab hover from
		// inheriting the previous tab's elapsed delay (each tab re-arms
		// from zero instead of the tooltip chasing the cursor).
		if imgui.IsItemHoveredV(imgui.HoveredFlagsDelayNormal|
			imgui.HoveredFlagsStationary|imgui.HoveredFlagsNoSharedDelay) &&
			imgui.BeginTooltip() {
			imgui.Text("output " + humanizeAge(now, lastOut))
			imgui.Text("input  " + humanizeAge(now, tab.Terminal.LastInput()))
			imgui.EndTooltip()
		}

		// Background
		bg := inactiveBg
		switch {
		case isActive:
			bg = activeBg
		case hovered:
			bg = hoverBg
		}
		// Visible tab body: inset only on the right edge (except for
		// the last tab) so adjacent tabs have a single-pixel gap
		// between them. Rounded top corners + square bottom so the
		// tab sits on the underline like a tab dock.
		const sideGap = 1
		const tabRounding = 3
		bgX0 := x0
		bgX1 := x1
		if i < numTabs-1 {
			bgX1 -= sideGap
		}
		drawList.AddRectFilledV(
			imgui.Vec2{X: bgX0, Y: y0},
			imgui.Vec2{X: bgX1, Y: y1},
			bg,
			tabRounding,
			imgui.DrawFlagsRoundCornersTop,
		)
		// Unseen-activity glow: a soft accent line bleeding down from
		// the tab's TOP edge (background layer, no size change) — a
		// background tab lights up when it produces output you haven't
		// looked at, and goes dark when you switch to it.
		if unviewed {
			glowCol := w.renderer.Theme.TabActivityGlow
			if glowCol == 0 { // theme didn't set one → fall back to the tab accent
				glowCol = underlineCol
			}
			accent := glowCol & 0x00FFFFFF // color only; the gradient supplies alpha
			// Crisp edge line (rounded top to match the tab body).
			drawList.AddRectFilledV(
				imgui.Vec2{X: bgX0, Y: y0},
				imgui.Vec2{X: bgX1, Y: y0 + 2},
				accent|0xF0000000,
				tabRounding,
				imgui.DrawFlagsRoundCornersTop,
			)
			// Downward glow bleed, fading to transparent.
			drawList.AddRectFilledMultiColor(
				imgui.Vec2{X: bgX0, Y: y0 + 2},
				imgui.Vec2{X: bgX1, Y: y0 + 7},
				accent|0xB0000000, accent|0xB0000000, // top: ~69% alpha
				accent, accent, // bottom: transparent
			)
		}
		// Active-tab underline (matches ImGui's TabBarOverline style).
		if isActive {
			overlineH := float32(2)
			drawList.AddRectFilled(
				imgui.Vec2{X: bgX0, Y: y1 - overlineH},
				imgui.Vec2{X: bgX1, Y: y1},
				underlineCol,
			)
		}

		// Centered label, clipped to the tab's interior so long
		// titles don't bleed into the close button or the next tab.
		// Prefix a bell marker for background tabs that beeped.
		label := tab.DisplayTitle()
		if tab.BellPending() && !isActive {
			label = "● " + label
		}
		labelSize := imgui.CalcTextSize(label)
		labelX := x0 + framePad.X
		labelMaxX := x1 - closeBtnW - framePad.X*2
		if labelMaxX-labelX > labelSize.X {
			// Center if there's slack.
			labelX = x0 + (tabW-closeBtnW-labelSize.X)/2
		}
		labelY := y0 + (height-labelSize.Y)/2
		// PushClipRect so long labels truncate at the close-button edge.
		drawList.PushClipRectV(
			imgui.Vec2{X: x0 + framePad.X, Y: y0},
			imgui.Vec2{X: labelMaxX, Y: y1},
			true,
		)
		drawList.AddTextVec2V(imgui.Vec2{X: labelX, Y: labelY}, textCol, label)
		drawList.PopClipRect()

		// Manual hover-test for the close-button area (no separate
		// InvisibleButton — see comment above).
		mp := imgui.MousePos()
		mouseInClose := hovered &&
			mp.X >= closeX0 && mp.X < closeX1 &&
			mp.Y >= closeY0 && mp.Y < closeY1
		// Show the X on hover of the tab or for the active tab.
		// Use full textCol (not dim) so it's readable against the
		// tab background; mouseInClose paints a hover-bg rect behind
		// the X to give clear feedback the button is targetable.
		if hovered || isActive {
			xCol := textCol
			if mouseInClose {
				drawList.AddRectFilled(
					imgui.Vec2{X: closeX0, Y: closeY0},
					imgui.Vec2{X: closeX1, Y: closeY1},
					hoverBg,
				)
			}
			pad := closeBtnW * 0.25
			thick := float32(1.5)
			drawList.AddLineV(
				imgui.Vec2{X: closeX0 + pad, Y: closeY0 + pad},
				imgui.Vec2{X: closeX1 - pad, Y: closeY1 - pad},
				xCol, thick,
			)
			drawList.AddLineV(
				imgui.Vec2{X: closeX1 - pad, Y: closeY0 + pad},
				imgui.Vec2{X: closeX0 + pad, Y: closeY1 - pad},
				xCol, thick,
			)
		}

		// Dispatch the click: cursor inside the close X rect → close,
		// anywhere else on the tab → switch. Gate on an actual mouse
		// press — InvisibleButton also reports `clicked` when ImGui
		// keyboard-nav ACTIVATES the (still nav-focused) button via
		// Enter/Space. Without this gate, hitting Enter in the terminal
		// re-"clicked" a previously-focused tab button and jumped focus
		// to it (the "Enter jumps to the first tab" bug).
		// Same cross-window leak guard as selection: only treat this
		// as our click if the OS window has focus. Without it, clicking
		// another window selected the "highlighted" (hovered) tab here.
		if clicked && imgui.IsMouseClickedBool(imgui.MouseButtonLeft) && w.hasOSFocus() {
			if mouseInClose {
				closedIdx = i
			} else {
				clickedIdx = i
			}
		}
	}

	// Honor any pending Cmd+N / wheel / etc. switch request before
	// applying click-based switches; the click handler below would
	// otherwise be a no-op for the keyboard path since clickedIdx<0.
	if w.tabSwitchReq != -1 {
		for i, tab := range w.tabs.Tabs {
			if tab.ID == w.tabSwitchReq {
				w.tabs.ActiveIdx = i
				break
			}
		}
		w.tabSwitchReq = -1
	}

	if clickedIdx >= 0 {
		w.tabs.ActiveIdx = clickedIdx
		// Click seeds drag-from-this-tab state. Drag only activates
		// once the user moves the cursor past a threshold from the
		// press position, so plain clicks still feel like clicks.
		w.tabDragIdx = clickedIdx
		mp := imgui.MousePos()
		w.tabDragStartX = mp.X
		w.tabDragStartY = mp.Y
	}

	// Drag-to-reorder within this tab bar, OR lift-off into a cross-
	// Window drag. Thresholds are RELATIVE to the press position, not
	// absolute screen Y — otherwise a click that happened to land
	// near either vertical edge of the tab bar would instantly trigger
	// detach with no actual movement.
	const liftOffPx = 30 // vertical movement required to detach
	const dragSlopPx = 4 // dead zone around the click before we consider any drag
	if w.tabDragIdx >= 0 && imgui.IsMouseDown(imgui.MouseButtonLeft) {
		mp := imgui.MousePos()
		dx := mp.X - w.tabDragStartX
		dy := mp.Y - w.tabDragStartY
		moved := dx*dx+dy*dy > dragSlopPx*dragSlopPx

		// Cursor feedback once we're past the dead zone — "Hand"
		// reads as "you're dragging this".
		if moved {
			imgui.SetMouseCursor(imgui.MouseCursorHand)
		}

		if dy > liftOffPx || dy < -liftOffPx {
			// Lift off — pull the tab out of this Window into the
			// process-wide drag state. After this point the tab
			// is no longer in any Window's tabs.Manager. We also
			// kick off a Wayland data_device drag so the compositor
			// will route cursor enter/leave/drop to other surfaces
			// during the held drag (Wayland's implicit pointer
			// grab otherwise blocks cross-surface events).
			if t := w.tabs.RemoveTab(w.tabDragIdx); t != nil {
				drag := &tabDrag{
					Term:      t.Terminal,
					Label:     t.DisplayTitle(),
					Title:     t.Title(),
					Host:      t.Host,
					From:      w,
					LastFocus: w.sdlWindowHandle(),
				}
				if cur := platform.MouseFocusWindowID(); cur != 0 {
					drag.LastFocus = cur
				}
				drag.WaylandStarted = platform.StartWaylandTabDrag(w.sdlWindowHandle())
				w.app.dragTab = drag
			}
			w.tabDragIdx = -1
		} else if moved && mp.Y >= originY && mp.Y < originY+height && mp.X >= originX {
			targetIdx := int((mp.X - originX) / tabW)
			if targetIdx < 0 {
				targetIdx = 0
			}
			if targetIdx >= numTabs {
				targetIdx = numTabs - 1
			}
			if targetIdx != w.tabDragIdx {
				moved := w.tabs.Tabs[w.tabDragIdx]
				w.tabs.MoveTab(w.tabDragIdx, targetIdx)
				w.tabDragIdx = targetIdx
				// Persist the reorder to the daemon so reattach
				// restores the new order. Same-window reorder
				// = WindowMoveTab with the same window ID +
				// new index. No-op for PTY-backed tabs.
				w.sendDaemonMoveTab(moved, w.daemonWindowID, int32(targetIdx))
			}
		}
	} else if !imgui.IsMouseDown(imgui.MouseButtonLeft) {
		w.tabDragIdx = -1
	}

	// While a cross-Window drag is in flight (any Window's drag),
	// show the "moving" cursor everywhere over our wrapper so the
	// user has feedback that the floating tab is held.
	if w.app.dragTab != nil {
		imgui.SetMouseCursor(imgui.MouseCursorResizeAll)
	}
	if closedIdx >= 0 {
		w.tabs.CloseTab(closedIdx)
		// Wake the run loop so the next frame fires immediately
		// instead of waiting for WaitEventTimeout to expire. The
		// tab-bar→no-tab-bar transition triggers an SDL window
		// resize at the START of the next frame; without the wake,
		// the user can see up to ~30ms of stale window-size before
		// the resize lands.
		platform.PostWake()
	}
}

// measureMenu estimates the rendered width/height of a menu item list
// in the popup's default ImGui font + style. Metrics are empirical
// (item rows advance ~17px, separators ~4px, 8px window padding each
// side; ~7px per default-font char). Submenu items add an arrow's
// worth of width. Used to size the popup surface so a cascaded submenu
// has room beside its parent.
func measureMenu(items []config.MenuItem) (w, h float32) {
	const (
		itemAdvance = 17.0
		sepAdvance  = 4.0
		padY        = 8.0
		padX        = 8.0
		charW       = 7.0
		arrowW      = 20.0 // submenu ">" indicator + spacing
	)
	w = 120
	h = padY * 2
	for _, item := range items {
		if item.Action == "separator" {
			h += sepAdvance
			continue
		}
		iw := float32(len(item.Label))*charW + float32(len(item.Shortcut))*charW + 30
		if len(item.Submenu) > 0 {
			iw += arrowW
		}
		if iw > w {
			w = iw
		}
		h += itemAdvance
	}
	w += padX * 2
	return
}

// measureCascade returns the width/height the cascade of nested
// submenus can occupy BEYOND the given level: the widest (deepest)
// chain of submenu widths and the tallest chain of submenu heights.
// Zero when no item has a submenu.
func measureCascade(items []config.MenuItem) (w, h float32) {
	for _, item := range items {
		if len(item.Submenu) == 0 {
			continue
		}
		sw, sh := measureMenu(item.Submenu)
		dw, dh := measureCascade(item.Submenu)
		if sw+dw > w {
			w = sw + dw
		}
		if sh+dh > h {
			h = sh + dh
		}
	}
	return
}

func (w *Window) renderContextMenu() {
	if !w.contextMenuOpen {
		return
	}

	// Open an SDL3 xdg_popup parented to THIS Window's OS window so
	// the compositor positions the menu relative to the parent
	// surface (Wayland forbids absolute toplevel positioning, which
	// is why the old multi-viewport-pop-out approach landed the menu
	// in the middle of the screen). RunImGuiPopup blocks until the
	// menu dismisses — the rest of the app pauses, which matches
	// native context-menu semantics on every OS.
	parentID := uintptr(0)
	if vp := w.viewport(); vp != nil {
		parentID = vp.PlatformHandle()
	}
	// Click pos is in absolute desktop coords. The popup wants
	// parent-relative coords. Compute relative from the Window's
	// captured contentOrigin (the wrapper Begin's screen-space top-
	// left) so the popup opens at the click position even if the
	// compositor stuck the OS window somewhere we didn't ask for.
	relX := int(w.contextMenuX - w.contentOriginX)
	relY := int(w.contextMenuY - w.contentOriginY)
	if relX < 0 {
		relX = 0
	}
	if relY < 0 {
		relY = 0
	}

	ctx := w.menuContext()
	// Pre-compute popup size. The SDL surface is deliberately sized to
	// fit the top-level menu PLUS the widest cascaded submenu to its
	// right (and tall enough for it to drop down), so BeginMenu can
	// open the submenu beside the parent instead of clamping it on top.
	// The surface is transparent, so the extra room is invisible until
	// a submenu opens. measureMenu's metrics are empirical for ImGui's
	// default font + style (see popup_imgui.cpp).
	expanded := w.app.expandMenu(w.app.cfg.Menu.Items)
	mainW, mainH := measureMenu(expanded)
	// Deepest cascade beyond the main menu (submenus can nest —
	// Remote → Clients — and each level opens beside its parent, so
	// the surface must fit the whole chain, not just one level).
	subW, subH := measureCascade(expanded)
	// Width: main + cascaded submenus side by side. Height: each
	// level can open as low as the bottom of its parent and drop its
	// own height further, so reserve the sum (transparent slack —
	// harmless).
	popupW := mainW + subW + 8
	popupH := mainH
	if subW > 0 {
		popupH = mainH + subH
	}
	var selectedAction string
	platform.RunImGuiPopup(parentID, relX, relY, int(popupW), int(popupH),
		func() platform.PopupMenuDrawResult {
			// Anchor the menu at the surface's top-left, sized to the
			// MEASURED main-menu rect — not the whole (oversized)
			// surface (which would paint the menu background across the
			// transparent submenu room) and not AlwaysAutoResize (which
			// shrinks to the widest label and ignores the shortcuts
			// drawn via the draw list, squashing label and shortcut
			// together). The submenu opens as its own child window into
			// the transparent area to the right.
			imgui.SetNextWindowPos(imgui.Vec2{X: 0, Y: 0})
			imgui.SetNextWindowSize(imgui.Vec2{X: mainW, Y: mainH})
			flags := imgui.WindowFlagsNoTitleBar |
				imgui.WindowFlagsNoResize |
				imgui.WindowFlagsNoMove |
				imgui.WindowFlagsNoSavedSettings |
				imgui.WindowFlagsNoCollapse |
				imgui.WindowFlagsNoScrollbar
			var action string
			if imgui.BeginV("##popupmenu", nil, flags) {
				action = menu.RenderItemsOnly(expanded, ctx)
				// XEROTTY_MOUSE_DEBUG: ImGui-side hover truth. Sweep
				// the menu top→bottom: contentMax vs winSize shows
				// whether items overflow the measured window (their
				// hit areas clip); winHov flipping false while mouse
				// is visually inside the menu marks the dead zone.
				if os.Getenv("XEROTTY_MOUSE_DEBUG") != "" {
					mp := imgui.MousePos()
					cm := imgui.CursorPos()
					ws := imgui.WindowSize()
					fmt.Fprintf(os.Stderr,
						"[xtty-menu] mouse=%.0f,%.0f winSize=%.0fx%.0f contentMax=%.0f,%.0f winHov=%v anyHov=%v\n",
						mp.X, mp.Y, ws.X, ws.Y, cm.X, cm.Y,
						imgui.IsWindowHoveredV(imgui.HoveredFlagsAllowWhenBlockedByPopup|imgui.HoveredFlagsChildWindows),
						imgui.IsWindowHoveredV(imgui.HoveredFlagsAnyWindow))
				}
			}
			imgui.End()
			var res platform.PopupMenuDrawResult
			if action != "" {
				selectedAction = action
				res.Close = true
			}
			// Dismiss on a click that lands in the transparent area
			// (outside the menu/submenu windows) — the surface is
			// bigger than the visible menu now, and the C side only
			// auto-dismisses clicks on OTHER OS windows.
			if imgui.IsMouseClickedBool(imgui.MouseButtonLeft) || imgui.IsMouseClickedBool(imgui.MouseButtonRight) {
				if !imgui.CurrentIO().WantCaptureMouse() {
					res.Close = true
				}
			}
			return res
		})

	w.contextMenuOpen = false
	w.contextMenuCaptured = false
	if selectedAction != "" {
		w.dispatchAction(selectedAction)
	}
	platform.PostWake()
}

func (w *Window) menuContext() *menu.Context {
	ctx := &menu.Context{
		HasSelection: w.sel.active,
		Selection:    w.selectedText(),
		ForceOpaque:  w.app.forceOpaque.Load(),
	}
	if tab := w.tabs.Active(); tab != nil {
		ctx.TabTitle = tab.DisplayTitle()
		// CWD detection via /proc
		if tab.Terminal != nil {
			ctx.CWD = getCWD(tab.Terminal)
		}
	}
	if w.hoveredLink != nil {
		ctx.HasLink = true
		ctx.Link = w.hoveredLink.URL
	}
	return ctx
}

func (w *Window) renderSearchOverlay() {
	tab := w.tabs.Active()
	if tab == nil {
		w.searchInputFocused = false
		return
	}
	s := w.getScroll(tab.ID)
	if !s.Searching {
		w.searchInputFocused = false
		return
	}

	// AlwaysAutoResize so the overlay stays as compact as the original
	// did. The match counter uses a fixed-width Dummy slot below so
	// digit-count changes (1/9 vs 10/14 vs 100/120) don't trigger
	// the auto-resize and reshuffle the buttons.
	vpX, vpY := w.contentOriginX, w.contentOriginY
	// Anchor by TOP-RIGHT pivot so the overlay's right edge sits
	// inside the wrapper regardless of its actual width — using a
	// fixed left-edge offset breaks any time the overlay's natural
	// auto-resized width grows past that offset. Inset by the
	// scrollbar width so the overlay doesn't cover the scrollbar.
	scrollbarInset := float32(w.app.cfg.Scrollbar.Width)
	// Pin the overlay to the wrapper's viewport so multi-viewport's
	// auto-pop-out logic can't detach it into its own OS window —
	// without this, the overlay can end up "floating behind" the
	// wrapper and the user has to close/reopen to get it back.
	if w.imViewport != nil {
		imgui.SetNextWindowViewport(w.imViewport.ID())
	}
	imgui.SetNextWindowPosV(
		imgui.Vec2{X: vpX + float32(w.width) - scrollbarInset, Y: vpY + w.tabBarH},
		imgui.CondAlways,
		imgui.Vec2{X: 1, Y: 0},
	)
	// NOT setting NoBringToFrontOnFocus here even though the wrapper has
	// it — this overlay needs to be the focused ImGui window so the
	// InputText keyboard nav routes correctly. NavWindow has to be the
	// overlay; without that, re-opening Ctrl-F (overlay → Esc → overlay)
	// can leave keyboard focus stuck on the wrapper and the input never
	// captures keystrokes. Z-order is enforced separately via
	// InternalBringWindowToDisplayFront at the end of this function.
	flags := imgui.WindowFlagsNoTitleBar | imgui.WindowFlagsNoResize |
		imgui.WindowFlagsNoMove | imgui.WindowFlagsNoScrollbar |
		imgui.WindowFlagsAlwaysAutoResize | imgui.WindowFlagsNoDocking

	w.searchInputFocused = false
	searchWinName := "##search" + w.imguiSuffix()
	if imgui.BeginV(searchWinName, nil, flags) {
		// Track actual rendered width for the selection hit-test.
		w.searchOverlayW = imgui.WindowWidth()

		imgui.SetNextItemWidth(180)

		// Re-focus the input when requested (on open, after < > clicks, or
		// when it loses focus to the terminal). Guard only on IsMouseDown
		// so we don't snatch ActiveId mid-click. We DO NOT guard on
		// IsMouseReleased — the macOS mirror's transition-based synthetic
		// UP events flip IsMouseReleased true on unrelated frames, which
		// was blocking the focus grab on Ctrl-F re-open.
		if w.searchFocusInput && !imgui.IsMouseDown(0) {
			imgui.SetKeyboardFocusHere()
			w.searchFocusInput = false
		}

		_, rows := w.gridSize()
		prevQuery := s.Query
		changed := imgui.InputTextWithHint("##searchinput", "Search...", &s.Query, 0, nil)
		w.searchInputFocused = imgui.IsItemFocused()
		if changed && s.Query != prevQuery {
			// The frame pass's EnsureSearch sees the new query and
			// kicks an async scan; jump to the nearest match when it
			// lands (results adopt on a later frame, so scrolling here
			// would use stale coordinates).
			w.searchJumpOnResult = true
		}
		// Counter: render into a fixed-width Dummy slot via drawList
		// so the window's auto-resize doesn't care about the digit
		// count. The text is drawn manually centered inside the slot.
		imgui.SameLineV(0, 4)
		slotW := float32(56)
		slotH := imgui.FrameHeight()
		slotPos := imgui.CursorScreenPos()
		// AlignTextToFramePadding so the Dummy's Y matches a button row.
		imgui.AlignTextToFramePadding()
		imgui.Dummy(imgui.Vec2{X: slotW, Y: slotH})
		var counterText string
		if len(s.Matches) > 0 {
			counterText = fmt.Sprintf("%d/%d", s.MatchIdx+1, len(s.Matches))
		} else if s.Query != "" {
			counterText = "0"
		}
		if counterText != "" {
			ts := imgui.CalcTextSize(counterText)
			tx := slotPos.X
			ty := slotPos.Y + (slotH-ts.Y)/2
			imgui.WindowDrawList().AddTextVec2V(
				imgui.Vec2{X: tx, Y: ty},
				imgui.ColorU32Col(imgui.ColText),
				counterText,
			)
		}

		imgui.SameLineV(0, 4)

		// Buttons: use ButtonV + debug trace to diagnose click issues.
		prevClicked := imgui.ButtonV("<", imgui.Vec2{X: 20, Y: 0})
		imgui.SameLineV(0, 2)
		nextClicked := imgui.ButtonV(">", imgui.Vec2{X: 20, Y: 0})
		imgui.SameLineV(0, 2)
		closeClicked := imgui.ButtonV("X", imgui.Vec2{X: 20, Y: 0})

		if prevClicked {
			s.PrevMatch()
			s.ScrollToCurrentMatch(rows)
			w.searchFocusInput = true
		}
		if nextClicked {
			s.NextMatch()
			s.ScrollToCurrentMatch(rows)
			w.searchFocusInput = true
		}
		if closeClicked {
			s.CloseSearch()
			w.searchFocusInput = false
			w.searchInputFocused = false
		}

		// Row 2: search options
		optChanged := imgui.Checkbox("CASE", &s.CaseSensitive)
		imgui.SameLineV(0, 8)
		optChanged = imgui.Checkbox("RE", &s.UseRegex) || optChanged
		imgui.SameLineV(0, 8)
		optChanged = imgui.Checkbox("EXACT", &s.WholeWord) || optChanged
		imgui.SameLineV(0, 8)
		optChanged = imgui.Checkbox("WRAP", &s.WrapAround) || optChanged
		if optChanged && s.Query != "" {
			w.searchJumpOnResult = true
		}
	}
	imgui.End()

	// Force the search overlay to the END of ImGui's global windows
	// list so it renders LAST = on top. Without this the wrapper's
	// drawlist (which gets the terminal cells drawn into it earlier
	// in frame()) ends up rendering after the overlay's drawlist
	// when focus events shuffle the order, and the overlay disappears
	// behind the terminal.
	if sw := imgui.InternalFindWindowByName(searchWinName); sw != nil {
		imgui.InternalBringWindowToDisplayFront(sw)
	}
	// Highlights are drawn in the terminal window's draw list (see frame())
}

func (w *Window) drawSearchHighlights(s *scrollback.State, drawList *imgui.DrawList) {
	cellW := w.cellW
	cellH := w.cellH
	_, rows := w.gridSize()
	matchBg := uint32(0x4400FFFF)   // yellow, semi-transparent (ABGR)
	currentBg := uint32(0x8800AAFF) // orange, more opaque (ABGR)

	for i, m := range s.Matches {
		// Convert absolute line index to screen row accounting for scroll offset.
		// Match lines: negative = scrollback, 0+ = live screen.
		// Scroll offset pushes everything down: scrollback lines become visible.
		screenRow := m.Line + s.Offset
		if screenRow < 0 || screenRow >= rows {
			continue
		}

		x := w.renderer.OffsetX + float32(m.Col)*cellW
		y := w.renderer.OffsetY + float32(screenRow)*cellH
		wd := float32(m.Len) * cellW

		bg := matchBg
		if i == s.MatchIdx {
			bg = currentBg
		}

		drawList.AddRectFilled(
			imgui.Vec2{X: x, Y: y},
			imgui.Vec2{X: x + wd, Y: y + cellH},
			bg,
		)
	}
}

func (w *Window) renderResizeOverlay() {
	if !w.resizeOverlay {
		return
	}
	// Honor cfg.Appearance.ResizeOverlay (master on/off) — also clear
	// the latched flag so a later toggle of the pref doesn't stall
	// with a stale overlay sitting at the resizeTime from before.
	if !w.app.cfg.Appearance.ResizeOverlay {
		w.resizeOverlay = false
		w.resizeOverlayText = ""
		return
	}

	elapsed := imgui.Time() - w.resizeTime
	// Total display time from prefs (seconds). Fade occupies the last
	// third (matches the original 1.0s solid + 0.5s fade = 1.5s total
	// ratio that was hardcoded before).
	duration := float64(w.app.cfg.Appearance.ResizeOverlayDuration)
	if duration <= 0 {
		duration = 1.0
	}
	fadeStart := duration * (2.0 / 3.0)

	if elapsed > duration {
		w.resizeOverlay = false
		w.resizeOverlayText = ""
		return
	}

	cols, rows := w.gridSize()
	primary := fmt.Sprintf("%d × %d", cols, rows)
	secondary := w.resizeOverlayText // empty unless triggered by zoom
	primarySize := imgui.CalcTextSize(primary)
	var secondarySize imgui.Vec2
	if secondary != "" {
		secondarySize = imgui.CalcTextSize(secondary)
	}

	lineGap := primarySize.Y // ~one line of blank space between primary and secondary
	innerW := primarySize.X
	if secondarySize.X > innerW {
		innerW = secondarySize.X
	}
	innerH := primarySize.Y
	if secondary != "" {
		innerH += lineGap + secondarySize.Y
	}
	padX := float32(16)
	padY := float32(10)
	boxW := innerW + padX*2
	boxH := innerH + padY*2

	// Center on the window's viewport in absolute desktop space — under
	// multi-viewport the global foreground drawlist isn't tied to the
	// SDL window.
	var vpX, vpY float32
	if vp := w.viewport(); vp != nil {
		vpX, vpY = vp.Pos().X, vp.Pos().Y
	}
	cx := vpX + float32(w.width)/2
	cy := vpY + float32(w.height)/2

	// Fade out alpha
	alpha := float32(1.0)
	if elapsed > fadeStart {
		alpha = float32(1.0 - (elapsed-fadeStart)/(duration-fadeStart))
	}

	bgColor := uint32(uint8(alpha*180)) << 24 // semi-transparent black
	fgColor := uint32(0x00FFFFFF) | (uint32(uint8(alpha*255)) << 24)

	dl := w.fgDrawList()
	dl.AddRectFilledV(
		imgui.Vec2{X: cx - boxW/2, Y: cy - boxH/2},
		imgui.Vec2{X: cx + boxW/2, Y: cy + boxH/2},
		bgColor, 6, 0,
	)
	topY := cy - innerH/2
	if secondary != "" {
		// Zoom % rides above cols×rows; the dimensions remain the
		// dominant bottom line so users glancing at the overlay during
		// either drag-resize or zoom see the same anchor.
		dl.AddTextVec2(
			imgui.Vec2{X: cx - secondarySize.X/2, Y: topY},
			fgColor,
			secondary,
		)
		primaryY := topY + secondarySize.Y + lineGap
		dl.AddTextVec2(
			imgui.Vec2{X: cx - primarySize.X/2, Y: primaryY},
			fgColor,
			primary,
		)
	} else {
		dl.AddTextVec2(
			imgui.Vec2{X: cx - primarySize.X/2, Y: topY},
			fgColor,
			primary,
		)
	}
}

// renderReconnectingOverlay dims the viewport and draws a small
// "reconnecting…" badge when the active tab is a daemon-backed source
// whose connection dropped and is re-dialing (layer 4b). The frozen
// last frame stays visible underneath the dim so the user keeps context
// (what was on screen) while seeing the link is stalled. Pure daemon
// sources reconnect; in-process PTY tabs never match the assertion, so
// this is a no-op for them.
func (w *Window) renderReconnectingOverlay() {
	tab := w.tabs.Active()
	if tab == nil {
		return
	}
	ds, ok := tab.Terminal.(*daemonsource.Source)
	if !ok || !ds.IsReconnecting() {
		return
	}

	var vpX, vpY float32
	if vp := w.viewport(); vp != nil {
		vpX, vpY = vp.Pos().X, vp.Pos().Y
	}
	dl := w.fgDrawList()
	// Dim the whole content area so the frozen frame reads as inactive.
	dl.AddRectFilledV(
		imgui.Vec2{X: vpX, Y: vpY},
		imgui.Vec2{X: vpX + float32(w.width), Y: vpY + float32(w.height)},
		uint32(70)<<24, // ~27% black wash
		0, 0,
	)

	label := "reconnecting…"
	ts := imgui.CalcTextSize(label)
	padX, padY := float32(14), float32(8)
	boxW, boxH := ts.X+padX*2, ts.Y+padY*2
	cx := vpX + float32(w.width)/2
	cy := vpY + float32(w.height)/2
	dl.AddRectFilledV(
		imgui.Vec2{X: cx - boxW/2, Y: cy - boxH/2},
		imgui.Vec2{X: cx + boxW/2, Y: cy + boxH/2},
		uint32(0xCC)<<24, // mostly-opaque black pill
		6, 0,
	)
	dl.AddTextVec2(
		imgui.Vec2{X: cx - ts.X/2, Y: cy - ts.Y/2},
		0xFFFFFFFF,
		label,
	)
	// Keep the loop awake so the badge stays painted while idle (the
	// daemon sends no frames during a drop, so nothing else wakes us).
	if w.app.idleWakeMs > 250 {
		w.app.idleWakeMs = 250
	}
}

func (w *Window) drawScrollbar(tab *tabs.Tab, scrollOff int, drawList *imgui.DrawList) {
	vis := w.app.cfg.Scrollbar.Visible
	if vis == "never" {
		return
	}

	sbLen := tab.Terminal.ScrollbackLen()
	_, rows := w.gridSize()
	totalLines := sbLen + rows

	// auto mode: only show when scrolled back
	if vis == "auto" && scrollOff == 0 {
		return
	}

	barW := float32(w.app.cfg.Scrollbar.Width)
	vpOffX, vpOffY := w.contentOriginX, w.contentOriginY
	barX := vpOffX + float32(w.width) - barW
	barY := vpOffY + w.tabBarH
	termH := float32(w.height) - w.tabBarH // full height below tab bar

	// Check if mouse is hovering the thumb
	mpos := imgui.MousePos()
	hovered := mpos.X >= barX && mpos.X <= barX+barW && mpos.Y >= barY && mpos.Y <= barY+termH

	thumbY, thumbH := w.renderer.DrawScrollbar(renderer.ScrollbarParams{
		X:              barX,
		Y:              barY,
		Width:          barW,
		Height:         termH,
		ScrollOffset:   scrollOff,
		TotalLines:     totalLines,
		VisibleLines:   rows,
		MinThumbHeight: float32(w.app.cfg.Scrollbar.MinThumbHeight),
		Hovered:        hovered,
	}, drawList)

	// Handle scrollbar click-drag. hasOSFocus: same cross-window
	// click-leak guard as selection/tab-bar — a click that landed on
	// another window must not page-jump this one's scrollbar.
	if imgui.IsMouseClickedBool(0) && hovered && w.hasOSFocus() {
		if mpos.Y >= thumbY && mpos.Y <= thumbY+thumbH {
			// Click ON the thumb — start drag
			w.sbDragging = true
		} else if mpos.Y < thumbY {
			// Click above thumb: page up
			if s, ok := w.scroll[tab.ID]; ok {
				s.PageUp(rows, sbLen)
			}
		} else if mpos.Y > thumbY+thumbH {
			// Click below thumb: page down
			if s, ok := w.scroll[tab.ID]; ok {
				s.PageDown(rows)
			}
		}
	}

	// End scrollbar drag on mouse release
	if !imgui.IsMouseDown(0) {
		w.sbDragging = false
	}

	// Drag thumb: map mouse Y to scroll offset.
	// Once dragging starts, track Y regardless of X (user may drift sideways).
	if w.sbDragging {
		trackSpace := termH - thumbH
		if trackSpace > 0 {
			frac := 1.0 - (mpos.Y-barY-thumbH/2)/trackSpace
			if frac < 0 {
				frac = 0
			}
			if frac > 1 {
				frac = 1
			}
			maxOff := sbLen
			newOff := int(frac * float32(maxOff))
			if s, ok := w.scroll[tab.ID]; ok {
				s.Offset = newOff
			}
		}
	}
}

func (w *Window) pasteText(text string) {
	tab := w.tabs.Active()
	if tab == nil {
		return
	}
	cfg := w.app.cfg.Clipboard.UnsafePaste
	if !cfg.Enabled {
		tab.Terminal.Paste(text)
		return
	}

	shouldWarn := false
	if cfg.MultilineWarning && strings.Contains(text, "\n") {
		shouldWarn = true
	}
	if !shouldWarn && cfg.NewlineGuard && (strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\r\n")) {
		shouldWarn = true
	}

	if !shouldWarn && len(cfg.Patterns) > 0 {
		for _, pattern := range cfg.Patterns {
			m, err := regexp.MatchString(pattern, text)
			if err != nil {
				// Treat invalid regex as unsafe so a broken pattern in
				// preferences fails closed instead of silently disabling
				// the guard.
				shouldWarn = true
				break
			}
			if m {
				shouldWarn = true
				break
			}
		}
	}

	if shouldWarn {
		w.pendingPaste = text
		imgui.OpenPopupStr("Unsafe Paste")
		return
	}
	tab.Terminal.Paste(text)
}

func (w *Window) renderPasteDialog() {
	if w.pendingPaste == "" {
		return
	}

	// Dim THIS Window's content rect only. ImGui's BeginPopupModalV
	// would auto-dim every OTHER viewport too (hardcoded in
	// imgui.cpp's RenderDimmedBackgrounds — "Draw dimming background
	// on _other_ viewports than the ones our windows are in"), which
	// made the user's peer terminals look greyed-out even though
	// they're fully usable. Use a regular BeginV with our own dim
	// quad scoped to this Window's wrapper drawlist instead.
	dimCol := uint32(0x80000000) // 50% black, ABGR
	w.bgDrawList().AddRectFilled(
		imgui.Vec2{X: w.contentOriginX, Y: w.contentOriginY},
		imgui.Vec2{X: w.contentOriginX + float32(w.width), Y: w.contentOriginY + float32(w.height)},
		dimCol,
	)

	// Center the dialog in the OWNING Window's content rect in
	// absolute desktop coords. Pin to this Window's viewport so it
	// doesn't pop out into its own OS window for a tiny dialog.
	center := imgui.Vec2{X: w.contentOriginX + float32(w.width)/2, Y: w.contentOriginY + float32(w.height)/2}
	if vp := w.viewport(); vp != nil {
		imgui.SetNextWindowViewport(vp.ID())
	}
	imgui.SetNextWindowPosV(center, imgui.CondAppearing, imgui.Vec2{X: 0.5, Y: 0.5})

	flags := imgui.WindowFlagsAlwaysAutoResize |
		imgui.WindowFlagsNoCollapse |
		imgui.WindowFlagsNoSavedSettings |
		imgui.WindowFlagsNoDocking
	if imgui.BeginV("Unsafe Paste###pastedlg"+w.imguiSuffix(), nil, flags) {
		lines := strings.Count(w.pendingPaste, "\n") + 1
		imgui.Text(fmt.Sprintf("Paste %d lines into terminal?", lines))
		imgui.Text("Multi-line paste may execute commands.")
		imgui.TextDisabled("Enter = paste   Esc = cancel")
		imgui.Separator()

		// Keyboard shortcuts: Enter / KeypadEnter = accept (paste),
		// Esc = cancel. Input gating happens in processKeys via
		// popupActive() (returns true when pendingPaste != ""), so
		// these IsKeyPressedBool calls catch the keys before they
		// reach the PTY. The button row is the authoritative path;
		// this just makes the common case fast.
		accept := imgui.Button("Paste")
		imgui.SameLineV(0, 8)
		cancel := imgui.Button("Cancel")
		if imgui.IsKeyPressedBool(imgui.KeyEnter) || imgui.IsKeyPressedBool(imgui.KeyKeypadEnter) {
			accept = true
		}
		if imgui.IsKeyPressedBool(imgui.KeyEscape) {
			cancel = true
		}
		if accept {
			if tab := w.tabs.Active(); tab != nil {
				tab.Terminal.Paste(w.pendingPaste)
			}
			w.pendingPaste = ""
		} else if cancel {
			w.pendingPaste = ""
		}
	}
	imgui.End()
}

func (w *Window) renderRenameDialog() {
	if !w.renamingTab {
		return
	}

	// Dim THIS Window only — same reason as renderPasteDialog.
	dimCol := uint32(0x80000000)
	w.bgDrawList().AddRectFilled(
		imgui.Vec2{X: w.contentOriginX, Y: w.contentOriginY},
		imgui.Vec2{X: w.contentOriginX + float32(w.width), Y: w.contentOriginY + float32(w.height)},
		dimCol,
	)

	if vp := w.viewport(); vp != nil {
		imgui.SetNextWindowViewport(vp.ID())
	}
	center := imgui.Vec2{X: w.contentOriginX + float32(w.width)/2, Y: w.contentOriginY + float32(w.height)/2}
	imgui.SetNextWindowPosV(center, imgui.CondAppearing, imgui.Vec2{X: 0.5, Y: 0.5})

	flags := imgui.WindowFlagsAlwaysAutoResize |
		imgui.WindowFlagsNoCollapse |
		imgui.WindowFlagsNoSavedSettings |
		imgui.WindowFlagsNoDocking
	if imgui.BeginV("Rename Tab###renamedlg"+w.imguiSuffix(), nil, flags) {
		imgui.Text("Tab name:")
		imgui.InputTextWithHint("##rename", "tab name", &w.renameBuffer, 0, nil)

		if imgui.IsItemFocused() && imgui.IsKeyPressedBool(imgui.KeyEnter) {
			if tab := w.tabs.Active(); tab != nil {
				tab.SetTitle(w.renameBuffer)
			}
			w.renamingTab = false
		}

		if imgui.Button("OK") {
			if tab := w.tabs.Active(); tab != nil {
				tab.SetTitle(w.renameBuffer)
			}
			w.renamingTab = false
		}
		imgui.SameLineV(0, 8)
		if imgui.Button("Cancel") {
			w.renamingTab = false
		}
	}
	imgui.End()
}

// openConnectDialog shows the ad-hoc "Connect to host…" prompt. The
// buffers persist between opens so a typo'd dest is still there to
// fix; only the stale error is cleared.
func (w *Window) openConnectDialog() {
	w.connectingHost = true
	w.connectFocus = true
	w.connectError = ""
	w.app.active = w
}

// renderConnectDialog draws the ad-hoc remote-connect prompt. Mirrors
// renderRenameDialog: a single dimmed, auto-resizing modal centered on
// this Window, Enter-to-submit from the destination field.
func (w *Window) renderConnectDialog() {
	if !w.connectingHost {
		return
	}

	// Dim THIS Window only — same as the rename / paste dialogs.
	dimCol := uint32(0x80000000)
	w.bgDrawList().AddRectFilled(
		imgui.Vec2{X: w.contentOriginX, Y: w.contentOriginY},
		imgui.Vec2{X: w.contentOriginX + float32(w.width), Y: w.contentOriginY + float32(w.height)},
		dimCol,
	)

	if vp := w.viewport(); vp != nil {
		imgui.SetNextWindowViewport(vp.ID())
	}
	center := imgui.Vec2{X: w.contentOriginX + float32(w.width)/2, Y: w.contentOriginY + float32(w.height)/2}
	imgui.SetNextWindowPosV(center, imgui.CondAppearing, imgui.Vec2{X: 0.5, Y: 0.5})

	flags := imgui.WindowFlagsAlwaysAutoResize |
		imgui.WindowFlagsNoCollapse |
		imgui.WindowFlagsNoSavedSettings |
		imgui.WindowFlagsNoDocking
	if imgui.BeginV("Connect to host###connectdlg"+w.imguiSuffix(), nil, flags) {
		imgui.Text("SSH destination (user@host, host, or ~/.ssh/config alias):")
		submit := false
		// Land keyboard focus in the dest field when the dialog opens so
		// the user can type immediately + Enter submits. Guard on
		// !IsMouseDown so a menu-click that opened the dialog doesn't get
		// its ActiveId snatched mid-click (same pattern as the search
		// overlay).
		if w.connectFocus && !imgui.IsMouseDown(0) {
			imgui.SetKeyboardFocusHere()
			w.connectFocus = false
		}
		imgui.InputTextWithHint("##connectdest", "user@host", &w.connectBuffer, 0, nil)
		if imgui.IsItemFocused() && imgui.IsKeyPressedBool(imgui.KeyEnter) {
			submit = true
		}
		imgui.Text("Extra ssh options (optional):")
		imgui.InputTextWithHint("##connectargs", "-p 2222 -i ~/.ssh/key", &w.connectArgsBuffer, 0, nil)
		if imgui.IsItemFocused() && imgui.IsKeyPressedBool(imgui.KeyEnter) {
			submit = true
		}

		if w.connectError != "" {
			imgui.Text("Error: " + w.connectError)
		}

		if imgui.Button("Connect") {
			submit = true
		}
		imgui.SameLineV(0, 8)
		if imgui.Button("Cancel") {
			w.connectingHost = false
			w.connectError = ""
		}

		if submit {
			w.doConnect()
		}
	}
	imgui.End()
}

// doConnect dials the typed ad-hoc destination, registers it as a
// session host so re-dials/reattach can find it, and reattaches the
// host's existing tabs (or opens a fresh one if it has none).
// On failure it keeps the dialog open with the error so the user can
// correct the input.
func (w *Window) doConnect() {
	dest := strings.TrimSpace(w.connectBuffer)
	if dest == "" {
		w.connectError = "destination is required"
		return
	}
	host := config.RemoteHost{
		Name:    dest,
		SSHDest: normalizeSSHDest(dest),
		SSHArgs: strings.Fields(w.connectArgsBuffer),
	}
	if w.app.adhocHosts == nil {
		w.app.adhocHosts = make(map[string]config.RemoteHost)
	}
	w.app.adhocHosts[dest] = host

	// Reattach rather than always opening a fresh tab: the remote
	// daemon keeps PTYs alive across disconnects, so connecting should
	// restore whatever was already running there (e.g. a long-lived
	// shell or editor). openRemoteReattach falls back to a new tab when
	// the host has nothing to reattach to.
	if err := w.openRemoteReattach(dest); err != nil {
		// Drop the half-registered entry so a later retry with
		// corrected args isn't shadowed by this failed attempt.
		delete(w.app.adhocHosts, dest)
		w.connectError = err.Error()
		return
	}
	w.connectingHost = false
	w.connectError = ""
}

func (w *Window) handleMouseSelection() {
	tab := w.tabs.Active()
	if tab == nil {
		return
	}

	mousePos := imgui.MousePos()
	col := int((mousePos.X - w.renderer.OffsetX) / w.cellW)
	row := int((mousePos.Y - w.renderer.OffsetY) / w.cellH)
	// Unclamped row, kept for drag auto-scroll: when the user drags a
	// selection past the top/bottom edge, rawRow goes negative / past
	// rows and we scroll the view to extend the selection beyond one
	// screen (row itself is clamped to the grid just below).
	rawRow := row

	// Window-local pixel coordinates. ImGui draw lists and MousePos() are in
	// absolute desktop space when multi-viewport is enabled, but w.width /
	// w.height / w.tabBarH / w.searchOverlayW are window-local — subtract
	// the wrapper's content origin to bring them into the same space.
	vpOffX, vpOffY := w.contentOriginX, w.contentOriginY
	wmX := mousePos.X - vpOffX
	wmY := mousePos.Y - vpOffY

	// Clamp to grid bounds
	cols, rows := w.gridSize()
	if col < 0 {
		col = 0
	}
	if col >= cols {
		col = cols - 1
	}
	if row < 0 {
		row = 0
	}
	if row >= rows {
		row = rows - 1
	}

	// Left click starts selection — but only in the terminal area.
	// Explicit rect checks skip the scrollbar column and the search
	// overlay region. The "any other ImGui window is on top" filter
	// used to be CurrentIO().WantCaptureMouse(), but post-multi-window
	// the terminal renders INSIDE the wrapper ImGui window so
	// WantCaptureMouse is ALWAYS true over the terminal — that made
	// selection silently no-op everywhere.
	//
	// IsWindowHovered() (no flags) is the right primitive: returns
	// true only when the wrapper itself is the topmost hovered window
	// — false when a popup or modal (rename, unsafe-paste, the
	// context menu) is over the wrapper, but true when the mouse is
	// over plain terminal cells with nothing on top.
	barW := float32(w.app.cfg.Scrollbar.Width)
	onScrollbar := wmX >= float32(w.width)-barW
	onSearch := tab != nil && w.getScroll(tab.ID).Searching &&
		wmX >= float32(w.width)-w.searchOverlayW &&
		wmY <= w.tabBarH+65
	// A click-DOWN is only ours if this OS window actually has input
	// focus. On mac multi-viewport ImGui keeps reporting the wrapper
	// hovered after focus jumps to another window (no mouse-leave is
	// delivered), so without this gate a click on a DIFFERENT window
	// leaks in as a selection start / triple-click here. An in-flight
	// drag (w.sel.dragging) is exempt below so a drag that began here
	// still tracks even if the focus probe flickers mid-drag.
	wrapperHovered := imgui.IsWindowHovered() && w.hasOSFocus()
	inTerminal := wrapperHovered && wmY >= w.tabBarH && !onScrollbar && !onSearch && !w.sbDragging

	if imgui.IsMouseDoubleClicked(imgui.MouseButtonLeft) && inTerminal {
		// Links.DoubleClick: open the URL under the cursor on
		// double-click. Wins over the word-select default — the user
		// who turned this on wanted clicks on links to navigate, not
		// select. Plain double-click on non-link text falls through
		// to the word/whitespace selection paths.
		if w.hoveredLink != nil && w.app.cfg.Links.Enabled && w.app.cfg.Links.DoubleClick {
			openURL(w.hoveredLink.URL, w.app.cfg.Links.Opener)
			w.lastDblClickTime = imgui.Time()
			w.lastDblClickRow = row
			w.lastDblClickCol = col
			return
		}
		scrollOff := 0
		if s, ok := w.scroll[tab.ID]; ok {
			scrollOff = s.Offset
		}
		// Selection lives in content rows (stable under scrolling) —
		// convert from the viewport row once, here at the mouse event.
		cRow := contentRowAt(tab.Terminal, row, scrollOff)
		cell := cellAtContent(tab.Terminal, col, cRow)
		if cell != nil && isSelWordChar(cell.Content) {
			w.sel.selectWord(tab.Terminal, col, cRow)
		} else {
			w.sel.selectSpace(tab.Terminal, col, cRow)
		}
		if w.sel.active {
			// iTerm2-style: hold-and-drag after a double-click extends
			// the selection by word, with the original word as the
			// anchor. Release without movement just keeps the word
			// selection.
			w.sel.dragging = true
			w.writeSelection(w.sel.extractText(tab.Terminal, w.app.cfg.Clipboard.TrimTrailingWhitespace))
		}
		w.lastDblClickTime = imgui.Time()
		w.lastDblClickRow = row
		w.lastDblClickCol = col
	} else if imgui.IsMouseClickedBool(imgui.MouseButtonLeft) {
		// Triple-click detection: click shortly after a double-click on the same row
		if inTerminal && imgui.Time()-w.lastDblClickTime < 0.4 && row == w.lastDblClickRow {
			scrollOff := 0
			if s, ok := w.scroll[tab.ID]; ok {
				scrollOff = s.Offset
			}
			w.sel.selectLine(tab.Terminal, contentRowAt(tab.Terminal, row, scrollOff))
			if w.sel.active {
				// Drag after triple-click extends the selection by full rows.
				w.sel.dragging = true
				w.writeSelection(w.sel.extractText(tab.Terminal, w.app.cfg.Clipboard.TrimTrailingWhitespace))
			}
			w.lastDblClickTime = 0 // consumed
		} else if inTerminal {
			scrollOff := 0
			if s, ok := w.scroll[tab.ID]; ok {
				scrollOff = s.Offset
			}
			w.sel.clear()
			w.sel.startCharDrag(contentRowAt(tab.Terminal, row, scrollOff), col)
		}
	}

	// If this window lost OS focus mid-drag, the button-up went to
	// ANOTHER window and ImGui never delivered it here — so dragging
	// stays true and IsMouseDown stays stuck true, while MousePos is
	// now off in the other window. Left unchecked, the edge
	// auto-scroll below reads a wildly out-of-range rawRow and scrolls
	// the terminal away on its own (it even PostWakes itself to keep
	// going). End the drag when focus leaves: finalize the selection
	// we have and stop tracking. (A real in-window drag keeps focus,
	// so this only fires on the cross-window click-away.)
	if w.sel.dragging && !w.hasOSFocus() {
		w.sel.dragging = false
		if w.sel.active {
			w.writeSelection(w.sel.extractText(tab.Terminal, w.app.cfg.Clipboard.TrimTrailingWhitespace))
		}
	}

	// Dragging extends selection. Mode (set when the drag started)
	// decides whether the moving end snaps to char / word / line.
	if w.sel.dragging && imgui.IsMouseDown(imgui.MouseButtonLeft) {
		s := w.getScroll(tab.ID)
		// Auto-scroll while dragging past an edge so a selection can
		// span more than one screen. TIME-based (rows/second via
		// cfg.Scrollback.DragScrollSpeed, fractional rows accumulate
		// across frames) — the first cut scrolled N rows per FRAME,
		// which tied the speed to the render rate (~240+ rows/sec at
		// the frame cap: way too fast, and faster on faster machines).
		// Speed ramps up to 4x as the cursor pulls further past the
		// edge. row is already clamped to the visible grid, so
		// extendDrag below anchors the moving end to the new
		// top/bottom row after the scroll.
		dist := 0
		if rawRow < 0 {
			dist = -rawRow
		} else if rawRow >= rows {
			dist = rawRow - rows + 1
		}
		if dist > 0 {
			base := float64(w.app.cfg.Scrollback.DragScrollSpeed)
			if base <= 0 {
				base = 25
			}
			mult := 1.0 + float64(dist-1)*0.25
			if mult > 4 {
				mult = 4
			}
			dt := float64(imgui.CurrentIO().DeltaTime())
			if dt > 0.1 {
				dt = 0.1 // a stalled frame must not teleport the view
			}
			w.dragScrollAccum += base * mult * dt
			if n := int(w.dragScrollAccum); n > 0 {
				w.dragScrollAccum -= float64(n)
				if rawRow < 0 {
					s.ScrollUp(n, tab.Terminal.ScrollbackLen())
				} else {
					s.ScrollDown(n)
				}
			}
		} else {
			w.dragScrollAccum = 0
		}
		// Converting at the CURRENT offset means dragging while
		// scrolling extends the selection through the content that
		// scrolls past — the anchor stays glued to its text.
		w.sel.extendDrag(contentRowAt(tab.Terminal, row, s.Offset), col, tab.Terminal)
		// Keep frames flowing while the cursor is parked past the edge
		// (no mouse motion events arrive when it's held still), so the
		// auto-scroll continues every frame instead of stalling.
		if rawRow < 0 || rawRow >= rows {
			platform.PostWake()
		}
	}

	// Release finalizes selection and copies to PRIMARY (+ CLIPBOARD
	// when copy_on_select is set).
	if w.sel.dragging && imgui.IsMouseReleased(imgui.MouseButtonLeft) {
		w.sel.dragging = false
		if w.sel.active {
			w.writeSelection(w.sel.extractText(tab.Terminal, w.app.cfg.Clipboard.TrimTrailingWhitespace))
		}
	}

	// Middle-click pastes from PRIMARY selection (terminal area only, not on
	// ImGui windows like prefs/search). Gated on Clipboard.PasteOnMiddleClick
	// — when off, middle-click is a no-op (some users rebind their X11
	// middle button or don't want accidental pastes from the primary
	// selection while reading).
	if w.app.cfg.Clipboard.PasteOnMiddleClick && imgui.IsMouseClickedBool(imgui.MouseButtonMiddle) {
		if wmY >= w.tabBarH && wrapperHovered {
			text, err := input.PrimaryRead()
			if err == nil && text != "" {
				w.pasteText(text)
			}
		}
	}
}

// writeSelection publishes selected text to the X11/Wayland PRIMARY
// selection (the middle-click target — Linux convention is to refresh
// it on every selection regardless of preference) and, when
// cfg.Clipboard.CopyOnSelect is set, also to the system clipboard
// (Ctrl+C target). The per-row cell-grid trim is done by
// extractText() (gated on the same pref); this function only handles
// the publish.
//
// Empty / whitespace-only text is dropped: writing it would clobber
// whatever the user previously copied without giving them anything
// useful in return.
func (w *Window) writeSelection(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	input.PrimaryWrite(text)
	if w.app.cfg.Clipboard.CopyOnSelect {
		input.ClipboardWrite(text)
		// Daemons need to know about copy-on-select too so MCP
		// get_clipboard / future PTY OSC52 reads see the
		// freshly-selected text. Without this, copy-on-select
		// users had a clipboard divergence between OS and daemon.
		w.app.broadcastClipboard(text)
	}
}

func (w *Window) selectedText() string {
	if !w.sel.active {
		return ""
	}
	tab := w.tabs.Active()
	if tab == nil {
		return ""
	}
	return w.sel.extractText(tab.Terminal, w.app.cfg.Clipboard.TrimTrailingWhitespace)
}

func getCWD(term interface{}) string {
	t, ok := term.(*terminal.Terminal)
	if !ok {
		return ""
	}
	return t.GetCWD()
}
