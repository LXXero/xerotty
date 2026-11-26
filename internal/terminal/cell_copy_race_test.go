package terminal

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
)

// CellAt, GridRow and ScrollbackCellAt hand out copies taken under
// publishMu, so reading them races neither output (ingest) nor a
// resize, both of which rewrite cells. Run with -race. Output here
// includes BEL, whose callback takes mu while the emulator lock is
// held: resize used to take those two locks the other way round, so
// this also checks the two can run side by side without deadlock.
func TestCellReadsRaceNeitherIngestNorResize(t *testing.T) {
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	term, err := NewDaemonHosted(&cfg, 40, 10, "")
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}
	defer term.Close()
	term.OnBell = func() {}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() { // reader: dereference every copy it gets
		defer wg.Done()
		n := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			if c := term.CellAt(n%40, n%10); c != nil {
				n += len(c.Content)
			}
			for _, c := range term.GridRow(n%10, 40) {
				n += len(c.Content)
			}
			if sb := term.ScrollbackLen(); sb > 0 {
				if c := term.ScrollbackCellAt(n%40, n%sb); c != nil {
					n += len(c.Content)
				}
			}
			n++
		}
	}()
	go func() { // resizer
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			term.Resize(30+i%20, 8+i%6)
		}
	}()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			term.ingest([]byte(fmt.Sprintf("row %d \x07with a bell\r\n", i)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("ingest stalled: resize and ingest deadlocked")
	}
	close(stop)
	wg.Wait()
}
