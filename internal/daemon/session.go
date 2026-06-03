package daemon

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/terminal"
)

// Session is a collection of windows that each hold tabs. Survives
// client detach. The daemon may host multiple sessions later;
// Phase 0 has exactly one named "default".
//
// Session > Window > Tab is the canonical layout. WindowInfo.TabIDs
// is the source of truth for tab-bar ordering within a window.
type Session struct {
	Name string

	cfg *config.Config

	// daemon is a back-reference for fan-out operations that need
	// to enumerate attached clients (bell + scrollback cleared
	// broadcasts). Set in newSession; nil-safe — Session methods
	// that need it guard.
	daemon *Daemon

	mu       sync.Mutex
	nextTabID uint32
	nextWinID uint32

	// revision is a monotonic per-session topology counter, bumped
	// under mu on every structural change (tab/window create/close,
	// tab move). It stamps the snapshots broadcast via
	// MsgTopologyChanged so clients can gate stale/duplicate frames.
	revision uint64

	// All tabs in the session, keyed by ID. Each tab also lives
	// in exactly one Window's TabIDs slice (the daemon enforces
	// this invariant on every Window/Tab mutation).
	tabs map[uint32]*Tab

	// tabsByName maps an agent-chosen label → tab ID, the index
	// behind FindOrCreateTab's idempotency. Only named tabs appear
	// here; an unnamed tab (Tab.Name == "") is absent. Kept in sync
	// with `tabs`: NewTab registers, CloseTab removes. Guarded by mu.
	tabsByName map[string]uint32

	// All windows in the session, in stable creation order.
	windows []*Window

	// App-wide focused tab — independent of per-window focus.
	focusedTabID uint32

	// Most recent clipboard text seen on this session (from any
	// attached client's ClipboardData frame). Daemon-side helpers
	// read it via Clipboard()/SetClipboard().
	clipboard string

	// Queue of writes proposed by agents in "propose" mode.
	// Consumed two ways: the GUI's approval banner (broadcast via
	// MsgProposalsChanged, resolved via MsgProposalResolve) and
	// the MCP list_proposals/approve_proposal/drop_proposal tools.
	// Bounded at proposedCap.
	proposed []ProposedAction

	// initMu serializes EnsureInitialTab so two concurrent
	// NewIfMissing attaches don't each spawn a tab.
	initMu sync.Mutex
}

// Title returns the OSC-set title for this tab. Safe to call
// concurrently with the OnTitle callback that sets it.
func (t *Tab) Title() string {
	t.titleMu.RLock()
	defer t.titleMu.RUnlock()
	return t.title
}

// SetTitle updates the tab title. Called by the OnTitle callback
// installed in NewTab. Exposed for tests; production code uses
// the callback.
func (t *Tab) SetTitle(s string) {
	t.titleMu.Lock()
	t.title = s
	t.titleMu.Unlock()
}

// ProposedAction is one queued write from an agent operating in
// "propose" mode. The UI is expected to surface these to the user,
// who approves or rejects each. Until a UI consumer ships, the
// queue grows unbounded; Phase 4 punts on retention policy.
type ProposedAction struct {
	TabID  uint32
	IsPaste bool   // true → Paste(), false → Write()
	Bytes  []byte // raw for input, paste text encoded as bytes
}

// MaxTabDim bounds client-requested grid dimensions. A bogus or
// hostile TabCreate/Resize (rows/cols in the millions) would
// otherwise make the daemon allocate an enormous cell grid on every
// snapshot and OOM. 2000 is far past any real display — a 4K screen
// at a tiny 6px font is ~640 columns.
const MaxTabDim = 2000

// ClampTabDim clamps a requested dimension to [1, MaxTabDim]. Used at
// every client-facing tab create/resize boundary (wire + MCP).
func ClampTabDim(v int) int {
	if v < 1 {
		return 1
	}
	if v > MaxTabDim {
		return MaxTabDim
	}
	return v
}

// proposedCap bounds Session.proposed so a noisy agent can't
// exhaust memory in propose mode. Drops oldest on overflow — the
// alternative (blocking the agent's RPC) would hang propose-mode
// callers indefinitely when no consumer drains the queue.
const proposedCap = 256

// QueueProposedInput stores an input proposal from an agent.
// Returns the queue index of the newly-queued item so the caller
// can correlate later approve/drop.
func (s *Session) QueueProposedInput(tabID uint32, bytes []byte) int {
	s.mu.Lock()
	cp := make([]byte, len(bytes))
	copy(cp, bytes)
	s.proposed = append(s.proposed, ProposedAction{
		TabID: tabID, IsPaste: false, Bytes: cp,
	})
	s.trimProposedLocked()
	idx := len(s.proposed) - 1
	s.mu.Unlock()
	s.notifyProposals()
	return idx
}

// QueueProposedPaste stores a paste proposal from an agent.
func (s *Session) QueueProposedPaste(tabID uint32, text string) int {
	s.mu.Lock()
	s.proposed = append(s.proposed, ProposedAction{
		TabID: tabID, IsPaste: true, Bytes: []byte(text),
	})
	s.trimProposedLocked()
	idx := len(s.proposed) - 1
	s.mu.Unlock()
	s.notifyProposals()
	return idx
}

// notifyProposals tells the daemon to broadcast the current queue
// to wire clients (GUI approval gate). nil-safe.
func (s *Session) notifyProposals() {
	if s.daemon != nil {
		s.daemon.broadcastProposals()
	}
}

func (s *Session) trimProposedLocked() {
	if len(s.proposed) > proposedCap {
		s.proposed = s.proposed[len(s.proposed)-proposedCap:]
	}
}

// ApproveProposal pops the proposal at idx and applies it to its
// target tab. Returns (true, applied-action, nil) on success.
// (false, _, nil) when idx is out of range. (true, _, err) when
// the apply itself failed (e.g. tab closed between queue + drain).
func (s *Session) ApproveProposal(idx int) (bool, ProposedAction, error) {
	s.mu.Lock()
	if idx < 0 || idx >= len(s.proposed) {
		s.mu.Unlock()
		return false, ProposedAction{}, nil
	}
	act := s.proposed[idx]
	s.proposed = append(s.proposed[:idx], s.proposed[idx+1:]...)
	t, ok := s.tabs[act.TabID]
	s.mu.Unlock()
	if !ok {
		return true, act, nil // tab gone — drop silently
	}
	if act.IsPaste {
		t.Term.Paste(string(act.Bytes))
	} else {
		if _, err := t.Term.Write(act.Bytes); err != nil {
			s.notifyProposals()
			return true, act, err
		}
	}
	s.notifyProposals()
	return true, act, nil
}

// DropProposal removes the proposal at idx without applying. Used
// by reject paths.
func (s *Session) DropProposal(idx int) bool {
	s.mu.Lock()
	if idx < 0 || idx >= len(s.proposed) {
		s.mu.Unlock()
		return false
	}
	s.proposed = append(s.proposed[:idx], s.proposed[idx+1:]...)
	s.mu.Unlock()
	s.notifyProposals()
	return true
}

// PendingProposals returns the current queue (snapshot). The UI
// consumer is expected to call ApproveProposal or DropProposal per
// item; this just returns what's queued.
func (s *Session) PendingProposals() []ProposedAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProposedAction, len(s.proposed))
	copy(out, s.proposed)
	return out
}

// Clipboard returns the most recent clipboard text recorded on this
// session.
func (s *Session) Clipboard() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clipboard
}

// SetClipboard records new clipboard text for the session. Called
// when a client pushes ClipboardData. Use to backfill OSC 52 reads
// later.
func (s *Session) SetClipboard(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clipboard = text
}

// Tab is one PTY-backed terminal inside a session. The wrapped
// *terminal.Terminal already owns the PTY, the SafeEmulator, and
// the reader goroutines. We just track its identity in the session.
type Tab struct {
	ID uint32

	// Name is an optional agent-chosen label, set once at creation
	// and never mutated — that write-once-before-publish discipline
	// (set under s.mu before the tab enters s.tabs) is what makes it
	// safe to read without a per-field lock, unlike `title`. Empty
	// for tabs spawned without a name. The reuse key for
	// FindOrCreateTab.
	Name string
	// title is set from the OnTitle callback (PTY reader goroutine)
	// and read from publishLoop (separate goroutine). Guard with
	// titleMu — straight string field had a -race-flagged race.
	titleMu sync.RWMutex
	title   string

	Term *terminal.Terminal

	// Exited is closed once the PTY child reaps. Subscribers (each
	// attached client's publishLoop) select on it to know when to
	// ship MsgChildExit + tear down the per-(client, tab) state.
	// ExitCode is set BEFORE Exited closes so racing reads see the
	// right value. Single-shot — Tab is never reused after exit.
	Exited   chan struct{}
	ExitCode int32
}

// Window is a logical UI window — a grouping of tabs. The daemon
// tracks geometry as hints (the compositor on Wayland may override
// position) so a UI restoring the layout from another machine has
// something to work with.
type Window struct {
	ID           uint32
	PosX         int32
	PosY         int32
	Width        int32
	Height       int32
	TabIDs       []uint32 // tab-bar order, left-to-right
	FocusedTabID uint32   // which tab is in front within this window
}

func newSession(name string, cfg *config.Config, d *Daemon) *Session {
	return &Session{
		Name:      name,
		cfg:       cfg,
		daemon:    d,
		nextTabID:  1,
		nextWinID:  1,
		tabs:       make(map[uint32]*Tab),
		tabsByName: make(map[string]uint32),
	}
}

// ensureDefaultWindowLocked creates a default window if the session
// has none yet. Caller must hold s.mu.
func (s *Session) ensureDefaultWindowLocked() *Window {
	if len(s.windows) > 0 {
		return s.windows[0]
	}
	w := &Window{
		ID:     s.nextWinID,
		Width:  80,
		Height: 24,
	}
	s.nextWinID++
	s.windows = append(s.windows, w)
	return w
}

// NewWindow registers a new logical window in the session. The
// returned window has no tabs yet.
func (s *Session) NewWindow(posX, posY, width, height int32) *Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := &Window{
		ID:     s.nextWinID,
		PosX:   posX,
		PosY:   posY,
		Width:  width,
		Height: height,
	}
	s.nextWinID++
	s.windows = append(s.windows, w)
	s.revision++
	return w
}

// CloseWindow removes a window. Tabs in the window get reassigned
// to the first remaining window in the session; if there are no
// other windows, a fresh default window is created so tabs stay
// accessible from a future reattach.
func (s *Session) CloseWindow(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed *Window
	var removedIdx int
	for i, w := range s.windows {
		if w.ID == id {
			removed = w
			removedIdx = i
			break
		}
	}
	if removed == nil {
		return
	}
	s.windows = append(s.windows[:removedIdx], s.windows[removedIdx+1:]...)
	s.revision++
	if len(removed.TabIDs) == 0 {
		return
	}
	var dest *Window
	if len(s.windows) > 0 {
		dest = s.windows[0]
	} else {
		dest = s.ensureDefaultWindowLocked()
	}
	dest.TabIDs = append(dest.TabIDs, removed.TabIDs...)
	if dest.FocusedTabID == 0 && len(dest.TabIDs) > 0 {
		dest.FocusedTabID = dest.TabIDs[0]
	}
}

// NewTab spawns a PTY-backed tab and adds it to a window. windowID
// of 0 means "session's default window" (one is created if absent).
// cols/rows are the initial PTY dimensions; cwd is the starting
// directory ("" for xerottyd's CWD). name is an optional reuse
// label ("" for an unnamed tab); when set it's recorded in
// tabsByName so a later FindOrCreateTab can return this same tab.
//
// Returns the new tab and the window it joined.
func (s *Session) NewTab(windowID uint32, cols, rows int, cwd, name string) (*Tab, *Window, error) {
	// Daemon-hosted variant forces unlimited+disk scrollback mode
	// on the server side regardless of cfg.Scrollback.Mode. Why:
	// memory mode's bounded ring rotates without notification, so
	// the daemon can't reliably ship scrollback rows to clients in
	// order (they go out of sync the moment the ring fills). Disk
	// mode keeps absolute scrollback indices stable. The user's
	// "memory mode" preference still applies as a client-side
	// display cap (daemonsource.Hub.SetScrollbackCap). Server
	// hoards; client decides how much to mirror.
	cols = ClampTabDim(cols)
	rows = ClampTabDim(rows)
	term, err := terminal.NewDaemonHosted(s.cfg, cols, rows, cwd)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &Tab{
		ID:     s.nextTabID,
		Name:   name,
		Term:   term,
		Exited: make(chan struct{}),
	}
	s.nextTabID++
	s.tabs[t.ID] = t
	// Register the reuse label before publishing so the index is
	// consistent the instant the tab is observable under mu.
	if name != "" {
		s.tabsByName[name] = t.ID
	}
	term.SetOnTitle(func(title string) {
		t.SetTitle(title)
	})
	// Bell fan-out — when the PTY child emits BEL, broadcast
	// MsgBell to every client attached to this tab. Without this
	// the wire-protocol message type exists but never fires; GUIs
	// and CLIs in daemon mode never hear the bell. Routed through
	// the Daemon back-ref since clients live there, not on Session.
	if s.daemon != nil {
		term.SetOnBell(func() {
			s.daemon.broadcastBell(t.ID)
		})
		// OSC 52: a PTY app writing the clipboard stores it on
		// the session + broadcasts to attached clients (which
		// write their local OS clipboards). A GET ("?") is
		// answered from the session clipboard, which clients
		// populate via SendClipboardData on copy.
		term.SetOnClipboardSet(func(text string) {
			s.SetClipboard(text)
			s.daemon.broadcastClipboardSet(text)
		})
		term.SetClipboardProvider(func() string {
			return s.Clipboard()
		})
	}
	// Latch the exit code + close Exited so per-(client, tab)
	// publishLoops can ship MsgChildExit. SetOnChildExit fires
	// immediately if the shell somehow already exited (rare).
	term.SetOnChildExit(func(code int) {
		// Guard against double-close in case OnChildExit fires
		// twice somehow (Terminal contract says no, but a recover
		// here is cheap insurance).
		defer func() { _ = recover() }()
		atomic.StoreInt32(&t.ExitCode, int32(code))
		close(t.Exited)
	})

	var w *Window
	if windowID != 0 {
		for _, candidate := range s.windows {
			if candidate.ID == windowID {
				w = candidate
				break
			}
		}
	}
	if w == nil {
		w = s.ensureDefaultWindowLocked()
	}
	w.TabIDs = append(w.TabIDs, t.ID)
	if w.FocusedTabID == 0 {
		w.FocusedTabID = t.ID
	}
	if s.focusedTabID == 0 {
		s.focusedTabID = t.ID
	}
	s.revision++
	return t, w, nil
}

// EnsureInitialTab atomically guarantees the session has at least
// one tab + window. Returns true if it created one, false if a tab
// already existed. Used by handleAttach to avoid the TOCTOU race
// where two clients with NewIfMissing each see an empty session
// and both spawn a tab.
//
// initMu serializes concurrent EnsureInitialTab callers around the
// check-and-spawn so only the first creates a tab. After all tabs
// close, the next EnsureInitialTab caller (post-CloseTab) will
// correctly spawn again — initMu just serializes; it doesn't
// "remember" prior spawns.
func (s *Session) EnsureInitialTab(cols, rows int, cwd string) (bool, error) {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	s.mu.Lock()
	already := len(s.tabs) > 0
	s.mu.Unlock()
	if already {
		return false, nil
	}
	if _, _, err := s.NewTab(0, cols, rows, cwd, ""); err != nil {
		return false, err
	}
	return true, nil
}

// FindOrCreateTab returns the tab labelled `name`, spawning + tagging
// one if none exists yet. The third return is true when a tab was
// actually created (false on reuse) so callers can skip the
// subscribe/broadcast they only owe a brand-new tab.
//
// An empty name means "no reuse key" — always a fresh tab, same as
// NewTab. For a non-empty name, initMu serializes concurrent callers
// around the check-and-spawn (exactly as EnsureInitialTab does) so
// two agents racing on the same label don't both spawn; the loser
// finds the winner's tab in tabsByName.
func (s *Session) FindOrCreateTab(name string, windowID uint32, cols, rows int, cwd string) (*Tab, *Window, bool, error) {
	if name == "" {
		t, w, err := s.NewTab(windowID, cols, rows, cwd, "")
		return t, w, true, err
	}
	s.initMu.Lock()
	defer s.initMu.Unlock()
	s.mu.Lock()
	if id, ok := s.tabsByName[name]; ok {
		// Reuse only a LIVE tab under this label. Two ways the
		// index can point at something unusable — both fall through
		// to recreate after dropping the stale label:
		//   - tab missing from s.tabs (defensive; CloseTab keeps the
		//     two maps in lockstep, so this shouldn't happen), or
		//   - tab present but its shell already reaped (Exited
		//     closed). Child-exit latches the corpse without
		//     unlinking it, so without this guard find-or-create
		//     would hand the agent a dead PTY that silently eats its
		//     writes. The corpse lingers in s.tabs like any other
		//     exited tab until something closes it; we just refuse
		//     to reuse it as the named handle.
		if t := s.tabs[id]; t != nil {
			select {
			case <-t.Exited:
				// shell dead — fall through to recreate
			default:
				w := s.windowOfLocked(id)
				s.mu.Unlock()
				return t, w, false, nil
			}
		}
		delete(s.tabsByName, name)
	}
	s.mu.Unlock()
	t, w, err := s.NewTab(windowID, cols, rows, cwd, name)
	return t, w, true, err
}

// windowOfLocked returns the window holding tab id, or nil. Caller
// must hold s.mu.
func (s *Session) windowOfLocked(id uint32) *Window {
	for _, w := range s.windows {
		for _, tid := range w.TabIDs {
			if tid == id {
				return w
			}
		}
	}
	return nil
}

// Tab returns the tab with the given ID, or nil if not found.
func (s *Session) Tab(id uint32) *Tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tabs[id]
}

// Tabs returns a snapshot slice of all tabs in window-order:
// iterate s.windows in their stable creation order, then
// w.TabIDs left-to-right. Anything not in a window (shouldn't
// happen — every tab is in a window — but defensive) gets
// appended by ID at the end. Deterministic so map iteration
// randomness never bleeds into client-visible ordering.
func (s *Session) Tabs() []*Tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.orderedTabsLocked()
}

// orderedTabsLocked returns the window-ordered tab list. Caller holds
// s.mu. Extracted so Tabs() and TopologySnapshot() share one ordering.
func (s *Session) orderedTabsLocked() []*Tab {
	out := make([]*Tab, 0, len(s.tabs))
	seen := make(map[uint32]bool, len(s.tabs))
	for _, w := range s.windows {
		for _, id := range w.TabIDs {
			if t, ok := s.tabs[id]; ok && !seen[id] {
				out = append(out, t)
				seen[id] = true
			}
		}
	}
	// Defensive sweep — any orphan tabs (in s.tabs but not in any
	// window's TabIDs) sorted by ID for stability.
	if len(out) < len(s.tabs) {
		var orphanIDs []uint32
		for id := range s.tabs {
			if !seen[id] {
				orphanIDs = append(orphanIDs, id)
			}
		}
		sort.Slice(orphanIDs, func(i, j int) bool { return orphanIDs[i] < orphanIDs[j] })
		for _, id := range orphanIDs {
			out = append(out, s.tabs[id])
		}
	}
	return out
}

// Revision returns the session's current topology revision.
func (s *Session) Revision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

// TopologySnapshot captures a consistent (single-lock) view of the
// session's structure — revision, windows, ordered tabs, focused tab
// — ready to ship as MsgTopologyChanged (or to seed Attached). The
// revision and window/tab membership are read together under s.mu so
// the revision always matches the structure it stamps. Per-tab
// Title/dims are read after the lock (they're metadata, not topology,
// so a momentarily stale title can't desync the reconcile).
func (s *Session) TopologySnapshot() protocol.TopologyChanged {
	s.mu.Lock()
	rev := s.revision
	focused := s.focusedTabID
	windows := make([]protocol.WindowInfo, len(s.windows))
	for i, w := range s.windows {
		ids := make([]uint32, len(w.TabIDs))
		copy(ids, w.TabIDs)
		windows[i] = protocol.WindowInfo{
			ID:           w.ID,
			PosX:         w.PosX,
			PosY:         w.PosY,
			Width:        w.Width,
			Height:       w.Height,
			TabIDs:       ids,
			FocusedTabID: w.FocusedTabID,
		}
	}
	ordered := s.orderedTabsLocked()
	s.mu.Unlock()

	tabs := make([]protocol.TabInfo, len(ordered))
	for i, t := range ordered {
		tabs[i] = protocol.TabInfo{
			ID:    t.ID,
			Title: t.Title(),
			Cols:  uint16(t.Term.Width()),
			Rows:  uint16(t.Term.Height()),
		}
	}
	return protocol.TopologyChanged{
		SessionName:  s.Name,
		Revision:     rev,
		Windows:      windows,
		Tabs:         tabs,
		FocusedTabID: focused,
	}
}

// Windows returns a snapshot copy of the window list with each
// window's TabIDs copied so the caller can iterate safely.
func (s *Session) Windows() []*Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Window, len(s.windows))
	for i, w := range s.windows {
		// shallow-copy + fresh TabIDs slice
		ids := make([]uint32, len(w.TabIDs))
		copy(ids, w.TabIDs)
		out[i] = &Window{
			ID:           w.ID,
			PosX:         w.PosX,
			PosY:         w.PosY,
			Width:        w.Width,
			Height:       w.Height,
			TabIDs:       ids,
			FocusedTabID: w.FocusedTabID,
		}
	}
	return out
}

// FocusedTab returns the app-wide focused tab ID.
func (s *Session) FocusedTab() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.focusedTabID
}

// SetFocusedTab updates the app-wide focused tab. No-op if id isn't
// a tab in this session.
func (s *Session) SetFocusedTab(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tabs[id]; ok {
		s.focusedTabID = id
	}
}

// SetWindowFocusedTab updates per-window focus.
func (s *Session) SetWindowFocusedTab(windowID, tabID uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.windows {
		if w.ID == windowID {
			// Confirm the tab actually belongs to this window
			for _, id := range w.TabIDs {
				if id == tabID {
					w.FocusedTabID = tabID
					return
				}
			}
		}
	}
}

// SetWindowGeometry updates a window's position/size hints.
func (s *Session) SetWindowGeometry(id uint32, posX, posY, width, height int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.windows {
		if w.ID == id {
			w.PosX = posX
			w.PosY = posY
			w.Width = width
			w.Height = height
			return
		}
	}
}

// MoveTab moves a tab to a different window at the given index.
// Negative or out-of-range index appends to the destination's tail.
func (s *Session) MoveTab(tabID, toWindowID uint32, index int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Locate source window + remove the tab.
	var srcWin *Window
	for _, w := range s.windows {
		for i, id := range w.TabIDs {
			if id == tabID {
				srcWin = w
				w.TabIDs = append(w.TabIDs[:i], w.TabIDs[i+1:]...)
				if w.FocusedTabID == tabID {
					if len(w.TabIDs) > 0 {
						w.FocusedTabID = w.TabIDs[0]
					} else {
						w.FocusedTabID = 0
					}
				}
				break
			}
		}
		if srcWin != nil {
			break
		}
	}
	if srcWin == nil {
		return // tab not found
	}
	// Locate destination window.
	var dst *Window
	for _, w := range s.windows {
		if w.ID == toWindowID {
			dst = w
			break
		}
	}
	if dst == nil {
		// Bad destination — re-insert in source so we don't lose
		// the tab.
		srcWin.TabIDs = append(srcWin.TabIDs, tabID)
		if srcWin.FocusedTabID == 0 {
			srcWin.FocusedTabID = tabID
		}
		return
	}
	if index < 0 || index > len(dst.TabIDs) {
		index = len(dst.TabIDs)
	}
	dst.TabIDs = append(dst.TabIDs, 0)
	copy(dst.TabIDs[index+1:], dst.TabIDs[index:])
	dst.TabIDs[index] = tabID
	if dst.FocusedTabID == 0 {
		dst.FocusedTabID = tabID
	}
	s.revision++
}

// CloseTab removes a tab from the session and kills its PTY child.
// Updates whichever window contained it. No-op if id isn't found.
func (s *Session) CloseTab(id uint32) {
	s.mu.Lock()
	t, ok := s.tabs[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.tabs, id)
	if t.Name != "" {
		delete(s.tabsByName, t.Name)
	}
	for _, w := range s.windows {
		for i, tid := range w.TabIDs {
			if tid == id {
				w.TabIDs = append(w.TabIDs[:i], w.TabIDs[i+1:]...)
				if w.FocusedTabID == id {
					if len(w.TabIDs) > 0 {
						w.FocusedTabID = w.TabIDs[0]
					} else {
						w.FocusedTabID = 0
					}
				}
				break
			}
		}
	}
	if s.focusedTabID == id {
		// Pick another tab if any exist.
		s.focusedTabID = 0
		for _, w := range s.windows {
			if w.FocusedTabID != 0 {
				s.focusedTabID = w.FocusedTabID
				break
			}
		}
	}
	s.revision++
	s.mu.Unlock()
	t.Term.Close()
}
