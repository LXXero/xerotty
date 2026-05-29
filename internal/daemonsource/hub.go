// Package daemonsource implements terminal.Source backed by a
// connection to xerottyd. Multiple tabs can share one Hub (which
// owns the network connection); each tab gets its own *Source that
// satisfies terminal.Source so the GUI can mix daemon-backed and
// PTY-backed tabs in the same window without caring which is which.
//
// Layout:
//
//	Hub
//	 ├── *clientproto.Client            (shared connection)
//	 ├── sources map[tabID]*Source       (registered tabs)
//	 └── one router goroutine            (demuxes incoming frames)
//
// The router reads from the shared clientproto channels, looks up
// the target Source by tab ID, and pushes the frame into that
// Source's local handler. Source's local handler in turn updates the
// shadow *vt.SafeEmulator that the GUI renders from.
package daemonsource

import (
	"fmt"
	"sync"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/protocol"
)

// Hub owns the daemon connection and demuxes incoming frames to
// per-tab Source instances. A GUI process needs ONE Hub for each
// daemon it talks to (typically just one: the local daemon).
type Hub struct {
	c *clientproto.Client

	// scrollbackCap is the per-Source scrollback ring cap. Read by
	// newSource. 0 = use the daemonsource default (10000). Set via
	// SetScrollbackCap before creating sources; runtime changes
	// only affect future Source instances.
	scrollbackCap int

	mu      sync.RWMutex
	sources map[uint32]*Source

	// onClipboardSet fires when the daemon ships MsgClipboardSet
	// (a remote PTY app wrote the clipboard). Clipboard is
	// session-global so this is a hub-level callback, not per-tab.
	// The GUI sets it to write the local OS clipboard.
	onClipboardSet func(string)

	// onProposals fires when the daemon ships MsgProposalsChanged
	// (propose-mode queue updated). Hub-level (session-global).
	// The GUI renders an approval gate from the list.
	onProposals func([]protocol.ProposalInfo)

	// pending buffers frames addressed to a tab ID we haven't yet
	// registered a Source for. Without this we'd race the daemon's
	// initial CellFull / TabState emission against our own
	// Hub.Adopt call (the daemon starts publishing the instant
	// TabCreate processes; the client only gets to Adopt after
	// TabCreated rounds back). Bounded per tab to keep a runaway
	// daemon from eating memory.
	pending map[uint32][]pendingFrame

	// Default WindowID to create new tabs in. 0 = daemon's default
	// window. The GUI can override per-tab via NewTabInWindow.
	defaultWindowID uint32

	stopCh chan struct{}
}

// pendingFrame is one buffered frame waiting for its Source to
// register. Tag identifies which apply* method to invoke when the
// Source shows up.
type pendingFrame struct {
	tag pendingTag
	raw interface{}
}

type pendingTag uint8

const (
	pendingCellFull pendingTag = iota
	pendingCellDiff
	pendingCursor
	pendingTitle
	pendingBell
	pendingChildExit
	pendingTabState
	pendingScrollbackAppend
	pendingScrollbackCleared
)

// pendingCap limits per-tab buffering. 64 covers the typical initial
// burst (CellFull, Cursor, TabState, plus a few diffs while the
// shell prints its prompt). Beyond this we silently drop the oldest
// — better than unbounded growth for a tab the client may never
// Adopt.
const pendingCap = 64

// NewHub wraps a connected, hello'd, attached clientproto.Client.
// Caller is responsible for calling c.Hello + c.Attach before
// constructing the Hub. NewHub starts a router goroutine; call
// Stop to tear it down.
func NewHub(c *clientproto.Client) *Hub {
	h := &Hub{
		c:       c,
		sources: make(map[uint32]*Source),
		pending: make(map[uint32][]pendingFrame),
		stopCh:  make(chan struct{}),
	}
	go h.router()
	return h
}

// Stop signals the router goroutine to exit. The underlying
// clientproto.Client is not closed — callers own its lifecycle.
func (h *Hub) Stop() { close(h.stopCh) }

// SetClipboardSetCallback registers the hub-level handler fired
// when the daemon ships MsgClipboardSet (a remote app wrote the
// clipboard via OSC 52). The GUI uses it to write the local OS
// clipboard. Last writer wins; all sources on the hub share it.
func (h *Hub) SetClipboardSetCallback(fn func(string)) {
	h.mu.Lock()
	h.onClipboardSet = fn
	h.mu.Unlock()
}

// SetProposalsCallback registers the hub-level handler fired when
// the daemon ships the propose-mode queue. The GUI renders its
// approval banner from the list.
func (h *Hub) SetProposalsCallback(fn func([]protocol.ProposalInfo)) {
	h.mu.Lock()
	h.onProposals = fn
	h.mu.Unlock()
}

// ResolveProposal approves/drops a pending proposal by index over
// this hub's connection.
func (h *Hub) ResolveProposal(index uint32, approve bool) error {
	return h.c.SendProposalResolve(index, approve)
}

// Sources returns a snapshot of all currently-registered Sources
// on this hub. Used by the GUI's aggregating MCP server to
// enumerate tabs across every daemon it's connected to.
func (h *Hub) Sources() []*Source {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Source, 0, len(h.sources))
	for _, s := range h.sources {
		out = append(out, s)
	}
	return out
}

// SetScrollbackCap controls how many rows of scrollback each new
// Source will mirror locally. Higher = more memory, more user
// history. 0 means "use the default" (10000). Sources created
// before this call keep their existing cap.
func (h *Hub) SetScrollbackCap(rows int) {
	h.mu.Lock()
	h.scrollbackCap = rows
	h.mu.Unlock()
}

// Client returns the underlying clientproto.Client. Useful for
// pushing one-off frames (e.g. clipboard sync) that aren't per-tab.
func (h *Hub) Client() *clientproto.Client { return h.c }

// register adds a Source to the routing table and drains any
// buffered frames that arrived for this tab ID before the Source
// existed. Called by Source's own constructor. Reverse via
// unregister().
func (h *Hub) register(s *Source) {
	h.mu.Lock()
	h.sources[s.tabID] = s
	queued := h.pending[s.tabID]
	delete(h.pending, s.tabID)
	h.mu.Unlock()

	// Replay in order. Sources expect frames in arrival order
	// (CellFull before any CellDiff for the same paint cycle).
	for _, pf := range queued {
		switch pf.tag {
		case pendingCellFull:
			s.applyCellFull(pf.raw.(*protocol.CellFull))
		case pendingCellDiff:
			s.applyCellDiff(pf.raw.(*protocol.CellDiff))
		case pendingCursor:
			s.applyCursor(pf.raw.(*protocol.Cursor))
		case pendingTitle:
			s.applyTitle(pf.raw.(*protocol.Title))
		case pendingBell:
			s.applyBell(pf.raw.(*protocol.Bell))
		case pendingChildExit:
			s.applyChildExit(pf.raw.(*protocol.ChildExit))
		case pendingTabState:
			s.applyTabState(pf.raw.(*protocol.TabState))
		case pendingScrollbackAppend:
			s.applyScrollbackAppend(pf.raw.(*protocol.ScrollbackAppend))
		case pendingScrollbackCleared:
			s.applyScrollbackCleared(pf.raw.(*protocol.ScrollbackCleared))
		}
	}
}

// stash queues a frame for a tab ID that has no Source yet. Drops
// oldest entries if the queue would exceed pendingCap.
func (h *Hub) stash(id uint32, tag pendingTag, raw interface{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	q := h.pending[id]
	if len(q) >= pendingCap {
		q = q[1:]
	}
	q = append(q, pendingFrame{tag: tag, raw: raw})
	h.pending[id] = q
}

func (h *Hub) unregister(id uint32) {
	h.mu.Lock()
	delete(h.sources, id)
	h.mu.Unlock()
}

func (h *Hub) lookup(id uint32) *Source {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sources[id]
}

// SetDefaultWindowID picks which daemon-side window new tabs land
// in when NewTab is called without an explicit window. The GUI
// usually creates one daemon-side window per UI window at startup
// and stores its ID here.
func (h *Hub) SetDefaultWindowID(id uint32) {
	h.mu.Lock()
	h.defaultWindowID = id
	h.mu.Unlock()
}

// NewTab requests a fresh tab on the daemon's default window (set
// via SetDefaultWindowID, 0 = daemon's own default). Convenience
// wrapper around NewTabIn. Avoid in multi-window setups where each
// GUI window needs its own daemon window — use NewTabIn there so
// concurrent window-A and window-B tab creates don't race for the
// global default.
func (h *Hub) NewTab(cols, rows int, cwd string) (*Source, error) {
	h.mu.RLock()
	winID := h.defaultWindowID
	h.mu.RUnlock()
	return h.NewTabIn(winID, cols, rows, cwd)
}

// NewTabIn requests a fresh tab on the named daemon window. Each
// GUI Window's tabs.Manager.SourceFactory closes over its own
// daemonWindowID and calls this — that way "new tab in window A"
// is always routed to daemon window A regardless of what some
// other GUI window most recently did.
//
// windowID = 0 means "daemon's default window" (typically the
// first window the daemon created).
func (h *Hub) NewTabIn(windowID uint32, cols, rows int, cwd string) (*Source, error) {
	if err := h.c.SendTabCreate(windowID, uint16(cols), uint16(rows), cwd, ""); err != nil {
		return nil, fmt.Errorf("daemonsource: SendTabCreate: %w", err)
	}
	// TabCreated should arrive on the client's channel; wait for it.
	// Multiple in-flight TabCreates can race — pick the first one.
	// Bail if the connection dies (Closed) or the hub shuts down
	// (stopCh) so a dropped daemon doesn't wedge the caller forever.
	select {
	case tc, ok := <-h.c.TabCreated():
		if !ok {
			return nil, fmt.Errorf("daemonsource: client closed before TabCreated")
		}
		return h.Adopt(tc.Info.ID, cols, rows), nil
	case <-h.c.Closed():
		return nil, fmt.Errorf("daemonsource: connection closed before TabCreated")
	case <-h.stopCh:
		return nil, fmt.Errorf("daemonsource: hub stopped before TabCreated")
	}
}

// Adopt creates a Source for a tab that already exists on the daemon
// (e.g. one we got back in the initial Attached frame, or one
// another client created). Idempotent: calling twice with the same
// tabID returns the same Source.
func (h *Hub) Adopt(tabID uint32, cols, rows int) *Source {
	if s := h.lookup(tabID); s != nil {
		return s
	}
	s := newSource(h, tabID, cols, rows)
	h.register(s)
	return s
}

// router is the single goroutine that demuxes frames from the
// client's channels into per-Source state. One goroutine instead of
// per-Source goroutines means we never deadlock on a Source that
// stopped draining: incoming frames just back-pressure the router,
// which back-pressures the client.
func (h *Hub) router() {
	cli := h.c
	for {
		select {
		case <-h.stopCh:
			return
		case <-cli.Closed():
			return
		case f := <-cli.CellFull():
			if s := h.lookup(f.ID); s != nil {
				s.applyCellFull(f)
			} else {
				h.stash(f.ID, pendingCellFull, f)
			}
		case f := <-cli.CellDiff():
			if s := h.lookup(f.ID); s != nil {
				s.applyCellDiff(f)
			} else {
				h.stash(f.ID, pendingCellDiff, f)
			}
		case f := <-cli.Cursor():
			if s := h.lookup(f.ID); s != nil {
				s.applyCursor(f)
			} else {
				h.stash(f.ID, pendingCursor, f)
			}
		case f := <-cli.Title():
			if s := h.lookup(f.ID); s != nil {
				s.applyTitle(f)
			} else {
				h.stash(f.ID, pendingTitle, f)
			}
		case f := <-cli.Bell():
			if s := h.lookup(f.ID); s != nil {
				s.applyBell(f)
			} else {
				h.stash(f.ID, pendingBell, f)
			}
		case f := <-cli.ChildExit():
			if s := h.lookup(f.ID); s != nil {
				s.applyChildExit(f)
			} else {
				h.stash(f.ID, pendingChildExit, f)
			}
		case f := <-cli.TabState():
			if s := h.lookup(f.ID); s != nil {
				s.applyTabState(f)
			} else {
				h.stash(f.ID, pendingTabState, f)
			}
		case f := <-cli.ScrollbackAppend():
			if s := h.lookup(f.ID); s != nil {
				s.applyScrollbackAppend(f)
			} else {
				h.stash(f.ID, pendingScrollbackAppend, f)
			}
		case f := <-cli.ScrollbackCleared():
			if s := h.lookup(f.ID); s != nil {
				s.applyScrollbackCleared(f)
			} else {
				h.stash(f.ID, pendingScrollbackCleared, f)
			}
		case cs := <-cli.ClipboardSet():
			// Session-global clipboard write from a remote app's
			// OSC 52 set — fire the hub-level handler (writes the
			// local OS clipboard).
			h.mu.RLock()
			cb := h.onClipboardSet
			h.mu.RUnlock()
			if cb != nil {
				cb(cs.Text)
			}
		case pc := <-cli.ProposalsChanged():
			h.mu.RLock()
			cb := h.onProposals
			h.mu.RUnlock()
			if cb != nil {
				cb(pc.Proposals)
			}
		case err := <-cli.Errors():
			// Protocol errors aren't tied to a tab. Log to stderr
			// via the Source-free fallback so the user sees it.
			_ = err // TODO: route to a Hub-level error sink
		}
	}
}

// applyChildExit silences "unused" linter — keeps the import live.
var _ = protocol.MsgChildExit
