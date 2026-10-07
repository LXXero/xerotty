// Hot-upgrade support: surrendering a live Terminal's process
// plumbing (ReleaseForHandoff) and rebuilding a Terminal around
// inherited plumbing (Adopt). See docs/UPGRADE_PLAN.md.
//
// The split of responsibilities: this file moves FDs and replays
// emulator-visible state; converting cells to/from the handoff
// file's wire shapes stays daemon-side (it owns protocol.Cell).

package terminal

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// FlushScrollbackToDisk force-evicts EVERY in-memory scrollback line
// to the disk store, so handoff state only has to carry the disk fd
// + offset index. No-op without a disk store (non-daemon configs).
// Part of upgrade quiesce; runs under publishMu so it can't tear
// against a concurrent PTY write.
func (t *Terminal) FlushScrollbackToDisk() {
	t.publishMu.Lock()
	defer t.publishMu.Unlock()
	t.mu.Lock()
	disk := t.disk
	t.mu.Unlock()
	if disk == nil {
		return
	}
	// Write-through means the ring is at most a few lines behind the
	// disk; catch it up, then drop the cache. ClearScrollback is the
	// real "drop the ring" (vt treats SetScrollbackSize(<=0) as "use
	// default" and would keep the lines, double-counting them
	// against the disk copy).
	mirrored, memLen := t.mirrorNewLines(disk)
	if mirrored != memLen {
		return // disk write failed; keep the unwritten tail in memory
	}
	t.Emu.ClearScrollback()
	t.mu.Lock()
	t.memMirrored = 0
	t.mu.Unlock()
}

// ReleaseForHandoff stops this Terminal's goroutines and surrenders
// its process plumbing WITHOUT killing the child or destroying the
// PTY: the returned ptmx is a dup of the master, childPID is the live
// shell, disk is the scrollback store (ownership transfers — Close on
// this Terminal will no longer touch it).
//
// The released Terminal is dead: IsClosed() reports true, no
// callbacks fire meaningfully again. Two wrinkles, both erased by the
// exec in a real upgrade (it replaces the process image) but live for
// any IN-PROCESS handoff:
//
//   - readPTY is NOT stopped. The master is a blocking fd, so closing
//     the original cannot interrupt the read(2) the reader is parked
//     in; it lingers on the still-open description and will swallow
//     the NEXT chunk of PTY output into this dead emulator before it
//     notices done and exits. An in-process adopter must wait for
//     readerDone first, or it races that reader for its own output.
//   - a cmd-based waitChild goroutine stays parked in cmd.Wait()
//     until the child really dies (it exits when the adopted Terminal
//     kills the child).
func (t *Terminal) ReleaseForHandoff() (ptmx *os.File, childPID int, disk *DiskScrollback, err error) {
	// Serialize against a mid-write snapshot/flush.
	t.publishMu.Lock()
	defer t.publishMu.Unlock()

	t.mu.Lock()
	if t.closed || t.released {
		t.mu.Unlock()
		return nil, 0, nil, fmt.Errorf("terminal: already closed/released")
	}
	t.released = true
	t.closed = true // IsClosed() true; Close() becomes a no-op via closeOnce below
	disk = t.disk
	t.disk = nil
	switch {
	case t.cmd != nil && t.cmd.Process != nil:
		childPID = t.cmd.Process.Pid
	case t.adoptedProc != nil:
		childPID = t.adoptedProc.Pid
	}
	t.mu.Unlock()

	dupFD, err := syscall.Dup(int(t.ptmx.Fd()))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("terminal: dup ptmx: %w", err)
	}
	ptmx = os.NewFile(uintptr(dupFD), "ptmx-handoff")

	// Tear down the pipelines exactly like Close, minus the kill:
	// burn the closeOnce so a later Close() can't double-run, close
	// done + the emulator input pipe (stops readEmu), close the
	// ORIGINAL ptmx. That close does NOT unblock readPTY (blocking fd:
	// the real close is deferred until its read(2) returns) — see the
	// doc comment above.
	t.closeOnce.Do(func() {
		close(t.done)
		if pw, ok := t.Emu.InputPipe().(*io.PipeWriter); ok {
			_ = pw.CloseWithError(io.EOF)
		}
		_ = t.ptmx.Close()
	})
	return ptmx, childPID, disk, nil
}

// AdoptSpec is everything Adopt needs to rebuild a Terminal around
// inherited plumbing.
type AdoptSpec struct {
	Ptmx     *os.File
	ChildPID int
	Cols     int
	Rows     int

	// Emulator-visible state to replay.
	Screen               [][]uv.Cell // viewport rows; zero-width placeholder cells are skipped
	CursorRow, CursorCol int
	AppCursor            bool
	CursorStyle          uint8
	CursorBlink          bool
	CursorStyleSet       bool

	// Modes the app had explicitly set/reset (ModeSnapshot's lists),
	// replayed as DECSET/DECRST and SM/RM before the screen cells so
	// a 1049 puts them on the alt buffer. All nil = a handoff written
	// by a daemon that predates mode capture; AppCursor is the only
	// mode known then.
	DECModesSet, DECModesReset   []int
	ANSIModesSet, ANSIModesReset []int

	// Scrollback store, already rebuilt (same object in-process, or
	// AdoptDiskScrollback(fd) across an exec). nil = none.
	Disk *DiskScrollback

	// Activity clock carried across the upgrade (unix nanos). 0 =
	// unknown (pre-this-version handoff) → Adopt reseeds to now so the
	// tab reads fresh rather than 1970.
	LastOutputAt int64
	LastInputAt  int64

	// ForeignChild: the shell is not this process's child (crash
	// resume). The terminal then waits on NotifyExit instead of
	// waitpid. Kill still works — signals don't need parentage.
	ForeignChild bool
}

// Adopt rebuilds a Terminal around an inherited PTY master + child
// PID, replaying the snapshot into a fresh emulator. The child must
// be a child of THIS process (exec-in-place guarantees it; anything
// else breaks waitpid).
func Adopt(spec AdoptSpec) (*Terminal, error) {
	if spec.Ptmx == nil || spec.ChildPID <= 0 {
		return nil, fmt.Errorf("terminal: adopt needs ptmx + child pid")
	}
	proc, err := os.FindProcess(spec.ChildPID)
	if err != nil {
		return nil, fmt.Errorf("terminal: find child %d: %w", spec.ChildPID, err)
	}
	cols, rows := spec.Cols, spec.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	emu := vt.NewSafeEmulator(cols, rows)
	t := &Terminal{
		Emu:         emu,
		ptmx:        spec.Ptmx,
		adoptedProc: proc,
		foreign:     spec.ForeignChild,
		DataCh:      make(chan struct{}, 1),
		cols:        cols,
		rows:        rows,
		ExitCode:    -1,
		done:        make(chan struct{}),
		readerDone:  make(chan struct{}),
	}
	if spec.ForeignChild {
		t.exitCh = make(chan int, 1)
	}
	// Daemon-hosted scrollback shape: vt's ring uncapped, our disk
	// mirror does the evicting (mirrors applyScrollbackConfig's
	// "unlimited" branch, minus creating a fresh store).
	t.Emu.SetScrollbackSize(int(^uint(0) >> 1))
	t.liveWindow = liveWindowDefault
	t.disk = spec.Disk

	t.cursorVisible.Store(true)
	t.cursorStyle.Store(packCursorStyle(spec.CursorStyle, spec.CursorBlink))
	t.cursorStyleSet.Store(spec.CursorStyleSet)

	// Restore the activity clock across the upgrade; reseed to now if
	// the handoff didn't carry it (pre-this-version daemon).
	now := time.Now().UnixNano()
	lastOut, lastIn := spec.LastOutputAt, spec.LastInputAt
	if lastOut == 0 {
		lastOut = now
	}
	if lastIn == 0 {
		lastIn = now
	}
	t.lastOutputAt.Store(lastOut)
	t.lastInputAt.Store(lastIn)

	t.installCallbacks()

	// Replay the app's mode state FIRST: 1049 must switch to the alt
	// buffer before the cells land on it, and 25l must hide the
	// cursor of the screen that will actually be shown. Ascending
	// mode order gives exactly that. The callbacks installed above
	// re-record each mode, so the next handoff carries it forward
	// again; nothing else ever re-sends these (apps don't repeat
	// DECSETs on SIGWINCH), which is why a daemon swap used to drop
	// bracketed paste and mouse reporting for the rest of the tab's
	// life.
	replayModes(emu, spec)

	// Replay the snapshot BEFORE the reader starts, so fresh PTY
	// output lands on top of the restored screen, never under it.
	for r, row := range spec.Screen {
		for c := range row {
			cell := &row[c]
			if cell.Width == 0 && cell.Content == "" {
				continue // wide-cell placeholder; its glyph wrote it
			}
			emu.SetCell(c, r, cell)
		}
	}
	// Cursor + DECCKM via escape replay (same trick as
	// daemonsource.applyCursor): the emulator parses them and its
	// internal state — and our mode callbacks — both line up.
	if spec.AppCursor {
		_, _ = emu.Write([]byte("\x1b[?1h"))
	}
	_, _ = emu.Write([]byte(fmt.Sprintf("\x1b[%d;%dH", spec.CursorRow+1, spec.CursorCol+1)))

	t.startPipelines()
	return t, nil
}

// replayModes writes one SM/RM (ANSI) or DECSET/DECRST (DEC private)
// sequence per recorded mode into the fresh emulator, ascending by
// mode number within each family.
func replayModes(emu *vt.SafeEmulator, spec AdoptSpec) {
	type entry struct {
		mode int
		set  bool
	}
	merge := func(set, reset []int) []entry {
		var out []entry
		for _, m := range set {
			out = append(out, entry{m, true})
		}
		for _, m := range reset {
			out = append(out, entry{m, false})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].mode < out[j].mode })
		return out
	}
	var sb strings.Builder
	for _, e := range merge(spec.ANSIModesSet, spec.ANSIModesReset) {
		fmt.Fprintf(&sb, "\x1b[%d%c", e.mode, hOrL(e.set))
	}
	for _, e := range merge(spec.DECModesSet, spec.DECModesReset) {
		fmt.Fprintf(&sb, "\x1b[?%d%c", e.mode, hOrL(e.set))
	}
	if sb.Len() > 0 {
		_, _ = emu.Write([]byte(sb.String()))
	}
}

func hOrL(set bool) byte {
	if set {
		return 'h'
	}
	return 'l'
}

// Handoff exports the disk store's plumbing for serialization: the
// open file (an unlinked temp inode that only exists through this
// fd), the line-offset index, and the append position. The store
// stays fully usable afterwards.
func (d *DiskScrollback) Handoff() (f *os.File, offsets []int64, size int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]int64, len(d.offsets))
	copy(out, d.offsets)
	return d.f, out, d.size
}

// AdoptDiskScrollback rebuilds a store around an inherited fd + its
// serialized offset index — the across-exec counterpart of Handoff.
// size < 0 means the index was not carried (crash resume): it is
// rebuilt by scanning the file's length-prefixed records.
func AdoptDiskScrollback(f *os.File, offsets []int64, size int64) *DiskScrollback {
	d := &DiskScrollback{f: f, offsets: offsets, size: size}
	if size < 0 {
		d.offsets = nil
		d.size = 0
		if err := d.RebuildIndex(); err != nil {
			fmt.Fprintf(os.Stderr, "xerotty: scrollback index rebuild: %v (history truncated at the last readable record)\n", err)
		}
	}
	return d
}
