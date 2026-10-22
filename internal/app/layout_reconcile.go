package app

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/platform"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/tabs"
)

// The GUI is the reference for which tabs share a window, their order
// and the active tab. The daemon's record of that layout is only
// updated by the messages the GUI happens to send, so any path that
// moves or creates a tab without one (a torn-off window with no daemon
// window yet, a tab made over MCP into the daemon's default window, a
// lost message) leaves the record drifted, and a GUI restart rebuilds
// the drifted record. Instead of patching each path, reconcileDaemonLayout
// diffs the GUI's windows against the daemon's latest topology and sends
// whatever corrections close the gap.

const (
	// layoutSettle is how long the GUI layout must be unchanged before it
	// is reconciled, so the daemon's reply to the user's own move has
	// arrived and isn't mistaken for drift.
	layoutSettle = 500 * time.Millisecond
	// layoutPeriod is the slow background check.
	layoutPeriod = 15 * time.Second
	// synthWindowBase marks planned daemon windows that don't exist yet.
	synthWindowBase = uint32(1) << 31
)

// guiLayoutWindow is one GUI window as seen by one daemon: the daemon
// window it maps to (0 = none yet), the tabs it shows from that daemon
// in tab-bar order, and its active tab if that tab is from the daemon.
type guiLayoutWindow struct {
	daemonID uint32
	tabs     []uint32
	active   uint32
	// parked marks tabs in tabs that this window only holds because the
	// GUI had no window for their daemon window. They belong to another
	// GUI's window, so a push never moves them or drains their window.
	parked map[uint32]bool
}

type layoutOpKind int

const (
	opMoveTab layoutOpKind = iota
	opFocusTab
	opCloseWindow
)

// layoutOp is one correction. win is a daemon window ID, or
// synthWindowBase+i for GUI window i whose daemon window must be
// created first.
type layoutOp struct {
	kind  layoutOpKind
	tab   uint32
	win   uint32
	from  uint32 // window the daemon has the tab in (opMoveTab)
	index int32  // destination index (opMoveTab)
}

// planLayout returns the corrections that make the daemon's windows
// match the GUI's. It simulates the daemon's own MoveTab/CloseWindow
// semantics so each op is computed against the state the earlier ops
// leave behind. Tabs the GUI doesn't show are never moved (another GUI
// may be attached to the same daemon); they only get pushed behind the
// GUI's tabs in a window the GUI owns. owned lists daemon windows this
// GUI created or adopted, the only emptied windows it may close.
//
// push says the GUI's layout just changed locally, so every difference
// is this GUI's to correct. Without it (a topology change or the slow
// timer) the difference may be another GUI's edit to the shared daemon
// record, which this GUI follows instead (followDaemonOrder): it then
// only moves tabs into daemon windows that don't exist yet, never
// reorders within or moves between existing ones, and sends no focus.
func planLayout(gui []guiLayoutWindow, daemon []protocol.WindowInfo, owned map[uint32]bool, push bool) []layoutOp {
	sim := make(map[uint32][]uint32, len(daemon))
	focus := make(map[uint32]uint32, len(daemon))
	var order []uint32
	where := make(map[uint32]uint32)
	for _, wi := range daemon {
		sim[wi.ID] = append([]uint32(nil), wi.TabIDs...)
		focus[wi.ID] = wi.FocusedTabID
		order = append(order, wi.ID)
		for _, t := range wi.TabIDs {
			where[t] = wi.ID
		}
	}
	indexOf := func(win, tab uint32) int {
		for i, t := range sim[win] {
			if t == tab {
				return i
			}
		}
		return -1
	}

	var ops []layoutOp
	drained := make(map[uint32]bool)
	dest := make([]uint32, len(gui))
	for gi, g := range gui {
		d := g.daemonID
		if _, ok := sim[d]; !ok || d == 0 {
			d = synthWindowBase + uint32(gi)
		}
		dest[gi] = d
		i := -1
		for _, tid := range g.tabs {
			if g.parked[tid] {
				continue // like a foreign tab: not counted, not moved
			}
			i++
			from, known := where[tid]
			if !known {
				continue // not in the daemon's record (yet)
			}
			if from == d && indexOf(d, tid) == i {
				continue
			}
			if !push && d < synthWindowBase {
				continue // another GUI's edit; followed, not undone
			}
			ops = append(ops, layoutOp{kind: opMoveTab, tab: tid, win: d, from: from, index: int32(i)})
			// Apply to the simulation, mirroring Session.MoveTab.
			src := sim[from]
			si := indexOf(from, tid)
			sim[from] = append(src[:si:si], src[si+1:]...)
			if focus[from] == tid {
				focus[from] = 0
				if len(sim[from]) > 0 {
					focus[from] = sim[from][0]
				}
			}
			if from != d {
				drained[from] = true
			}
			dst := sim[d]
			at := i
			if at > len(dst) {
				at = len(dst)
			}
			dst = append(dst, 0)
			copy(dst[at+1:], dst[at:])
			dst[at] = tid
			sim[d] = dst
			where[tid] = d
			if focus[d] == 0 {
				focus[d] = tid
			}
		}
	}
	if !push {
		return ops
	}
	for gi, g := range gui {
		d := dest[gi]
		if g.active == 0 || focus[d] == g.active || indexOf(d, g.active) < 0 {
			continue
		}
		ops = append(ops, layoutOp{kind: opFocusTab, tab: g.active, win: d})
		focus[d] = g.active
	}
	for _, id := range order {
		if len(sim[id]) == 0 && (drained[id] || owned[id]) {
			ops = append(ops, layoutOp{kind: opCloseWindow, win: id})
		}
	}
	return ops
}

// guiLayoutFor collects the GUI windows that show tabs from hub.
func (a *App) guiLayoutFor(hub *daemonsource.Hub) (gui []guiLayoutWindow, wins []*Window, owned map[uint32]bool) {
	owned = make(map[uint32]bool)
	cli := hub.Client()
	for _, w := range a.windows {
		if w.tabs != nil && !w.pendingClose {
			if d := w.daemonWindowForHub(hub); d != 0 {
				owned[d] = true
			}
		}
	}
	for _, w := range a.windows {
		if w.tabs == nil || w.pendingClose {
			continue
		}
		g := guiLayoutWindow{daemonID: w.daemonWindowForHub(hub)}
		var activeSrc *daemonsource.Source
		if at := w.tabs.Active(); at != nil {
			activeSrc, _ = at.Terminal.(*daemonsource.Source)
		}
		for _, t := range w.tabs.Tabs {
			ds, ok := t.Terminal.(*daemonsource.Source)
			if !ok || ds.HubClient() != cli {
				continue
			}
			if p, ok := w.pendingDaemonMove.(*daemonsource.Source); ok && p == ds {
				continue // its own move is still being retried
			}
			g.tabs = append(g.tabs, ds.TabID())
			if from, ok := a.parkedTabs[ds]; ok {
				if from != g.daemonID && !owned[from] {
					if g.parked == nil {
						g.parked = make(map[uint32]bool)
					}
					g.parked[ds.TabID()] = true
				} else {
					delete(a.parkedTabs, ds) // this GUI now has a window for it
				}
			}
			if ds == activeSrc {
				g.active = ds.TabID()
			}
		}
		gui = append(gui, g)
		wins = append(wins, w)
	}
	return gui, wins, owned
}

// closeVanishedTab removes a tab the daemon no longer has (closed by
// another client, or lost in a daemon restart). That is the daemon's
// change, not an edit made on this GUI, so when the layout was in step
// before the removal the fingerprint is brought forward with it, as
// applyPendingTopology does; otherwise the next pass would push this
// GUI's layout over the other client's.
func (a *App) closeVanishedTab(m *tabs.Manager, i int, ds *daemonsource.Source) {
	synced := a.layoutFingerprint() == a.layoutFP
	m.CloseTab(i)
	delete(a.parkedTabs, ds)
	if synced {
		a.layoutFP = a.layoutFingerprint()
	}
}

// forgetDaemonWindow drops id from every window's mapping for hub, so
// the next windowIDForHub mints a fresh daemon window.
func (a *App) forgetDaemonWindow(hub *daemonsource.Hub, id uint32) {
	for _, w := range a.windows {
		w.daemonWindowIDsMu.Lock()
		if w.daemonWindowIDs[hub] == id {
			delete(w.daemonWindowIDs, hub)
		}
		w.daemonWindowIDsMu.Unlock()
		if hub == a.daemonHub && w.daemonWindowID == id {
			w.daemonWindowID = 0
		}
	}
}

// followDaemonOrder applies the daemon's tab order to each GUI window
// that maps to a daemon window, for the tabs both show, leaving the
// slots of tabs from elsewhere and the active tab alone. It is how a
// reorder made by another GUI on the same daemon reaches this one
// (cross-window moves are followed by relocateDaemonTab). Main thread.
func (a *App) followDaemonOrder(hub *daemonsource.Hub, topo *protocol.TopologyChanged) {
	cli := hub.Client()
	for _, w := range a.windows {
		if w.tabs == nil || w.pendingClose {
			continue
		}
		d := w.daemonWindowForHub(hub)
		if d == 0 {
			continue
		}
		var order []uint32
		for _, wi := range topo.Windows {
			if wi.ID == d {
				order = wi.TabIDs
			}
		}
		rank := make(map[uint32]int, len(order))
		for i, id := range order {
			rank[id] = i
		}
		var slots []int
		for i, t := range w.tabs.Tabs {
			ds, ok := t.Terminal.(*daemonsource.Source)
			if !ok || ds.HubClient() != cli {
				continue
			}
			if p, ok := w.pendingDaemonMove.(*daemonsource.Source); ok && p == ds {
				continue
			}
			if _, in := rank[ds.TabID()]; in {
				slots = append(slots, i)
			}
		}
		picked := make([]*tabs.Tab, len(slots))
		for k, i := range slots {
			picked[k] = w.tabs.Tabs[i]
		}
		sort.SliceStable(picked, func(x, y int) bool {
			return rank[picked[x].Terminal.(*daemonsource.Source).TabID()] < rank[picked[y].Terminal.(*daemonsource.Source).TabID()]
		})
		var active *tabs.Tab
		if w.tabs.ActiveIdx >= 0 && w.tabs.ActiveIdx < len(w.tabs.Tabs) {
			active = w.tabs.Tabs[w.tabs.ActiveIdx]
		}
		for k, i := range slots {
			w.tabs.Tabs[i] = picked[k]
		}
		for i, t := range w.tabs.Tabs {
			if t == active {
				w.tabs.ActiveIdx = i
			}
		}
	}
}

// reconcileDaemonLayout makes the daemon's window record on hub match
// the GUI windows, per planLayout, and returns how many corrections it
// sent. Runs on the main thread (it reads the window/tab structures
// and may block briefly creating a daemon window).
func (a *App) reconcileDaemonLayout(hub *daemonsource.Hub, host string, topo *protocol.TopologyChanged, push bool) int {
	if hub == nil || topo == nil || hub.Client() == nil {
		return 0
	}
	gui, wins, owned := a.guiLayoutFor(hub)
	ops := planLayout(gui, topo.Windows, owned, push)
	if len(ops) == 0 {
		return 0
	}
	cli := hub.Client()
	resolved := make(map[uint32]uint32)
	resolve := func(id uint32) uint32 {
		if id < synthWindowBase {
			return id
		}
		if r, ok := resolved[id]; ok {
			return r
		}
		w := wins[id-synthWindowBase]
		if stale := w.daemonWindowForHub(hub); stale != 0 {
			a.forgetDaemonWindow(hub, stale) // daemon no longer has it
		}
		r := w.windowIDForHub(hub) // mints via CreateWindow
		if r != 0 && hub == a.daemonHub && w.daemonWindowID == 0 {
			w.daemonWindowID = r
		}
		resolved[id] = r
		return r
	}
	sent := 0
	for _, op := range ops {
		win := resolve(op.win)
		if win == 0 {
			break // daemon didn't answer; the next pass retries
		}
		switch op.kind {
		case opMoveTab:
			a.noteLayoutDrift(host, op, win)
			_ = cli.SendWindowMoveTab(op.tab, win, op.index)
		case opFocusTab:
			_ = cli.SendWindowFocusTab(win, op.tab)
		case opCloseWindow:
			_ = cli.SendWindowClose(win)
			a.forgetDaemonWindow(hub, win)
		}
		sent++
	}
	return sent
}

// noteLayoutDrift logs a tab move once per distinct (from, to) so the
// log shows which path left the daemon's record wrong without a
// periodic check repeating itself.
func (a *App) noteLayoutDrift(host string, op layoutOp, to uint32) {
	key := fmt.Sprintf("%s/%d/%d/%d/%d", host, op.tab, op.from, to, op.index)
	if a.driftLogged[key] {
		return
	}
	if a.driftLogged == nil {
		a.driftLogged = make(map[string]bool)
	}
	a.driftLogged[key] = true
	if op.from == to {
		fmt.Fprintf(os.Stderr, "xerotty: layout drift: tab %d daemon window %d index -> %d (host %q)\n", op.tab, to, op.index, host)
		return
	}
	fmt.Fprintf(os.Stderr, "xerotty: layout drift: tab %d daemon window %d -> %d (host %q)\n", op.tab, op.from, to, host)
}

// noteTopology records the daemon's latest window record for hub.
func (a *App) noteTopology(hub *daemonsource.Hub, topo *protocol.TopologyChanged) {
	a.topoMu.Lock()
	if a.lastTopo == nil {
		a.lastTopo = make(map[*daemonsource.Hub]*protocol.TopologyChanged)
	}
	a.lastTopo[hub] = topo
	a.topoMu.Unlock()
}

// noteAttached seeds the record from an attach snapshot.
func (a *App) noteAttached(hub *daemonsource.Hub, at *protocol.Attached) {
	a.noteTopology(hub, &protocol.TopologyChanged{
		Revision: at.Revision,
		Windows:  at.Windows,
		Tabs:     at.Tabs,
	})
}

// layoutFingerprint summarises the GUI layout (windows, tab order,
// active tab) so a change can be noticed cheaply every frame.
func (a *App) layoutFingerprint() string {
	var b []byte
	for _, w := range a.windows {
		b = append(b, '|')
		if w.tabs == nil {
			continue
		}
		for i, t := range w.tabs.Tabs {
			id := uint32(0)
			if ds, ok := t.Terminal.(*daemonsource.Source); ok {
				id = ds.TabID()
			}
			mark := byte(',')
			if i == w.tabs.ActiveIdx {
				mark = '*'
			}
			b = append(b, fmt.Sprintf("%c%d", mark, id)...)
		}
	}
	return string(b)
}

// layoutPass is one reconcile: a local change is pushed to the daemon;
// otherwise the daemon's order is followed first. Returns the number of
// corrections sent.
func (a *App) layoutPass(hub *daemonsource.Hub, host string, topo *protocol.TopologyChanged, push bool) int {
	if !push {
		a.followDaemonOrder(hub, topo)
	}
	return a.reconcileDaemonLayout(hub, host, topo, push)
}

// maybeReconcileLayout runs once per frame on the main thread. It
// reconciles shortly after the GUI layout or the daemon's record
// changes, and on a slow timer in case a change went unnoticed.
func (a *App) maybeReconcileLayout() {
	if a.dragTab != nil || len(a.daemonAdoptQueue) > 0 {
		return
	}
	now := time.Now()
	wake := func() { time.AfterFunc(layoutSettle+50*time.Millisecond, platform.PostWake) }
	if fp := a.layoutFingerprint(); fp != a.layoutFP {
		a.layoutFP = fp
		a.layoutDirty = true
		a.layoutDue = now.Add(layoutSettle)
		wake()
	}
	a.topoMu.Lock()
	topos := make(map[*daemonsource.Hub]*protocol.TopologyChanged, len(a.lastTopo))
	for hub, t := range a.lastTopo {
		topos[hub] = t
		if a.layoutSeenRev[hub] != t.Revision {
			if a.layoutSeenRev == nil {
				a.layoutSeenRev = make(map[*daemonsource.Hub]uint64)
			}
			a.layoutSeenRev[hub] = t.Revision
			a.layoutDue = now.Add(layoutSettle)
			wake()
		}
	}
	a.topoMu.Unlock()
	if !a.layoutLast.IsZero() && now.Sub(a.layoutLast) >= layoutPeriod && a.layoutDue.IsZero() {
		a.layoutDue = now
	}
	if a.layoutLast.IsZero() {
		a.layoutLast = now
	}
	if a.layoutDue.IsZero() || now.Before(a.layoutDue) {
		return
	}
	a.layoutDue = time.Time{}
	a.layoutLast = now
	// Only a change made on this GUI is pushed to the daemon; anything
	// else may be another GUI's edit to the shared record and is followed.
	push := a.layoutDirty
	a.layoutDirty = false
	for name, hub := range a.hubsByName() {
		if t := topos[hub]; t != nil {
			a.layoutPass(hub, name, t, push)
		}
	}
	if !push {
		a.layoutFP = a.layoutFingerprint() // following isn't a local change
	}
}

// tabPlacement is where list_tabs reports a tab sits.
type tabPlacement struct {
	windowIndex int    // index in the GUI's window list
	guiWindowID uint32 // daemon window the GUI window maps to
}

// publishTabLayout stores each shown daemon tab's GUI window for the
// guimcp goroutine (main thread only, like publishFocusedSources).
func (a *App) publishTabLayout() {
	out := map[*daemonsource.Source]tabPlacement{}
	for i, w := range a.windows {
		if w.tabs == nil {
			continue
		}
		for _, t := range w.tabs.Tabs {
			ds, ok := t.Terminal.(*daemonsource.Source)
			if !ok {
				continue
			}
			p := tabPlacement{windowIndex: i}
			if hub := a.findHubForClient(ds.HubClient()); hub != nil {
				p.guiWindowID = w.daemonWindowForHub(hub)
			}
			out[ds] = p
		}
	}
	a.tabLayout.Store(&out)
}

// recordedWindow returns the daemon window the daemon's latest record
// puts tab in on hub (0 = unknown).
func (a *App) recordedWindow(hub *daemonsource.Hub, tab uint32) uint32 {
	a.topoMu.Lock()
	defer a.topoMu.Unlock()
	t := a.lastTopo[hub]
	if t == nil {
		return 0
	}
	for _, wi := range t.Windows {
		for _, id := range wi.TabIDs {
			if id == tab {
				return wi.ID
			}
		}
	}
	return 0
}
