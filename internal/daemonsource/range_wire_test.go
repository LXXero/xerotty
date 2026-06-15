package daemonsource

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
)

// TestScrollbackRangeWire exercises on-demand window fetch end-to-end
// over a real daemon connection: with the window cap shrunk so the
// client can't mirror all the history, scrolling into cold rows must
// trigger a ScrollbackRequest, the daemon must serve it from its
// disk-backed scrollback, and the rows must become readable.
func TestScrollbackRangeWire(t *testing.T) {
	// Shrink the window so ~120 rows of history can't all be cached.
	defer func(c, m int) { scrollbackWindowCap, scrollbackWindowMargin = c, m }(scrollbackWindowCap, scrollbackWindowMargin)
	scrollbackWindowCap, scrollbackWindowMargin = 20, 5

	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	cfg.Scrollback.Mode = "unlimited"
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	cli, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	if _, err := cli.Hello("range-test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	<-cli.Attached()

	hub := NewHub(cli)
	hub.SetScrollbackWindowed()
	defer hub.Stop()

	src, err := hub.NewTab(40, 10, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}

	if _, err := src.Write([]byte("for i in $(seq 1 120); do echo RANGE$i; done\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if src.ScrollbackLen() >= 100 {
			break
		}
		select {
		case <-src.DataChan():
		case <-time.After(150 * time.Millisecond):
		}
	}
	total := src.ScrollbackLen()
	if total < 100 {
		t.Fatalf("scrollback only %d rows; need >= 100", total)
	}

	// Cold row 15 is far below the live-anchored 20-row window — it
	// should NOT be cached yet.
	if c := src.ScrollbackCellAt(0, 15); c != nil {
		t.Fatal("cold row unexpectedly cached; window cap not taking effect")
	}

	// Ask the viewport to show rows around 15 → triggers a wire fetch.
	readable := false
	for i := 0; i < 40 && !readable; i++ {
		src.EnsureScrollbackWindow(12, 18)
		select {
		case <-src.DataChan():
		case <-time.After(150 * time.Millisecond):
		}
		if c := src.ScrollbackCellAt(0, 15); c != nil {
			readable = true
		}
	}
	if !readable {
		t.Fatal("cold row 15 never became readable after fetch")
	}

	// Verify the fetched content is genuine scrollback, not blanks.
	var sb strings.Builder
	for col := 0; col < src.Width(); col++ {
		if c := src.ScrollbackCellAt(col, 15); c != nil {
			sb.WriteString(c.Content)
		}
	}
	if !strings.Contains(sb.String(), "RANGE") {
		t.Fatalf("fetched row 15 = %q, expected a RANGE marker", sb.String())
	}
	// ScrollbackLen still reports the true total, not the window.
	if src.ScrollbackLen() < 100 {
		t.Fatalf("ScrollbackLen dropped to %d after fetch", src.ScrollbackLen())
	}
}
