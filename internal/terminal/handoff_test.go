package terminal

import (
	"os"
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

func waitFor(t *testing.T, term *Terminal, needle string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if screenContains(term, needle) {
			return
		}
		select {
		case <-term.DataChan():
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("%q never appeared on screen", needle)
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
	// the object boundary is identical.
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
	line := adopted.LineAt(0)
	var sb strings.Builder
	for _, c := range line {
		sb.WriteString(c.Content)
	}
	if !strings.Contains(sb.String(), "DISK_") {
		t.Fatalf("adopted store line 0 unreadable: %q", sb.String())
	}
}
