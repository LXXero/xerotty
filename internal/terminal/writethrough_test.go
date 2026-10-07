package terminal

import (
	"strings"
	"testing"

	"github.com/LXXero/xerotty/internal/config"
)

// TestScrollbackWriteThrough: in disk-backed mode every line that
// scrolls off the grid is on disk right away (so a daemon death loses
// none of it), the absolute row count is the disk count, and reads
// resolve old rows from disk and recent rows from the memory cache.
func TestScrollbackWriteThrough(t *testing.T) {
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	term, err := NewDaemonHosted(&cfg, 40, 5, "")
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}
	defer term.Close()
	// The marker is assembled at run time so the echoed command line
	// itself never contains "ROW_".
	term.Write([]byte("R=ROW; i=0; while [ $i -lt 30 ]; do echo ${R}_$i; i=$((i+1)); done; printf 'END_%s\\n' MARK\r"))
	waitFor(t, term, "END_MARK")

	term.mu.Lock()
	disk, mirrored := term.disk, term.memMirrored
	term.mu.Unlock()
	if disk == nil {
		t.Fatal("daemon-hosted terminal has no disk store")
	}
	memLen := term.Emu.ScrollbackLen()
	if mirrored != memLen {
		t.Fatalf("ring not fully mirrored: mirrored=%d memLen=%d", mirrored, memLen)
	}
	if disk.Len() != term.ScrollbackLen() {
		t.Fatalf("disk holds %d lines but ScrollbackLen = %d", disk.Len(), term.ScrollbackLen())
	}
	if disk.Len() < 30 {
		t.Fatalf("expected >= 30 lines on disk, got %d", disk.Len())
	}
	// Every row reads back whichever side serves it; the newest
	// ROW_ lines may still be on the 5-row screen.
	var found int
	for r := 0; r < term.ScrollbackLen(); r++ {
		if strings.Contains(term.ScrollbackLineText(r, 40), "ROW_") {
			found++
		}
	}
	for _, row := range term.SnapshotViewport() {
		var sb strings.Builder
		for i := range row {
			sb.WriteString(row[i].Content)
		}
		if strings.Contains(sb.String(), "ROW_") {
			found++
		}
	}
	if found != 30 {
		t.Fatalf("found %d ROW_ lines across disk+memory+screen, want 30", found)
	}
	// And the bulk path agrees with the per-row path.
	snap := term.SnapshotScrollbackRange(0, term.ScrollbackLen())
	if len(snap) != term.ScrollbackLen() {
		t.Fatalf("snapshot rows %d != len %d", len(snap), term.ScrollbackLen())
	}
}
