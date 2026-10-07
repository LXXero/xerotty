package terminal

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/LXXero/xerotty/internal/config"
)

// TestScrollbackLenNeverOvercounts: in disk-backed mode a lock-free
// ScrollbackLen reader (the daemon's sendNewScrollback) racing the
// write-through mirror must never see a total that later shrinks. The
// mirror used to grow the disk store before bumping memMirrored, so a
// read between the two counted the freshly mirrored lines twice; the
// daemon then shipped a short ScrollbackAppend with an inflated Total
// and the windowed client's history went permanently out of step.
func TestScrollbackLenNeverOvercounts(t *testing.T) {
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	term, err := NewDaemonHosted(&cfg, 40, 5, "")
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}
	defer term.Close()

	var stop atomic.Bool
	type drop struct{ from, to int }
	drops := make(chan drop, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		prev := 0
		for !stop.Load() {
			n := term.ScrollbackLen()
			if n < prev {
				select {
				case drops <- drop{prev, n}:
				default:
				}
				return
			}
			prev = n
		}
	}()

	// Feed output straight into the emulator: each chunk scrolls a few
	// lines off the 5-row grid and triggers one disk mirror.
	for i := 0; i < 4000; i++ {
		term.ingest([]byte(fmt.Sprintf("line-%d-a\r\nline-%d-b\r\nline-%d-c\r\n", i, i, i)))
	}
	stop.Store(true)
	<-done

	select {
	case d := <-drops:
		t.Fatalf("ScrollbackLen went from %d down to %d with no clear: a read overcounted mid-mirror", d.from, d.to)
	default:
	}
	total := term.ScrollbackLen()
	if snap := term.SnapshotScrollbackRange(0, total); len(snap) != total {
		t.Fatalf("snapshot returned %d rows, ScrollbackLen = %d", len(snap), total)
	}
}
