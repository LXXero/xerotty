package daemonsource_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
)

// TestReattachRestoresTabsAndScrollback is the regression for
// gaps #1 + #2 + #3 — the daemon-mode persistence story:
//
//  1. Client 1 attaches, creates a tab, writes a marker, then
//     Detaches (not Close — daemon-side tab survives).
//  2. Client 2 attaches fresh, expects to see the same tab in
//     the Attached frame.
//  3. Adopting that tab should yield a Source whose scrollback
//     mirror contains the marker the previous client wrote.
//
// Pre-fix: detach was sending TabClose; reattach found no tabs;
// even if it had, the new subscriber seeded lastScrollbackLen to
// the daemon's current length so no history would ship.
func TestReattachRestoresTabsAndScrollback(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	// --- session 1: create a tab and dump output into scrollback ---
	cli1, _ := clientproto.Dial(sockPath)
	cli1.Hello("reattach-1")
	go cli1.Run()
	cli1.Attach("", false)
	<-cli1.Attached()

	hub1 := daemonsource.NewHub(cli1)
	hub1.SetScrollbackCap(20000)
	src1, err := hub1.NewTab(40, 10, "")
	if err != nil {
		t.Fatalf("session 1 new tab: %v", err)
	}
	tabID := uint32(0)
	// Hub.NewTab gave us a Source — its tabID is encapsulated, but
	// we can capture it via the Attached frame from session 2.

	// Write a unique marker that'll scroll off into history.
	marker := "REATTACH_MARKER_42"
	cmd := "echo " + marker + "\rfor i in $(seq 1 80); do echo PAD$i; done\r"
	src1.Write([]byte(cmd))

	// Wait for the marker + pad lines to scroll into scrollback.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if found := scanScrollback(src1, marker); found {
			break
		}
		select {
		case <-src1.DataChan():
		case <-time.After(80 * time.Millisecond):
		}
	}
	if !scanScrollback(src1, marker) {
		t.Fatalf("session 1: marker never reached scrollback")
	}

	// Detach (not Close!) — daemon-side tab MUST survive.
	src1.Detach()
	hub1.Stop()
	cli1.Close()
	<-cli1.Closed()
	time.Sleep(100 * time.Millisecond)

	// --- session 2: reattach, expect the tab to be there ---
	cli2, _ := clientproto.Dial(sockPath)
	defer cli2.Close()
	cli2.Hello("reattach-2")
	go cli2.Run()
	cli2.Attach("", false)
	attached2 := <-cli2.Attached()
	if len(attached2.Tabs) != 1 {
		t.Fatalf("after reattach: expected 1 tab, got %d", len(attached2.Tabs))
	}
	tabID = attached2.Tabs[0].ID

	hub2 := daemonsource.NewHub(cli2)
	defer hub2.Stop()
	hub2.SetScrollbackCap(20000)
	src2 := hub2.Adopt(tabID, int(attached2.Tabs[0].Cols), int(attached2.Tabs[0].Rows))

	// Backfill should ship recent history including the marker.
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if scanScrollback(src2, marker) {
			return
		}
		select {
		case <-src2.DataChan():
		case <-time.After(80 * time.Millisecond):
		}
	}
	t.Fatalf("reattach: marker %q NEVER reached the new session's scrollback. Persistence is broken.", marker)
}

func scanScrollback(s *daemonsource.Source, needle string) bool {
	for row := 0; row < s.ScrollbackLen(); row++ {
		var sb strings.Builder
		for col := 0; col < s.Width(); col++ {
			c := s.ScrollbackCellAt(col, row)
			if c == nil || c.Content == "" {
				continue
			}
			sb.WriteString(c.Content)
		}
		if strings.Contains(sb.String(), needle) {
			return true
		}
	}
	return false
}
