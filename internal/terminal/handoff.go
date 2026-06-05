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
	"syscall"

	"github.com/charmbracelet/x/vt"
	uv "github.com/charmbracelet/ultraviolet"
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
	sb := t.Emu.Scrollback()
	if sb == nil {
		return
	}
	lines := sb.Lines()
	wrote := 0
	for _, line := range lines {
		if err := disk.Append(line); err != nil {
			break
		}
		wrote++
	}
	if wrote == 0 {
		return
	}
	if wrote == len(lines) {
		// vt treats SetScrollbackSize(<=0) as "use default", so a
		// shrink-to-zero dance would silently keep the lines (and
		// double-count them against the disk copy). ClearScrollback
		// is the real "drop the ring".
		t.Emu.ClearScrollback()
	} else {
		// Partial flush (disk write failed midway): drop only the
		// prefix we actually wrote, mirrorScrollback-style.
		t.Emu.SetScrollbackSize(len(lines) - wrote)
		t.Emu.SetScrollbackSize(int(^uint(0) >> 1))
	}
}

// ReleaseForHandoff stops this Terminal's goroutines and surrenders
// its process plumbing WITHOUT killing the child or destroying the
// PTY: the returned ptmx is a dup of the master (the original fd is
// closed to unblock the reader), childPID is the live shell, disk is
// the scrollback store (ownership transfers — Close on this
// Terminal will no longer touch it).
//
// The released Terminal is dead: IsClosed() reports true, no
// callbacks fire meaningfully again. One wrinkle: a cmd-based
// waitChild goroutine stays parked in cmd.Wait() until the child
// really dies — irrelevant in the real upgrade flow (exec replaces
// the process image) and harmless in tests (the goroutine exits
// when the adopted Terminal kills the child).
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
	// ORIGINAL ptmx (unblocks readPTY's blocking Read).
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

	// Scrollback store, already rebuilt (same object in-process, or
	// AdoptDiskScrollback(fd) across an exec). nil = none.
	Disk *DiskScrollback
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
		DataCh:      make(chan struct{}, 1),
		cols:        cols,
		rows:        rows,
		ExitCode:    -1,
		done:        make(chan struct{}),
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

	t.installCallbacks()

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
func AdoptDiskScrollback(f *os.File, offsets []int64, size int64) *DiskScrollback {
	return &DiskScrollback{f: f, offsets: offsets, size: size}
}
