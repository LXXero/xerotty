// Supervisor plumbing, daemon-child side (internal/supervise). The
// daemon hands every tab's PTY master + scrollback file to the
// supervisor as it spawns them, streams its topology so the
// supervisor can write a resume handoff if the daemon dies, and
// relays the exits the supervisor observes for tabs the daemon did
// not spawn itself (crash-resumed "foreign" children). See
// docs/CRASH_RESTORE_PLAN.md.

package daemon

import (
	"log"
	"sync"
	"time"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/LXXero/xerotty/internal/handoff"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/supervise"
	"github.com/LXXero/xerotty/internal/terminal"
)

// The state the daemon streams to its supervisor is what a crash
// resume restores, so it has to stay close to current, not only to the
// last topology change: a resume from an old snapshot came back with
// app cursor keys and scroll regions as they were hours earlier. Three
// triggers, one coalescing timer:
//
//   - stateDebounce after a topology change or a change to replay
//     state (a mode, the scroll margins, a charset): these decide how
//     keys are encoded and where output lands, so they go out fast;
//   - outputStateDelay after PTY output, so the screen is at most
//     that stale;
//   - never sooner than minStateGap after the previous push, which
//     caps the rate when an app toggles a mode on every frame (many
//     hide the cursor around each redraw).
//
// Each push is one local socketpair write of every tab's screen.
const (
	stateDebounce    = 100 * time.Millisecond
	outputStateDelay = time.Second
	minStateGap      = 500 * time.Millisecond
)

type supervision struct {
	client *supervise.Client
	mu     sync.Mutex
	timer  *time.Timer // pending push; nil when none
	due    time.Time   // when the pending push fires
	last   time.Time   // when the previous push started
}

// SetSupervisor attaches the control channel to the supervisor and
// starts relaying the shell exits it reports.
func (d *Daemon) SetSupervisor(c *supervise.Client) {
	d.sup.Store(&supervision{client: c})
	go d.relaySupervisorExits(c)
}

// supervisor is lock-free: pushStateAfterOutput asks on every PTY read.
func (d *Daemon) supervisor() *supervision { return d.sup.Load() }

// SupervisorExecFD returns a non-cloexec duplicate of the control
// channel for an exec-in-place upgrade to inherit, or -1 when the
// daemon is unsupervised.
func (d *Daemon) SupervisorExecFD() int {
	s := d.supervisor()
	if s == nil {
		return -1
	}
	fd, err := s.client.ExecFD()
	if err != nil {
		log.Printf("xerottyd: supervisor control fd for exec: %v", err)
		return -1
	}
	return fd
}

// tabSpawned hands a freshly spawned tab's plumbing to the supervisor.
func (d *Daemon) tabSpawned(t *Tab) {
	s := d.supervisor()
	if s == nil {
		return
	}
	ptmx, disk, err := t.Term.DupFiles()
	if err != nil {
		log.Printf("xerottyd: tab %d: dup files for supervisor: %v", t.ID, err)
		return
	}
	fds := []int{int(ptmx.Fd())}
	if disk != nil {
		fds = append(fds, int(disk.Fd()))
	}
	if err := s.client.SendTab(t.ID, t.Term.ChildPID(), fds...); err != nil {
		log.Printf("xerottyd: tab %d: send to supervisor: %v", t.ID, err)
	}
	_ = ptmx.Close()
	if disk != nil {
		_ = disk.Close()
	}
	d.pushState()
}

// tabGone tells the supervisor to drop a tab's plumbing (closed, or
// its shell exited).
func (d *Daemon) tabGone(id uint32) {
	s := d.supervisor()
	if s == nil {
		return
	}
	if err := s.client.SendTabGone(id); err != nil {
		log.Printf("xerottyd: tab %d: send gone to supervisor: %v", id, err)
	}
	d.pushState()
}

// pushState schedules a prompt state push (topology, names, titles,
// replay state). Safe to call under any lock: the snapshot runs later
// on the timer goroutine.
func (d *Daemon) pushState() { d.schedulePush(stateDebounce) }

// pushStateAfterOutput schedules a push that refreshes the screens.
func (d *Daemon) pushStateAfterOutput() { d.schedulePush(outputStateDelay) }

func (d *Daemon) schedulePush(delay time.Duration) {
	s := d.supervisor()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	at := now.Add(delay)
	if earliest := s.last.Add(minStateGap); at.Before(earliest) {
		at = earliest
	}
	if s.timer != nil {
		if !at.Before(s.due) {
			return // the pending push fires first and sees the latest state
		}
		if !s.timer.Stop() {
			return // already firing
		}
	}
	s.due = at
	s.timer = time.AfterFunc(at.Sub(now), func() {
		s.mu.Lock()
		s.timer = nil
		s.last = time.Now()
		s.mu.Unlock()
		d.sendState(s)
	})
}

// sendState snapshots the session and hands it to the supervisor.
func (d *Daemon) sendState(s *supervision) {
	if d.suspended.Load() {
		// Upgrade quiesce: the terminals are being released, and the
		// handoff that follows supersedes anything sent now.
		return
	}
	st := d.SnapshotState()
	b, err := st.MarshalMsg(nil)
	if err != nil {
		log.Printf("xerottyd: marshal state for supervisor: %v", err)
		return
	}
	if len(b) > supervise.MaxFrame {
		// Too many screens for one frame. Topology and replay state
		// still go; a resume then shows blank screens until apps
		// repaint, as it did before screens were streamed.
		for i := range st.Tabs {
			st.Tabs[i].Screen = nil
		}
		if b, err = st.MarshalMsg(nil); err != nil {
			log.Printf("xerottyd: marshal state for supervisor: %v", err)
			return
		}
		log.Printf("xerottyd: state with screens exceeds %d bytes; sent without screens", supervise.MaxFrame)
	}
	if err := s.client.SendState(b); err != nil {
		log.Printf("xerottyd: send state to supervisor: %v", err)
	}
}

// relaySupervisorExits routes supervisor exit notices to the foreign
// tabs they belong to. Exits of our own children are ignored here:
// waitpid already reported them.
func (d *Daemon) relaySupervisorExits(c *supervise.Client) {
	for m := range c.Exits() {
		sess := d.SessionByName("default")
		if sess == nil {
			continue
		}
		for _, t := range sess.Tabs() {
			if t.Term.ForeignChild() && t.Term.ChildPID() == m.PID {
				t.Term.NotifyExit(m.Code)
			}
		}
	}
}

// SnapshotState captures the default session as handoff.State
// without fds or releasing anything — what the supervisor keeps on
// hand to resume from after a crash. Per tab it carries the same
// metadata and emulator state as the hot-upgrade SerializeUpgrade
// (tabMeta + tabEmuState); only the plumbing differs: no fds, and no
// scrollback offset index (the resumed daemon rebuilds it from the
// file, which is written through and so is current).
func (d *Daemon) SnapshotState() *handoff.State {
	st := &handoff.State{
		Version:      handoff.Version,
		WireListenFD: -1,
		MCPListenFD:  -1,
		SocketPath:   d.socketPath,
	}
	d.mu.Lock()
	st.InstanceID = d.instanceID
	d.mu.Unlock()
	sess := d.SessionByName("default")
	if sess == nil {
		return st
	}
	tabs := sess.snapshotTopologyInto(st)
	for _, t := range tabs {
		select {
		case <-t.Exited:
			continue
		default:
		}
		ts := tabMeta(t)
		tabEmuState(t, &ts)
		ts.PtmxFD = -1
		ts.DiskFD = -1
		st.Tabs = append(st.Tabs, ts)
	}
	return st
}

// snapshotTopologyInto fills the session-level handoff fields and
// returns the live tab list, all under one lock hold.
func (s *Session) snapshotTopologyInto(st *handoff.State) []*Tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.NextTabID = s.nextTabID
	st.NextWindowID = s.nextWinID
	st.Revision = s.revision
	st.Windows = st.Windows[:0]
	for _, w := range s.windows {
		st.Windows = append(st.Windows, protocol.WindowInfo{
			ID: w.ID, PosX: w.PosX, PosY: w.PosY,
			Width: w.Width, Height: w.Height,
			TabIDs: append([]uint32(nil), w.TabIDs...), FocusedTabID: w.FocusedTabID,
		})
	}
	tabs := make([]*Tab, 0, len(s.tabs))
	for _, t := range s.tabs {
		tabs = append(tabs, t)
	}
	return tabs
}

// tabMeta is the per-tab handoff metadata that needs no quiesce:
// identity, size, activity clock, child pid and whether it is ours.
func tabMeta(t *Tab) handoff.TabState {
	term := t.Term
	return handoff.TabState{
		ID: t.ID, Name: t.Name(), Title: t.Title(),
		CWD:  term.GetCWD(),
		Cols: term.Width(), Rows: term.Height(),
		ChildPID:     term.ChildPID(),
		ForeignChild: term.ForeignChild(),
		LastOutputAt: term.LastOutputUnixNano(),
		LastInputAt:  term.LastInputUnixNano(),
	}
}

// tabEmuState fills ts with the tab's emulator state: screen, cursor,
// modes, scroll margins, charsets. The crash-resume state and the
// in-place upgrade handoff both come through here, so a resume after a
// crash replays what an upgrade would
// (TestCrashStateMatchesUpgradeHandoff holds them to it).
func tabEmuState(t *Tab, ts *handoff.TabState) {
	es := t.Term.CaptureEmuState()
	ts.Screen = trimTrailingBlanks(cellsToProto(es.Screen))
	ts.CursorRow, ts.CursorCol = es.CursorRow, es.CursorCol
	ts.CursorStyle, ts.CursorBlink, ts.StyleSet = es.CursorStyle, es.CursorBlink, es.CursorStyleSet
	ts.AppCursor = es.AppCursor
	ts.DECModesSet, ts.DECModesReset = es.DECModesSet, es.DECModesReset
	ts.ANSIModesSet, ts.ANSIModesReset = es.ANSIModesSet, es.ANSIModesReset
	if m := es.Margins; m != nil {
		ts.Margins = &handoff.Margins{Top: m.Top, Bottom: m.Bottom, Left: m.Left, Right: m.Right}
	}
	if cs := es.Charsets; cs != nil {
		ts.Charsets = &handoff.Charsets{G: string(cs.G[:]), GL: cs.GL, GR: cs.GR}
	}
}

// trimTrailingBlanks drops each row's trailing default blank cells:
// a fresh emulator already holds exactly those, and a mostly empty
// shell screen shrinks to a fraction of its size on the wire.
func trimTrailingBlanks(rows [][]protocol.Cell) [][]protocol.Cell {
	blank := cellFromUV(&uv.EmptyCell)
	for i, row := range rows {
		n := len(row)
		for n > 0 && (row[n-1] == blank || row[n-1] == protocol.Cell{}) {
			n--
		}
		rows[i] = row[:n]
	}
	return rows
}

// emuSpec is tabEmuState's inverse, for the adopt side.
func emuSpec(ts handoff.TabState) (*terminal.Margins, *terminal.Charsets) {
	var m *terminal.Margins
	if hm := ts.Margins; hm != nil {
		m = &terminal.Margins{Top: hm.Top, Bottom: hm.Bottom, Left: hm.Left, Right: hm.Right}
	}
	var cs *terminal.Charsets
	if hc := ts.Charsets; hc != nil && len(hc.G) == 4 {
		cs = &terminal.Charsets{GL: hc.GL, GR: hc.GR}
		copy(cs.G[:], hc.G)
	}
	return m, cs
}
