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

	"github.com/LXXero/xerotty/internal/handoff"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/supervise"
)

// stateDebounce coalesces topology pushes: a window resize or a
// burst of tab creations should cost one frame, not one per change.
const stateDebounce = 100 * time.Millisecond

type supervision struct {
	client *supervise.Client
	mu     sync.Mutex
	timer  *time.Timer
}

// SetSupervisor attaches the control channel to the supervisor and
// starts relaying the shell exits it reports.
func (d *Daemon) SetSupervisor(c *supervise.Client) {
	d.mu.Lock()
	d.sup = &supervision{client: c}
	d.mu.Unlock()
	go d.relaySupervisorExits(c)
}

func (d *Daemon) supervisor() *supervision {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sup
}

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

// pushState schedules a debounced topology push. Safe to call under
// any lock: the snapshot runs later on the timer goroutine.
func (d *Daemon) pushState() {
	s := d.supervisor()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		return // one already pending; it will see the latest state
	}
	s.timer = time.AfterFunc(stateDebounce, func() {
		s.mu.Lock()
		s.timer = nil
		s.mu.Unlock()
		st := d.SnapshotState()
		b, err := st.MarshalMsg(nil)
		if err != nil {
			log.Printf("xerottyd: marshal state for supervisor: %v", err)
			return
		}
		if err := s.client.SendState(b); err != nil {
			log.Printf("xerottyd: send state to supervisor: %v", err)
		}
	})
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

// SnapshotState captures the default session's topology and per-tab
// metadata as handoff.State WITHOUT screens, fds or releasing
// anything — what the supervisor keeps on hand to resume from. The
// hot-upgrade SerializeUpgrade builds on the same per-tab metadata.
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
