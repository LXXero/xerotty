package daemonsource

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/testutil"
)

// lRow is a scrollback row whose text identifies its absolute index.
func lRow(n int) []protocol.Cell {
	s := fmt.Sprintf("row-%d", n)
	row := make([]protocol.Cell, len(s))
	for i, r := range s {
		row[i] = protocol.Cell{Content: string(r), Width: 1}
	}
	return row
}

func rowText(r []uv.Cell) string {
	var sb strings.Builder
	for _, c := range r {
		sb.WriteString(c.Content)
	}
	return strings.TrimRight(sb.String(), " ")
}

func appendLabelled(s *Source, from, to int) {
	rows := make([][]protocol.Cell, 0, to-from)
	for i := from; i < to; i++ {
		rows = append(rows, lRow(i))
	}
	s.applyScrollbackAppend(&protocol.ScrollbackAppend{BaseIdx: uint32(from), Rows: rows, Total: uint32(to)})
	select {
	case <-s.dataCh:
	default:
	}
}

// assertRows checks grid holds consecutive rows labelled first..
func assertRows(t *testing.T, what string, grid [][]uv.Cell, first, n int) {
	t.Helper()
	if len(grid) != n {
		t.Fatalf("%s: got %d rows, want %d", what, len(grid), n)
	}
	for i, r := range grid {
		if got, want := rowText(r), fmt.Sprintf("row-%d", first+i); got != want {
			t.Fatalf("%s: row %d = %q, want %q", what, i, got, want)
		}
	}
}

// Capped mode: ScrollbackLen is the mirror length, so indices already
// coincide with the mirror — guard that it stays that way.
func TestSnapshotScrollbackRangeCapped(t *testing.T) {
	s := &Source{dataCh: make(chan struct{}, 1), scrollbackCap: 10}
	appendLabelled(s, 0, 25) // FIFO keeps the last 10 (rows 15..24)
	if n := s.ScrollbackLen(); n != 10 {
		t.Fatalf("ScrollbackLen = %d, want 10", n)
	}
	assertRows(t, "whole", s.SnapshotScrollbackRange(0, 10), 15, 10)
	assertRows(t, "tail", s.SnapshotScrollbackRange(5, 10), 20, 5)
	if g := s.SnapshotScrollbackRange(10, 20); g != nil {
		t.Fatalf("past-end read returned %d rows", len(g))
	}
}

// Windowed mode: indices are ABSOLUTE but the mirror only holds
// [winStart, winStart+len). The bug read mirror index `from`, so a
// from:0 read returned mid-session text and the default last-N read
// (from ≥ window len) returned nothing. With no hub the out-of-window
// part can't be fetched; the result must then be a correctly-labelled
// prefix — never shifted rows.
func TestSnapshotScrollbackRangeWindowed(t *testing.T) {
	defer func(c int) { scrollbackWindowCap = c }(scrollbackWindowCap)
	scrollbackWindowCap = 100

	s := newWindowedSource()
	appendLabelled(s, 0, 350) // window = 250..349
	total := s.ScrollbackLen()
	if total != 350 || s.winStart != 250 {
		t.Fatalf("total=%d winStart=%d, want 350/250", total, s.winStart)
	}

	// The default MCP read: last 50 rows — all held.
	assertRows(t, "last-50", s.SnapshotScrollbackRange(total-50, total), 300, 50)
	// Clamped past the end.
	assertRows(t, "clamp", s.SnapshotScrollbackRange(340, 9999), 340, 10)
	// Entirely below the window, no hub: nothing, not wrong rows.
	if g := s.SnapshotScrollbackRange(0, 10); len(g) != 0 {
		t.Fatalf("cold read returned %d rows (first %q)", len(g), rowText(g[0]))
	}
	// Straddling the window start: the cold head can't be fetched, so
	// returning the held tail would shift it — expect an empty prefix.
	if g := s.SnapshotScrollbackRange(245, 255); len(g) != 0 {
		t.Fatalf("straddle read returned %d rows (first %q)", len(g), rowText(g[0]))
	}
}

// End to end over a real daemon: history far outside the client's
// window is fetched privately with correct absolute labels, and the
// fetch must NOT move the display window.
func TestSnapshotScrollbackRangeWire(t *testing.T) {
	defer func(c, p, f int) { scrollbackWindowCap, scrollbackPrefetch, scrollbackFetchSpan = c, p, f }(scrollbackWindowCap, scrollbackPrefetch, scrollbackFetchSpan)
	scrollbackWindowCap, scrollbackPrefetch, scrollbackFetchSpan = 20, 5, 30

	sockPath := filepath.Join(testutil.SockDir(t), "xerottyd.sock")
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
	if _, err := cli.Hello("range-mcp-test"); err != nil {
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
	if _, err := src.Write([]byte("for i in $(seq 1 120); do echo MARK$i; done\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && src.ScrollbackLen() < 100 {
		select {
		case <-src.DataChan():
		case <-time.After(150 * time.Millisecond):
		}
	}
	total := src.ScrollbackLen()
	if total < 100 {
		t.Fatalf("scrollback only %d rows; need >= 100", total)
	}
	src.mu.Lock()
	winBefore, lenBefore := src.winStart, len(src.scrollback)
	src.mu.Unlock()
	if winBefore == 0 {
		t.Fatal("window starts at 0; cap not taking effect")
	}

	// Whole history: spans cold rows (fetched, > one fetch span) plus
	// the held window.
	grid := src.SnapshotScrollbackRange(0, total)
	if len(grid) != total {
		t.Fatalf("full read returned %d rows, want %d", len(grid), total)
	}
	// The MARKn rows must run 1,2,3,… with no gap or reorder (the tail
	// MARKs are still on the live screen, not in scrollback, so the run
	// ends short of 120 — but it must cover well past the window).
	var marks []int
	for _, r := range grid {
		var n int
		if _, err := fmt.Sscanf(rowText(r), "MARK%d", &n); err == nil {
			marks = append(marks, n)
		}
	}
	for i, n := range marks {
		if n != i+1 {
			t.Fatalf("MARK rows out of sequence at %d: %v", i, marks)
		}
	}
	if len(marks) < 90 {
		t.Fatalf("only %d MARK rows in %d-row history: %v", len(marks), total, marks)
	}
	// A cold read must match the same absolute rows.
	cold := src.SnapshotScrollbackRange(5, 15)
	for i, r := range cold {
		if rowText(r) != rowText(grid[5+i]) {
			t.Fatalf("cold row %d = %q, full read had %q", 5+i, rowText(r), rowText(grid[5+i]))
		}
	}

	src.mu.Lock()
	winAfter, lenAfter := src.winStart, len(src.scrollback)
	src.mu.Unlock()
	if winAfter < winBefore || lenAfter > scrollbackWindowCap {
		t.Fatalf("MCP read disturbed the display window: %d+%d -> %d+%d", winBefore, lenBefore, winAfter, lenAfter)
	}
}
