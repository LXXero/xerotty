package terminal

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	vt "github.com/charmbracelet/x/vt"
)

// TestSnapshotRangeReadsOncePerRow guards the daemon-hang fix:
// SnapshotScrollbackRange must read each disk line ONCE per row, not
// once per column. The per-cell version did rows×cols disk decodes
// (an 80× amplifier) which thrashed the daemon under on-demand range
// fetches.
func TestSnapshotRangeReadsOncePerRow(t *testing.T) {
	const cols = 80
	const rows = 300
	disk, err := NewDiskScrollback()
	if err != nil {
		t.Fatalf("disk: %v", err)
	}
	for r := 0; r < rows; r++ {
		line := make(uv.Line, cols)
		for c := range line {
			line[c] = uv.Cell{Content: "x", Width: 1}
		}
		if err := disk.Append(line); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	term := &Terminal{Emu: vt.NewSafeEmulator(cols, 24), disk: disk}

	before := disk.ReadCount()
	out := term.SnapshotScrollbackRange(0, rows)
	reads := disk.ReadCount() - before

	if len(out) != rows {
		t.Fatalf("snapshot returned %d rows, want %d", len(out), rows)
	}
	// One read per row (allow a tiny constant of slack). The bug did
	// rows*cols = 24000 reads.
	if reads > int64(rows)+8 {
		t.Fatalf("snapshot did %d disk reads for %d rows — per-cell amplification regressed (cols=%d)", reads, rows, cols)
	}
	// Sanity: content actually came through.
	if out[0][0].Content != "x" {
		t.Fatalf("row content wrong: %q", out[0][0].Content)
	}
}
