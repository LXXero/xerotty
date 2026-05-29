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
	"sync/atomic"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/protocol"
)

// tabCreateTimeout bounds how long NewTabIn waits for the daemon to
// confirm a tab. The closed-channel cases handle a dropped conn; this
// covers the conn-stays-open-but-daemon-never-answers case so a hung
// daemon can't wedge the GUI's tab-create indefinitely.
const tabCreateTimeout = 10 * time.Second

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

	// Request correlation for tab creates. nextReqID mints a unique
	// ID per NewTabIn; pendingCreates maps it to the waiter's reply
	// channel. The router delivers each MsgTabCreated to the matching
	// waiter (or drops it if none — a late ack after the waiter timed
	// out, or a tab another client created). This lets concurrent
	// creates coexist and stops a late ack from poisoning a newer one.
	nextReqID      atomic.Uint64
	createMu       sync.Mutex
	pendingCreates map[uint64]chan *protocol.TabCreated
	// pendingWindowCreates is the WindowCreate analogue — same ReqID
	// correlation (router-demuxed) so a late WindowCreated can't be
	// adopted as a newer create's reply, and so undrained acks can't
	// fill the client channel and wedge the read loop.
	pendingWindowCreates map[uint64]chan *protocol.WindowCreated

	// lastRevision is the highest topology revision applied so far,
	// seeded from Attached.Revision. Only the router goroutine touches
	// it (in applyTopology), so no lock needed beyond that serialization.
	lastRevision uint64

	// onTopology, if set, fires after the Hub reconciles a topology
	// snapshot — the GUI hooks it to refresh its window/tab view.
	// Guarded by mu.
	onTopology func(*protocol.TopologyChanged)

	// createTimeout bounds NewTabIn's wait for its ack. A field (not
	// the bare const) so tests can shorten it.
	createTimeout time.Duration

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
		c:              c,
		sources:        make(map[uint32]*Source),
		pending:        make(map[uint32][]pendingFrame),
		pendingCreates:       make(map[uint64]chan *protocol.TabCreated),
		pendingWindowCreates: make(map[uint64]chan *protocol.WindowCreated),
		createTimeout:        tabCreateTimeout,
		stopCh:               make(chan struct{}),
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
	// Mint a request ID and register a waiter channel BEFORE sending,
	// so the router can never deliver our ack before we're listening.
	reqID := h.nextReqID.Add(1)
	reply := make(chan *protocol.TabCreated, 1)
	h.createMu.Lock()
	h.pendingCreates[reqID] = reply
	h.createMu.Unlock()
	defer func() {
		h.createMu.Lock()
		delete(h.pendingCreates, reqID)
		h.createMu.Unlock()
	}()

	if err := h.c.SendTabCreateReq(windowID, uint16(cols), uint16(rows), cwd, "", reqID); err != nil {
		return nil, fmt.Errorf("daemonsource: SendTabCreate: %w", err)
	}
	// Wait for OUR ack (matched by ReqID in the router). Bail if the
	// connection dies, the hub shuts down, or the daemon never answers
	// — so a dropped/hung daemon can't wedge the caller forever, and a
	// late ack for a timed-out create can't bind us to the wrong tab.
	select {
	case tc := <-reply:
		// Adopt at the dims the daemon actually allocated (it clamps
		// to MaxTabDim), not what we requested — otherwise the local
		// emulator would be sized wrong and desync from the daemon.
		return h.Adopt(tc.Info.ID, int(tc.Info.Cols), int(tc.Info.Rows)), nil
	case <-h.c.Closed():
		return nil, fmt.Errorf("daemonsource: connection closed before TabCreated")
	case <-h.stopCh:
		return nil, fmt.Errorf("daemonsource: hub stopped before TabCreated")
	case <-time.After(h.createTimeout):
		// The conn is up but the daemon never confirmed — don't block
		// the GUI's tab-create forever.
		return nil, fmt.Errorf("daemonsource: timed out waiting for TabCreated")
	}
}

// SetTopologyCallback registers a hub-level handler fired after the
// Hub reconciles a topology snapshot (a tab/window was created/closed/
// moved by some client or MCP agent). The GUI uses it to refresh its
// window/tab view from the Hub's now-current Sources. Guarded by mu.
func (h *Hub) SetTopologyCallback(fn func(*protocol.TopologyChanged)) {
	h.mu.Lock()
	h.onTopology = fn
	h.mu.Unlock()
}

// SeedRevision sets the baseline topology revision (from the Attached
// frame) so subsequent MsgTopologyChanged frames are gated correctly.
// Call once, right after attach, before the router could deliver a
// broadcast. Safe to call from the attach goroutine.
func (h *Hub) SeedRevision(rev uint64) {
	h.mu.Lock()
	if rev > h.lastRevision {
		h.lastRevision = rev
	}
	h.mu.Unlock()
}

// deliverTabCreated routes one MsgTabCreated to its waiting NewTabIn
// by ReqID. Unmatched acks (a create that already timed out, or a tab
// another client created — handled by topology reconcile) are dropped.
func (h *Hub) deliverTabCreated(tc *protocol.TabCreated) {
	h.createMu.Lock()
	reply, ok := h.pendingCreates[tc.ReqID]
	if ok {
		delete(h.pendingCreates, tc.ReqID)
	}
	h.createMu.Unlock()
	if ok {
		select {
		case reply <- tc:
		default: // waiter already gave up; reply is cap-1 anyway
		}
	}
}

// CreateWindow registers a new daemon-side window and returns its
// assigned ID, correlating the reply by ReqID (so a late ack from a
// timed-out create can't be mistaken for this one's). Mirrors NewTabIn.
func (h *Hub) CreateWindow(posX, posY, width, height int32) (uint32, error) {
	reqID := h.nextReqID.Add(1)
	reply := make(chan *protocol.WindowCreated, 1)
	h.createMu.Lock()
	h.pendingWindowCreates[reqID] = reply
	h.createMu.Unlock()
	defer func() {
		h.createMu.Lock()
		delete(h.pendingWindowCreates, reqID)
		h.createMu.Unlock()
	}()

	if err := h.c.SendWindowCreateReq(posX, posY, width, height, reqID); err != nil {
		return 0, fmt.Errorf("daemonsource: SendWindowCreate: %w", err)
	}
	select {
	case wc := <-reply:
		return wc.Info.ID, nil
	case <-h.c.Closed():
		return 0, fmt.Errorf("daemonsource: connection closed before WindowCreated")
	case <-h.stopCh:
		return 0, fmt.Errorf("daemonsource: hub stopped before WindowCreated")
	case <-time.After(h.createTimeout):
		return 0, fmt.Errorf("daemonsource: timed out waiting for WindowCreated")
	}
}

// deliverWindowCreated routes one MsgWindowCreated to its waiting
// CreateWindow by ReqID; unmatched acks are dropped.
func (h *Hub) deliverWindowCreated(wc *protocol.WindowCreated) {
	h.createMu.Lock()
	reply, ok := h.pendingWindowCreates[wc.ReqID]
	if ok {
		delete(h.pendingWindowCreates, wc.ReqID)
	}
	h.createMu.Unlock()
	if ok {
		select {
		case reply <- wc:
		default:
		}
	}
}

// applyTopology reconciles a topology snapshot against the Hub's
// adopted Sources: adopt newly-appeared tab IDs, mark vanished ones
// gone, and gate on revision so a stale/duplicate snapshot can't roll
// the view backwards. Runs only on the router goroutine, so the
// revision gate needs no extra lock. Idempotent.
func (h *Hub) applyTopology(topo *protocol.TopologyChanged) {
	h.mu.Lock()
	if topo.Revision <= h.lastRevision {
		h.mu.Unlock()
		return
	}
	h.lastRevision = topo.Revision
	want := make(map[uint32]bool, len(topo.Tabs))
	for _, ti := range topo.Tabs {
		want[ti.ID] = true
	}
	var vanished []*Source
	for id, s := range h.sources {
		if !want[id] {
			vanished = append(vanished, s)
		}
	}
	cb := h.onTopology
	h.mu.Unlock()

	// Adopt new tabs (Adopt locks h.mu internally, so call it outside
	// the lock above). Idempotent for already-known IDs.
	for _, ti := range topo.Tabs {
		h.Adopt(ti.ID, int(ti.Cols), int(ti.Rows))
	}
	// Drop tabs that no longer exist on the daemon.
	for _, s := range vanished {
		s.markVanished()
	}
	if cb != nil {
		cb(topo)
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
		case tc := <-cli.TabCreated():
			// Correlated ack for a NewTabIn (or a stale/foreign one we
			// drop). Routed here — not consumed directly by NewTabIn —
			// so concurrent creates don't steal each other's acks.
			h.deliverTabCreated(tc)
		case wc := <-cli.WindowCreated():
			h.deliverWindowCreated(wc)
		case topo := <-cli.Topology():
			h.applyTopology(topo)
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
