package terminal

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
)

// screenContains scans the viewport for a needle.
func screenContains(t *Terminal, needle string) bool {
	for _, row := range t.SnapshotViewport() {
		var sb strings.Builder
		for i := range row {
			if row[i].Content != "" {
				sb.WriteString(row[i].Content)
			}
		}
		if strings.Contains(sb.String(), needle) {
			return true
		}
	}
	return false
}

// retireOldReader makes an in-process handoff faithful to the real one.
// A real upgrade execs, which kills the old image's readPTY. In-process
// nothing does: the PTY master is a blocking fd, so ReleaseForHandoff's
// Close can't interrupt the read(2) the old reader is parked in — it
// lingers and STEALS the adopter's next chunk of output into the dead
// emulator. That was the long-running flake here: usually it ate only
// the command echo (nothing asserts on it), but sometimes the marker
// ("never appeared") or the ESC[?1049l ("mode changes not tracked").
//
// A single space is the gentlest poke that produces output: the tty (or
// readline) echoes one byte — no newline, no prompt, no mode traffic —
// which the old reader, still the PTY's only reader, swallows on its way
// out. The leading space it leaves on the shell's input line is inert.
func retireOldReader(t *testing.T, old *Terminal, ptmx *os.File) {
	t.Helper()
	if _, err := ptmx.Write([]byte(" ")); err != nil {
		t.Fatalf("poke released pty: %v", err)
	}
	select {
	case <-old.readerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("released terminal's reader never exited")
	}
}

func waitFor(t *testing.T, term *Terminal, needle string) {
	t.Helper()
	// A round-trip takes milliseconds; the deadline only bites a genuine
	// hang. (It sat at 45s for a while on a wrong diagnosis — CPU
	// starvation — of what was really retireOldReader's stolen chunk.)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if screenContains(term, needle) {
			return
		}
		select {
		case <-term.DataChan():
		case <-time.After(50 * time.Millisecond):
		}
	}
	// Dump what IS on screen: a bare "never appeared" from CI says
	// nothing about whether the shell echoed, ran, or never saw input.
	var dump strings.Builder
	for i, row := range term.SnapshotViewport() {
		var sb strings.Builder
		for c := range row {
			if row[c].Content != "" {
				sb.WriteString(row[c].Content)
			}
		}
		if s := strings.TrimRight(sb.String(), " "); s != "" {
			fmt.Fprintf(&dump, "\n  %2d| %s", i, s)
		}
	}
	t.Fatalf("%q never appeared on screen (alt=%v); screen was:%s", needle, term.IsAltScreen(), dump.String())
}

// TestReleaseAdoptShellSurvives is Phase 1's acceptance test: tear a
// live Terminal down to its plumbing (WITHOUT killing the shell),
// rebuild a fresh Terminal around it, and prove it's the same shell
// with the same screen, scrollback, and a working input path. This
// is the heart of hot upgrade, minus the exec.
func TestReleaseAdoptShellSurvives(t *testing.T) {
	cfg := config.Default()
	cfg.Shell = "/bin/sh" // no zsh prompt noise
	old, err := NewDaemonHosted(&cfg, 80, 24, "")
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}

	// Put a marker on screen, history into scrollback.
	if _, err := old.Write([]byte("echo BEFORE_UPGRADE_MARK\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, old, "BEFORE_UPGRADE_MARK")
	old.Write([]byte("i=0; while [ $i -lt 60 ]; do echo SCROLL_$i; i=$((i+1)); done\r"))
	waitFor(t, old, "SCROLL_59")

	// Quiesce: everything to disk, snapshot, release.
	old.FlushScrollbackToDisk()
	sbLen := old.ScrollbackLen()
	if sbLen == 0 {
		t.Fatal("expected scrollback before handoff")
	}
	screen := old.SnapshotViewport()
	pos := old.CursorPosition()
	appCursor := old.AppCursorMode()

	ptmx, pid, disk, err := old.ReleaseForHandoff()
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if pid <= 0 || ptmx == nil {
		t.Fatalf("bad plumbing: pid=%d ptmx=%v", pid, ptmx)
	}
	if disk == nil {
		t.Fatal("daemon-hosted terminal must hand off a disk store")
	}
	// Shell must still be alive after release (signal 0 probe).
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("shell died during release: %v", err)
	}

	// Rebuild. In the real flow this happens in the new exec image;
	// the object boundary is identical — once the old reader is gone,
	// which the exec does for free and we must do by hand.
	retireOldReader(t, old, ptmx)
	neu, err := Adopt(AdoptSpec{
		Ptmx: ptmx, ChildPID: pid, Cols: 80, Rows: 24,
		Screen: screen, CursorRow: pos.Y, CursorCol: pos.X,
		AppCursor: appCursor, Disk: disk,
	})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	defer neu.Close()

	// Screen carried over.
	if !screenContains(neu, "BEFORE_UPGRADE_MARK") && !screenContains(neu, "SCROLL_59") {
		t.Fatal("restored screen lost its contents")
	}
	// Scrollback carried over, same absolute indices.
	if got := neu.ScrollbackLen(); got != sbLen {
		t.Fatalf("scrollback len: %d, want %d", got, sbLen)
	}
	found := false
	for row := 0; row < neu.ScrollbackLen() && !found; row++ {
		var sb strings.Builder
		for col := 0; col < 80; col++ {
			if c := neu.ScrollbackCellAt(col, row); c != nil && c.Content != "" {
				sb.WriteString(c.Content)
			}
		}
		if strings.Contains(sb.String(), "SCROLL_0") {
			found = true
		}
	}
	if !found {
		t.Fatal("scrollback rows unreadable after adopt")
	}

	// CWD must resolve through the adopted pid (regression: cmd-only
	// lookup read "" for every tab after a hot upgrade).
	if cwd := neu.GetCWD(); cwd == "" {
		t.Error("GetCWD empty on adopted terminal")
	}

	// And it's STILL THE SAME LIVE SHELL: new input round-trips.
	if _, err := neu.Write([]byte("echo ALIVE_AFTER_$((40+2))\r")); err != nil {
		t.Fatalf("post-adopt write: %v", err)
	}
	waitFor(t, neu, "ALIVE_AFTER_42")
}

// TestAdoptDiskScrollbackFromFD covers the across-exec disk path:
// rebuild a store from (fd, offsets, size) — what the handoff file
// carries — and read the same lines back.
func TestAdoptDiskScrollbackFromFD(t *testing.T) {
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	term, err := NewDaemonHosted(&cfg, 40, 10, "")
	if err != nil {
		t.Skipf("no PTY: %v", err)
	}
	defer term.Close()
	term.Write([]byte("i=0; while [ $i -lt 40 ]; do echo DISK_$i; i=$((i+1)); done\r"))
	waitFor(t, term, "DISK_39")
	term.FlushScrollbackToDisk()

	term.mu.Lock()
	store := term.disk
	term.mu.Unlock()
	if store == nil || store.Len() == 0 {
		t.Skip("no disk rows produced")
	}
	f, offsets, size := store.Handoff()

	// Simulate the fd crossing an exec: dup it, rebuild from the dup.
	dupFD, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	adopted := AdoptDiskScrollback(os.NewFile(uintptr(dupFD), "disk-handoff"), offsets, size)
	if adopted.Len() != store.Len() {
		t.Fatalf("len: %d want %d", adopted.Len(), store.Len())
	}
	// A DISK_* line must be readable back from the adopted store. Don't
	// assume a specific index: scrollback line 0 is the wrapped
	// shell-prompt + command echo (`sh-5.3$ i=0; while ...`), NOT
	// DISK_0, and exactly how much scrolled off depends on prompt
	// timing — keying on line 0 made this test flaky.
	found := false
	for row := 0; row < adopted.Len() && !found; row++ {
		var sb strings.Builder
		for _, c := range adopted.LineAt(row) {
			sb.WriteString(c.Content)
		}
		if strings.Contains(sb.String(), "DISK_") {
			found = true
		}
	}
	if !found {
		t.Fatal("no DISK_* line readable from the adopted store")
	}
}

// TestAdoptReplaysModes: the modes a foreground app set before the
// swap must be live in the adopted emulator. Regression for the
// post-`serve --upgrade` bug where every tab silently lost bracketed
// paste (multi-line pastes into Claude Code submitted line by line,
// image-paste paths typed raw), mouse reporting, its hidden cursor and
// the alt screen — nothing re-sends DECSETs after a daemon swap.
func TestAdoptReplaysModes(t *testing.T) {
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	old, err := NewDaemonHosted(&cfg, 80, 24, "")
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}
	// App-style setup: bracketed paste, SGR mouse, hidden cursor, alt
	// screen; the marker proves the sequences ahead of it were parsed.
	old.Write([]byte("printf '\\033[?2004h\\033[?1002h\\033[?1006h\\033[?25l\\033[?1049h'; printf 'MODES_%s\\n' SET\r"))
	waitFor(t, old, "MODES_SET")
	if !old.bracketedPaste.Load() || !old.sgrMouse.Load() || old.cursorVisible.Load() || !old.IsAltScreen() {
		t.Fatalf("precondition: modes not parsed (paste=%v sgr=%v cursor=%v alt=%v)",
			old.bracketedPaste.Load(), old.sgrMouse.Load(), old.cursorVisible.Load(), old.IsAltScreen())
	}
	decSet, decReset, ansiSet, ansiReset := old.ModeSnapshot()
	screen := old.SnapshotViewport()
	pos := old.CursorPosition()

	ptmx, pid, disk, err := old.ReleaseForHandoff()
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	retireOldReader(t, old, ptmx)
	neu, err := Adopt(AdoptSpec{
		Ptmx: ptmx, ChildPID: pid, Cols: 80, Rows: 24,
		Screen: screen, CursorRow: pos.Y, CursorCol: pos.X, Disk: disk,
		DECModesSet: decSet, DECModesReset: decReset,
		ANSIModesSet: ansiSet, ANSIModesReset: ansiReset,
	})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	defer neu.Close()

	if !neu.bracketedPaste.Load() {
		t.Error("bracketed paste lost across adopt")
	}
	if !neu.sgrMouse.Load() {
		t.Error("SGR mouse reporting lost across adopt")
	}
	if neu.cursorVisible.Load() {
		t.Error("hidden cursor came back visible across adopt")
	}
	if !neu.IsAltScreen() {
		t.Error("alt screen lost across adopt")
	}
	if !screenContains(neu, "MODES_SET") {
		t.Error("alt-screen contents were not restored onto the alt buffer")
	}
	// The adopted terminal re-records the replayed modes, so a second
	// handoff carries them forward again.
	ds2, dr2, _, _ := neu.ModeSnapshot()
	if !reflect.DeepEqual(ds2, decSet) || !reflect.DeepEqual(dr2, decReset) {
		t.Errorf("adopted terminal's mode snapshot drifted: set %v/%v reset %v/%v", ds2, decSet, dr2, decReset)
	}

	// Still the same live shell, and later mode changes keep tracking
	// through the adopted emulator's callbacks. Only the alt screen is
	// asserted: /bin/sh is bash on some boxes, and bash re-arms
	// bracketed paste (2004h) with every prompt, so 2004's state after
	// the marker is a race against the next prompt.
	neu.Write([]byte("printf '\\033[?1049l'; printf 'BACK_ON_%s\\n' MAIN\r"))
	waitFor(t, neu, "BACK_ON_MAIN")
	if neu.IsAltScreen() {
		t.Error("mode changes after adopt not tracked")
	}
}
