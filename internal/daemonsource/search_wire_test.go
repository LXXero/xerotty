package daemonsource

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
)

// TestSearchWire exercises daemon-side search end to end: the windowed
// client ships a query, the daemon scans its full (disk-backed)
// scrollback, and match coordinates come back — including matches in
// history the client never mirrored.
func TestSearchWire(t *testing.T) {
	defer func(c, m int) { scrollbackWindowCap, scrollbackWindowMargin = c, m }(scrollbackWindowCap, scrollbackWindowMargin)
	scrollbackWindowCap, scrollbackWindowMargin = 20, 5 // history exceeds the window

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
	if _, err := cli.Hello("search-test"); err != nil {
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

	if _, err := src.Write([]byte("for i in $(seq 1 120); do echo SEARCHME$i; done\r")); err != nil {
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
	if src.ScrollbackLen() < 100 {
		t.Fatalf("scrollback only %d rows; need >= 100", src.ScrollbackLen())
	}

	search := func(q string) []searchHit {
		reqID := src.RequestScrollbackSearch(q, false, false, false)
		if reqID == 0 {
			t.Fatalf("RequestScrollbackSearch(%q) returned 0 (no connection?)", q)
		}
		dl := time.Now().Add(5 * time.Second)
		for time.Now().Before(dl) {
			if rid, matches, _, ok := src.TakeSearchResults(); ok && rid == reqID {
				out := make([]searchHit, len(matches))
				for i, m := range matches {
					out[i] = searchHit{int(m.Line), int(m.Col)}
				}
				return out
			}
			select {
			case <-src.DataChan():
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatalf("no search results for %q within 5s", q)
		return nil
	}

	// A unique marker deep in cold history (well beyond the 20-row
	// window) must still be found — proving daemon-side search reaches
	// what the client never mirrored.
	if hits := search("SEARCHME42"); len(hits) != 1 {
		t.Fatalf("search SEARCHME42 = %d hits, want exactly 1", len(hits))
	}
	// A common substring hits every generated line.
	if hits := search("SEARCHME"); len(hits) < 100 {
		t.Fatalf("search SEARCHME = %d hits, want >= 100 (full history)", len(hits))
	}
	// No match → empty.
	if hits := search("NOPE_NOT_PRESENT_xyz"); len(hits) != 0 {
		t.Fatalf("search for absent term = %d hits, want 0", len(hits))
	}
}

type searchHit struct{ line, col int }
