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
	// conn is the live daemon connection. It's an atomic pointer, not a
	// plain field, because the reconnect loop SWAPS it for a fresh
	// client after a drop (layer 4b) while Source.Write / router / RPC
	// helpers read it concurrently. Every accessor goes through
	// client(); Sources reach it via s.hub.client() too, so a swap is
	// transparently picked up everywhere without re-pointing Sources.
	conn atomic.Pointer[clientproto.Client]

	// redial, if set, lets the Hub re-establish a dropped connection
	// itself instead of the router just exiting. It returns a fresh
	// client that has ALREADY completed Hello + Attach (with Run
	// started) plus the Attached snapshot to resync topology from. The
	// app injects it (local auto-spawn re-dial, or SSH re-dial). nil =
	// no self-healing (tests / transient hubs); the router exits on
	// disconnect as it did before layer 4b.
	redial func() (*clientproto.Client, *protocol.Attached, error)

	// scrollbackCap is the per-Source scrollback ring cap. Read by
	// newSource. 0 = use the daemonsource default (10000). Set via
	// SetScrollbackCap before creating sources; runtime changes
	// only affect future Source instances.
	scrollbackCap int

	mu      sync.RWMutex
	sources map[uint32]*Source

	// tombstones records tab IDs the local user CLOSED (Source.Close,
	// which means "kill the remote tab" — not Detach, which keeps it).
	// They're replayed on every topology snapshot so a close that didn't
	// reach the daemon (issued while the connection was down, or racing a
	// drop) isn't undone by the snapshot resync resurrecting the tab —
	// the documented Phase 10 pitfall. A tombstoned ID that's still
	// present in a snapshot gets a fresh MsgTabClose; once it's gone from
	// the snapshot (daemon confirmed the close) the tombstone is dropped.
	// Guarded by its own mutex so Source.Close (UI goroutine) and the
	// router's reconcile don't contend on h.mu.
	tombMu     sync.Mutex
	tombstones map[uint32]struct{}
	// daemonInstance is the daemon-process identity (Attached.InstanceID)
	// the tombstones are scoped to. On reconnect to a DIFFERENT instance
	// (a restarted daemon — fresh tab-id space starting back at 1), the
	// tombstones belonged to the dead daemon's IDs and must be dropped,
	// or a legitimately-recreated tab that reuses an ID would be
	// suppressed (and SendTabClose replayed) forever. Guarded by tombMu.
	daemonInstance string

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
		sources:        make(map[uint32]*Source),
		tombstones:     make(map[uint32]struct{}),
		pending:        make(map[uint32][]pendingFrame),
		pendingCreates:       make(map[uint64]chan *protocol.TabCreated),
		pendingWindowCreates: make(map[uint64]chan *protocol.WindowCreated),
		createTimeout:        tabCreateTimeout,
		stopCh:               make(chan struct{}),
	}
	h.conn.Store(c)
	go h.router()
	return h
}

// client returns the live daemon connection. Hot path (Source.Write
// reads it per keystroke), so it's a lock-free atomic load.
func (h *Hub) client() *clientproto.Client { return h.conn.Load() }

// SetRedial installs the reconnect dialer (see Hub.redial). Call once,
// right after NewHub, before a disconnect could happen. Without it the
// Hub behaves as before layer 4b: the router exits on disconnect.
func (h *Hub) SetRedial(fn func() (*clientproto.Client, *protocol.Attached, error)) {
	h.mu.Lock()
	h.redial = fn
	h.mu.Unlock()
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
	return h.client().SendProposalResolve(index, approve)
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
func (h *Hub) Client() *clientproto.Client { return h.client() }

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

// addTombstone records that the local user closed tab id (kill, not
// detach). Called from Source.Close. The next topology snapshot that
// still lists id will get a replayed MsgTabClose instead of resurrecting
// the tab; one that omits id clears the tombstone.
func (h *Hub) addTombstone(id uint32) {
	h.tombMu.Lock()
	h.tombstones[id] = struct{}{}
	h.tombMu.Unlock()
}

// SeedInstance records the daemon's instance identity at initial attach
// (alongside SeedRevision), so the first reconnect can tell same-daemon
// from restarted-daemon. Call once, right after attach. id "" (a
// pre-v7 daemon that doesn't send InstanceID) is recorded as-is; tombstones
// then never auto-drop on reconnect, which is the safe pre-fix behavior.
func (h *Hub) SeedInstance(id string) {
	h.tombMu.Lock()
	h.daemonInstance = id
	h.tombMu.Unlock()
}

// InstanceID returns the daemon-process identity this Hub is currently
// connected to (Attached.InstanceID, updated on every reconnect by
// noteInstance). "" means unknown (pre-v7 daemon, or not yet seeded).
// The GUI's reseat uses it to scope its mint-once to a single daemon
// instance: if the instance changes mid-reseat (a second restart), the
// previously-minted window ID belongs to a dead daemon and must be
// re-minted on the new one.
func (h *Hub) InstanceID() string {
	h.tombMu.Lock()
	defer h.tombMu.Unlock()
	return h.daemonInstance
}

// noteInstance is called on every reconnect with the new connection's
// Attached.InstanceID. It drops all tombstones only when the identity
// CHANGES from a known baseline (a restarted/fresh daemon — its tab-id
// space resets to 1, so old tombstones would suppress legit reused IDs).
// Returns true when this reattach observed a genuine daemon RESTART (the
// identity changed from a known non-empty baseline) — the caller uses
// that to mark vanished Sources restart-vanished so the GUI reseats
// rather than quits.
// Cases:
//   - id == "" (pre-v7 daemon): unknown identity → never drop, not a
//     detectable restart (return false).
//   - no baseline yet (unseeded): record it, DON'T drop — clearing on a
//     first same-daemon reconnect would wrongly discard a tombstone for
//     a tab closed while disconnected. Not a restart (return false).
//   - id differs from baseline: a restart → drop tombstones, rebase,
//     return true.
//   - id matches baseline: same daemon → keep tombstones, return false.
func (h *Hub) noteInstance(id string) bool {
	if id == "" {
		return false
	}
	restarted := false
	h.tombMu.Lock()
	switch {
	case h.daemonInstance == "":
		h.daemonInstance = id
	case id != h.daemonInstance:
		h.tombstones = make(map[uint32]struct{})
		h.daemonInstance = id
		restarted = true
	}
	h.tombMu.Unlock()
	return restarted
}

// reconcileTombstones is the tombstone-replay step run before adopting
// any snapshot (live broadcast OR reconnect resync). For each tombstoned
// ID: if it's still in the snapshot, re-send MsgTabClose (the close
// never landed); if it's gone, drop the tombstone (the daemon confirmed
// it). Returns the snapshot's tab list with tombstoned entries filtered
// out, so neither the Hub's own adopt loop nor the GUI callback ever
// re-creates a tab the user already closed. Runs only on the router
// goroutine; SendTabClose is issued outside tombMu.
func (h *Hub) reconcileTombstones(tabs []protocol.TabInfo) []protocol.TabInfo {
	h.tombMu.Lock()
	if len(h.tombstones) == 0 {
		h.tombMu.Unlock()
		return tabs
	}
	present := make(map[uint32]bool, len(tabs))
	for _, ti := range tabs {
		present[ti.ID] = true
	}
	var replay []uint32
	for id := range h.tombstones {
		if present[id] {
			replay = append(replay, id)
		} else {
			delete(h.tombstones, id)
		}
	}
	tomb := make(map[uint32]bool, len(h.tombstones))
	for id := range h.tombstones {
		tomb[id] = true
	}
	h.tombMu.Unlock()

	cli := h.client()
	for _, id := range replay {
		_ = cli.SendTabClose(id)
	}
	if len(tomb) == 0 {
		return tabs
	}
	out := make([]protocol.TabInfo, 0, len(tabs))
	for _, ti := range tabs {
		if !tomb[ti.ID] {
			out = append(out, ti)
		}
	}
	return out
}

// topoWithTabs returns a shallow copy of t with Tabs replaced — used to
// hand the GUI callback a tombstone-filtered snapshot without mutating
// the original (Windows[].TabIDs may still reference a filtered tab, but
// the GUI reconcile adopts off Tabs, so that's harmless).
func topoWithTabs(t *protocol.TopologyChanged, tabs []protocol.TabInfo) *protocol.TopologyChanged {
	cp := *t
	cp.Tabs = tabs
	return &cp
}

func (h *Hub) lookup(id uint32) *Source {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sources[id]
}

// Lookup returns the registered Source for a tab ID, or nil if none —
// a read-only accessor that, unlike Adopt, never creates a Source. Used
// by tests to assert what the Hub adopted on its own (e.g. that a resync
// adopted a new tab) without the lookup itself mutating hub state.
func (h *Hub) Lookup(id uint32) *Source { return h.lookup(id) }

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

	cli := h.client()
	if err := cli.SendTabCreateReq(windowID, uint16(cols), uint16(rows), cwd, "", reqID); err != nil {
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
	case <-cli.Closed():
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

	cli := h.client()
	if err := cli.SendWindowCreateReq(posX, posY, width, height, reqID); err != nil {
		return 0, fmt.Errorf("daemonsource: SendWindowCreate: %w", err)
	}
	select {
	case wc := <-reply:
		return wc.Info.ID, nil
	case <-cli.Closed():
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
	cb := h.onTopology
	h.mu.Unlock()

	// Replay locally-closed tabs + filter them out so the resync can't
	// resurrect a tab the user already closed (the documented pitfall).
	tabs := h.reconcileTombstones(topo.Tabs)

	h.mu.Lock()
	want := make(map[uint32]bool, len(tabs))
	for _, ti := range tabs {
		want[ti.ID] = true
	}
	var vanished []*Source
	for id, s := range h.sources {
		if !want[id] {
			vanished = append(vanished, s)
		}
	}
	h.mu.Unlock()

	// Adopt new tabs (Adopt locks h.mu internally, so call it outside
	// the lock above). Idempotent for already-known IDs.
	for _, ti := range tabs {
		h.Adopt(ti.ID, int(ti.Cols), int(ti.Rows))
	}
	// Drop tabs that no longer exist on the daemon. This is the LIVE
	// path (a topology broadcast on a still-connected daemon), so any
	// vanish here is a remote close, never a restart.
	for _, s := range vanished {
		s.markVanished(false)
	}
	if cb != nil {
		cb(topoWithTabs(topo, tabs))
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
	for {
		cli := h.client()
		h.route(cli)
		// route returned: the connection dropped or the hub is stopping.
		// Stop wins; otherwise try to reconnect (no-op if no redial set).
		select {
		case <-h.stopCh:
			return
		default:
		}
		if !h.reconnect() {
			return
		}
	}
}

// route demuxes frames from ONE client connection until that connection
// closes or the hub stops, then returns so router() can decide whether
// to reconnect. Reading cli's channels (not h.client()'s) is deliberate:
// after a reconnect swap, router() calls route() again with the fresh
// client, so this loop is always bound to a single connection's lifetime.
func (h *Hub) route(cli *clientproto.Client) {
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

// Reconnect backoff bounds. Start fast (a daemon restart / SSH blip is
// usually back within a second or two) and cap so a genuinely-down
// target is retried cheaply forever rather than spinning. Mosh-style:
// we never give up on our own; the user closing the tab is what stops it.
const (
	reconnectMinBackoff = 500 * time.Millisecond
	reconnectMaxBackoff = 5 * time.Second
)

// reconnect freezes every Source (last frame held, input dropped), then
// re-dials with capped backoff until it succeeds or the hub stops. On
// success it swaps in the fresh client, resyncs topology from the new
// Attached snapshot, clears the frozen state, and returns true so the
// router resumes on the new connection. Returns false only when there's
// no redial dialer or the hub is stopping.
func (h *Hub) reconnect() bool {
	h.mu.RLock()
	redial := h.redial
	h.mu.RUnlock()
	if redial == nil {
		return false
	}
	h.markAllReconnecting(true)
	backoff := reconnectMinBackoff
	for {
		select {
		case <-h.stopCh:
			return false
		default:
		}
		newCli, attached, err := redial()
		if err != nil {
			select {
			case <-h.stopCh:
				return false
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > reconnectMaxBackoff {
				backoff = reconnectMaxBackoff
			}
			continue
		}
		old := h.conn.Swap(newCli)
		if old != nil {
			_ = old.Close()
		}
		h.resyncAfterReconnect(attached)
		h.markAllReconnecting(false)
		return true
	}
}

// markAllReconnecting flips the reconnecting flag on every registered
// Source. While set, a Source holds its last rendered frame (the shadow
// emulator is never cleared) and drops input (the daemon can't receive
// it). signalDirty wakes the render loop so the GUI repaints the frozen-
// /dimmed frame + any "reconnecting" badge promptly.
func (h *Hub) markAllReconnecting(v bool) {
	h.mu.RLock()
	srcs := make([]*Source, 0, len(h.sources))
	for _, s := range h.sources {
		srcs = append(srcs, s)
	}
	h.mu.RUnlock()
	for _, s := range srcs {
		s.setReconnecting(v)
	}
}

// resyncAfterReconnect reconciles the Hub's adopted Sources against the
// Attached snapshot from a freshly-reconnected daemon. Unlike
// applyTopology it does NOT revision-gate — a new connection's snapshot
// is authoritative, so the revision is reset to it. Tabs that survived
// (same IDs — the daemon persisted the session across an SSH drop) are
// re-adopted idempotently and will be repainted by the CellFull the
// daemon re-sends on (re)subscribe; tabs that vanished (a daemon RESTART
// lost them) are marked gone. The onTopology callback then lets the GUI
// reconcile its tab bar.
func (h *Hub) resyncAfterReconnect(att *protocol.Attached) {
	h.mu.Lock()
	h.lastRevision = att.Revision
	cb := h.onTopology
	h.mu.Unlock()

	// If we reconnected to a DIFFERENT daemon instance (a restart), drop
	// tombstones first — they belonged to the dead daemon's tab-id space,
	// which the fresh daemon reuses from 1. Must run BEFORE
	// reconcileTombstones so a reused ID isn't suppressed / re-closed.
	// restarted also tells us how to mark any vanished Sources below: a
	// restart-vanish lets the GUI reseat (the daemon came back), whereas
	// a same-instance reattach that still dropped a tab is a remote close.
	restarted := h.noteInstance(att.InstanceID)

	// Same tombstone replay as the live path: a tab the user closed
	// while the link was down is re-closed, not resurrected, by this
	// authoritative snapshot.
	tabs := h.reconcileTombstones(att.Tabs)

	h.mu.Lock()
	var vanished []*Source
	if restarted {
		// The daemon RESTARTED: the old tab-id space is DEAD. Numeric IDs
		// on the fresh daemon belong to a different process and are
		// meaningless to compare against pre-restart Sources — a new tab
		// reusing an old ID is NOT the old tab surviving. So vanish EVERY
		// pre-restart Source (restart-vanished, so the GUI reseats) and
		// let the new daemon's tabs be adopted as FRESH Sources below,
		// regardless of ID collisions.
		for _, s := range h.sources {
			vanished = append(vanished, s)
		}
	} else {
		// Same instance (e.g. an SSH blip the daemon survived): match by
		// ID. Surviving tabs are re-adopted idempotently; only IDs absent
		// from the snapshot vanished (a remote close while the link was
		// down).
		want := make(map[uint32]bool, len(tabs))
		for _, ti := range tabs {
			want[ti.ID] = true
		}
		for id, s := range h.sources {
			if !want[id] {
				vanished = append(vanished, s)
			}
		}
	}
	h.mu.Unlock()

	// Vanish FIRST, adopt SECOND. markVanished unregisters the Source, so
	// on a restart an Adopt for a reused numeric ID then creates a FRESH
	// Source instead of returning the dead one. (Order is irrelevant in
	// the same-instance case — vanished IDs and adopted IDs never overlap
	// there — so this is safe for both paths.)
	for _, s := range vanished {
		s.markVanished(restarted)
	}
	for _, ti := range tabs {
		h.Adopt(ti.ID, int(ti.Cols), int(ti.Rows))
	}
	if cb != nil {
		cb(&protocol.TopologyChanged{
			SessionName:  att.SessionName,
			Revision:     att.Revision,
			Windows:      att.Windows,
			Tabs:         tabs,
			FocusedTabID: att.FocusedTabID,
		})
	}
}

// applyChildExit silences "unused" linter — keeps the import live.
var _ = protocol.MsgChildExit
