package daemonsource

import (
	"testing"

	"github.com/LXXero/xerotty/internal/protocol"
)

// TestScrollbackAppendWakesAndBumps: a scrollback append must (a)
// signal DataChan so the GUI repaints a scrolled-back view without
// waiting for an unrelated event, and (b) bump RenderGeneration so
// the renderer's cell-layer cache rebuilds instead of replaying the
// pre-append frame. Regression for a codex-review finding: the
// append path mutated visible state without signaling.
func TestScrollbackAppendWakesAndBumps(t *testing.T) {
	s := &Source{
		dataCh:        make(chan struct{}, 1),
		scrollbackCap: 100,
	}
	genBefore := s.RenderGeneration()

	s.applyScrollbackAppend(&protocol.ScrollbackAppend{
		Rows: [][]protocol.Cell{{{Content: "x", Width: 1}}},
	})

	select {
	case <-s.DataChan():
		// woken — good
	default:
		t.Fatal("scrollback append did not signal DataChan")
	}
	if got := s.RenderGeneration(); got != genBefore+1 {
		t.Fatalf("RenderGeneration = %d, want %d", got, genBefore+1)
	}
	if s.ScrollbackLen() != 1 {
		t.Fatalf("ScrollbackLen = %d, want 1", s.ScrollbackLen())
	}
}
