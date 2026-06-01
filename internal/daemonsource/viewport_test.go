package daemonsource_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
)

// TestViewportConsistencyUnderBurst is the regression for the
// "1, 6573, 3, 4, 5..." live-cell scramble. Daemon's snapshot
// loop read cells one at a time via SafeEmulator's per-call lock;
// PTY writes between reads scrolled the grid, so the published
// CellDiff frames contained cells from different timeline points
// mixed together. The fix added a publishMu coordination lock so
// SnapshotViewport reads are atomic w.r.t. emulator writes.
//
// Test: run `seq 1 1000` into a small viewport, wait for output to
// settle, then read the visible cells from the client's mirror and
// assert any numeric rows are in strictly increasing order.
// Mid-stream snapshots ARE allowed to be partial — only the
// terminal state has to be consistent.
func TestViewportConsistencyUnderBurst(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	cli, _ := clientproto.Dial(sockPath)
	defer cli.Close()
	cli.Hello("viewport-consistency")
	go cli.Run()
	cli.Attach("", false)
	<-cli.Attached()

	hub := daemonsource.NewHub(cli)
	defer hub.Stop()
	hub.SetScrollbackCap(2000)

	src, _ := hub.NewTab(40, 10, "")

	const total = 1000
	if _, err := src.Write([]byte("seq " + strconv.Itoa(total) + "\r")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Drain until the scrollback stops growing.
	deadline := time.Now().Add(30 * time.Second)
	lastSB := -1
	stable := time.Now()
	for time.Now().Before(deadline) {
		l := src.ScrollbackLen()
		if l != lastSB {
			lastSB = l
			stable = time.Now()
		}
		if l > total-50 && time.Since(stable) > 800*time.Millisecond {
			break
		}
		select {
		case <-src.DataChan():
		case <-time.After(80 * time.Millisecond):
		}
	}

	// Now check the visible viewport via SnapshotViewport — same
	// atomic-snapshot guarantee the wire-publish path uses. Going
	// through src.Emulator().CellAt returns live pointers that
	// race the router goroutine (race detector confirms).
	grid := src.SnapshotViewport()
	var lastN int = -1
	regressions := 0
	checked := 0
	for r, row := range grid {
		var sb strings.Builder
		for _, cell := range row {
			if cell.Content == "" {
				continue
			}
			sb.WriteString(cell.Content)
		}
		s := strings.TrimSpace(sb.String())
		n, err := strconv.Atoi(s)
		if err != nil {
			continue
		}
		checked++
		if lastN < 0 {
			lastN = n
			continue
		}
		if n != lastN+1 {
			regressions++
			if regressions <= 3 {
				t.Logf("viewport regression at row %d: %d → %d (want %d)", r, lastN, n, lastN+1)
			}
		}
		lastN = n
	}
	if checked < 2 {
		t.Fatalf("not enough numeric rows in viewport (%d)", checked)
	}
	if regressions > 0 {
		t.Errorf("%d live-viewport regressions (rows scrambled mid-publish)", regressions)
	}
}
