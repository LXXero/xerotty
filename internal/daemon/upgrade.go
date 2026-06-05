// Hot-upgrade support, daemon side: SerializeUpgrade tears the
// "default" session down to handoff state (the point of no return —
// terminals are released), ResumeFromHandoff rebuilds it in the new
// exec image. The exec itself lives in internal/runner, which owns
// argv. See docs/UPGRADE_PLAN.md.

package daemon

import (
	"fmt"
	"os"
	"syscall"

	"github.com/LXXero/xerotty/internal/handoff"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/terminal"
	uv "github.com/charmbracelet/ultraviolet"
)

// DisconnectClients force-closes every attached wire client — the
// quiesce step before SerializeUpgrade. Their publishLoops exit on
// the closed conns, so nothing snapshots a terminal mid-release.
// Clients treat it like any daemon restart and reconnect-loop.
func (d *Daemon) DisconnectClients() {
	d.mu.Lock()
	conns := make([]*clientConn, 0, len(d.clients))
	for c := range d.clients {
		conns = append(conns, c)
	}
	d.mu.Unlock()
	for _, c := range conns {
		c.shutdown()
	}
}

// SerializeUpgrade captures the default session as handoff state and
// RELEASES every tab's terminal (goroutines stopped, plumbing
// surrendered). After this returns successfully the daemon must not
// serve anything — the only sane next step is exec. Caller is
// responsible for having stopped listeners + client connections
// FIRST (a publishLoop snapshotting mid-release would read a dead
// emulator). The returned keepFiles are the live *os.File handles
// whose fd numbers the state records — the caller must hold them
// (runtime.KeepAlive) through the exec: os.File finalizers CLOSE
// their fds at GC, and after release nothing else references these
// files, so dropping them turns the handoff fds into dead numbers
// before the new image ever runs.
//
// Failure mode honesty: per-tab work is snapshot-then-release, so an
// error BEFORE any release leaves the daemon intact; an error after
// the first release means those tabs are already unservable and the
// caller should proceed with the tabs that did serialize (partial
// handoff beats lost handoff).
func (d *Daemon) SerializeUpgrade() (*handoff.State, []*os.File, error) {
	sess := d.SessionByName("default")
	if sess == nil {
		return nil, nil, fmt.Errorf("upgrade: no default session")
	}

	st := &handoff.State{
		InstanceID:   d.instanceID,
		WireListenFD: -1,
		MCPListenFD:  -1,
		SocketPath:   d.socketPath,
	}

	sess.mu.Lock()
	st.NextTabID = sess.nextTabID
	st.NextWindowID = sess.nextWinID
	st.Revision = sess.revision
	for _, w := range sess.windows {
		st.Windows = append(st.Windows, protocol.WindowInfo{
			ID: w.ID, PosX: w.PosX, PosY: w.PosY,
			Width: w.Width, Height: w.Height,
			TabIDs: append([]uint32(nil), w.TabIDs...), FocusedTabID: w.FocusedTabID,
		})
	}
	tabs := make([]*Tab, 0, len(sess.tabs))
	for _, t := range sess.tabs {
		tabs = append(tabs, t)
	}
	sess.mu.Unlock()

	var keepFiles []*os.File
	for _, t := range tabs {
		select {
		case <-t.Exited:
			// Dead shell — nothing to carry; the tab's corpse is not
			// worth a handoff entry (clients re-learn topology anyway).
			continue
		default:
		}
		term := t.Term

		// Snapshot BEFORE release (release kills the emulator feed).
		term.FlushScrollbackToDisk()
		screen := term.SnapshotViewport()
		pos := term.CursorPosition()
		style, blink, styleSet := term.CursorStyle()
		ts := handoff.TabState{
			ID: t.ID, Name: t.Name, Title: t.Title(),
			CWD:  term.GetCWD(),
			Cols: term.Width(), Rows: term.Height(),
			CursorRow: pos.Y, CursorCol: pos.X,
			CursorStyle: style, CursorBlink: blink, StyleSet: styleSet,
			AppCursor: term.AppCursorMode(),
			Screen:    cellsToProto(screen),
			DiskFD:    -1,
		}

		ptmx, pid, disk, err := term.ReleaseForHandoff()
		if err != nil {
			// Already closed under us (child raced an exit) — skip.
			continue
		}
		ts.PtmxFD = int(ptmx.Fd())
		ts.ChildPID = pid
		keepFiles = append(keepFiles, ptmx)
		if disk != nil {
			f, offsets, size := disk.Handoff()
			ts.DiskFD = int(f.Fd())
			ts.DiskOffsets = offsets
			ts.DiskSize = size
			keepFiles = append(keepFiles, f)
		}
		st.Tabs = append(st.Tabs, ts)
	}
	return st, keepFiles, nil
}

// ResumeFromHandoff rebuilds the default session from handoff state
// in the new exec image: terminals adopted around inherited fds +
// child pids, windows/counters/InstanceID restored, and every child
// poked with SIGWINCH so full-screen apps repaint state the snapshot
// can't carry (alt screen, etc.).
func (d *Daemon) ResumeFromHandoff(st *handoff.State) error {
	d.mu.Lock()
	d.instanceID = st.InstanceID
	d.mu.Unlock()

	sess := d.session("default")
	sess.mu.Lock()
	sess.nextTabID = st.NextTabID
	sess.nextWinID = st.NextWindowID
	sess.revision = st.Revision
	for _, wi := range st.Windows {
		sess.windows = append(sess.windows, &Window{
			ID: wi.ID, PosX: wi.PosX, PosY: wi.PosY,
			Width: wi.Width, Height: wi.Height,
			TabIDs: append([]uint32(nil), wi.TabIDs...), FocusedTabID: wi.FocusedTabID,
		})
	}
	sess.mu.Unlock()

	var firstErr error
	resumed := map[uint32]bool{}
	for _, ts := range st.Tabs {
		if err := sess.restoreTab(ts); err != nil {
			// One broken tab must not sink the rest.
			if firstErr == nil {
				firstErr = fmt.Errorf("resume tab %d: %w", ts.ID, err)
			}
			continue
		}
		resumed[ts.ID] = true
		// Repaint wiggle. The PTY size is already correct, so a
		// plain SIGWINCH is enough for most TUIs; apps that ignore
		// it redraw on their next output anyway.
		_ = syscall.Kill(ts.ChildPID, syscall.SIGWINCH)
	}
	// Drop window references to tabs that didn't make it so the
	// topology snapshot stays truthful.
	sess.mu.Lock()
	for _, w := range sess.windows {
		kept := w.TabIDs[:0]
		for _, id := range w.TabIDs {
			if resumed[id] {
				kept = append(kept, id)
			}
		}
		w.TabIDs = kept
	}
	sess.mu.Unlock()
	return firstErr
}

// restoreTab is NewTab's resume-path sibling: same Tab shape, same
// callback wiring, but the Terminal is adopted, not spawned, and the
// ID is preserved.
func (s *Session) restoreTab(ts handoff.TabState) error {
	ptmx := os.NewFile(uintptr(ts.PtmxFD), "ptmx-resume")
	if ptmx == nil {
		return fmt.Errorf("bad ptmx fd %d", ts.PtmxFD)
	}
	var disk *terminal.DiskScrollback
	if ts.DiskFD >= 0 {
		disk = terminal.AdoptDiskScrollback(
			os.NewFile(uintptr(ts.DiskFD), "scrollback-resume"),
			ts.DiskOffsets, ts.DiskSize)
	}
	term, err := terminal.Adopt(terminal.AdoptSpec{
		Ptmx: ptmx, ChildPID: ts.ChildPID,
		Cols: ts.Cols, Rows: ts.Rows,
		Screen:    protoToUV(ts.Screen),
		CursorRow: ts.CursorRow, CursorCol: ts.CursorCol,
		AppCursor:   ts.AppCursor,
		CursorStyle: ts.CursorStyle, CursorBlink: ts.CursorBlink, CursorStyleSet: ts.StyleSet,
		Disk: disk,
	})
	if err != nil {
		return err
	}
	t := &Tab{
		ID:     ts.ID,
		Name:   ts.Name,
		Term:   term,
		Exited: make(chan struct{}),
	}
	t.SetTitle(ts.Title)
	s.mu.Lock()
	s.tabs[t.ID] = t
	if ts.Name != "" {
		s.tabsByName[ts.Name] = t.ID
	}
	s.mu.Unlock()
	s.wireTabCallbacks(t)
	return nil
}

// cellsToProto converts a uv snapshot to wire cells (handoff reuses
// the wire cell encoding).
func cellsToProto(grid [][]uv.Cell) [][]protocol.Cell {
	out := make([][]protocol.Cell, len(grid))
	for r, row := range grid {
		pr := make([]protocol.Cell, len(row))
		for c := range row {
			pr[c] = cellFromUV(&row[c])
		}
		out[r] = pr
	}
	return out
}

// protoToUV is cellsToProto's inverse — the daemon-side twin of
// daemonsource.uvCellFromProto, with the same placeholder rule:
// Width 0 stays a true zero cell so Adopt's replay can skip it.
func protoToUV(grid [][]protocol.Cell) [][]uv.Cell {
	out := make([][]uv.Cell, len(grid))
	for r, row := range grid {
		ur := make([]uv.Cell, len(row))
		for c := range row {
			ur[c] = cellToUV(row[c])
		}
		out[r] = ur
	}
	return out
}
