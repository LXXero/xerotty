package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/scrollback"
	"github.com/LXXero/xerotty/internal/testutil"
)

// TestReadersOnShortScrollbackRows runs selection, copy, search and
// link hit-testing over scrollback rows held at their stored length
// (no trailing blank cells), as a version-11 daemon sends them. A
// column past a row's end must read as blank and never panic.
func TestReadersOnShortScrollbackRows(t *testing.T) {
	const cols, rows = 40, 10
	const link = "https://example.com/short"

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
	if _, err := cli.Hello("short-rows"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	<-cli.Attached()
	hub := daemonsource.NewHub(cli)
	hub.SetScrollbackWindowed()
	defer hub.Stop()
	src, err := hub.NewTab(cols, rows, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}

	cmd := `clear; printf 'ALPHA beta\n\nsee ` + link + ` ok\n'; seq 1 30` + "\r"
	if _, err := src.Write([]byte(cmd)); err != nil {
		t.Fatalf("write: %v", err)
	}
	idx, sb := -1, 0
	deadline := time.Now().Add(8 * time.Second)
	for idx < 0 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the rows in scrollback (%d rows)", sb)
		}
		select {
		case <-src.DataChan():
		case <-time.After(50 * time.Millisecond):
		}
		sb = src.ScrollbackLen()
		src.EnsureScrollbackWindow(0, sb)
		for i := 0; i+2 < sb; i++ {
			if strings.TrimRight(scrollbackRowText(src, i, cols), " ") == "ALPHA beta" &&
				strings.Contains(scrollbackRowText(src, i+2, cols), link) {
				idx = i
				break
			}
		}
	}

	// The mirror holds the rows short.
	if c := src.ScrollbackCellAt(len("ALPHA beta")-1, idx); c == nil || c.Content != "a" {
		t.Fatalf("last cell of the row = %+v, want a", c)
	}
	if c := src.ScrollbackCellAt(len("ALPHA beta"), idx); c != nil {
		t.Fatalf("cell past the row end = %+v, want nil (row not stored short)", c)
	}

	// Selection and copy across a short row, an empty row and the link
	// row, with both ends past the text.
	sel := selection{active: true, startRow: idx, startCol: 0, endRow: idx + 2, endCol: cols - 1}
	want := "ALPHA beta\n\nsee " + link + " ok"
	if got := sel.extractText(src, true); got != want {
		t.Errorf("copy (trimmed) = %q, want %q", got, want)
	}
	lines := strings.Split(sel.extractText(src, false), "\n")
	if len(lines) != 3 || strings.TrimRight(lines[0], " ") != "ALPHA beta" || strings.TrimRight(lines[1], " ") != "" {
		t.Errorf("copy (untrimmed) = %q", lines)
	}
	var s2 selection
	s2.selectWord(src, 7, idx) // inside "beta"
	if got := s2.extractText(src, true); got != "beta" {
		t.Errorf("word selection = %q, want beta", got)
	}
	s2.selectWord(src, cols-2, idx) // past the row end
	s2.selectSpace(src, cols-2, idx)
	s2.selectLine(src, idx)
	if got := s2.extractText(src, true); got != "ALPHA beta" {
		t.Errorf("line selection = %q, want ALPHA beta", got)
	}
	s2.selectLine(src, idx+1) // the empty row
	if got := s2.extractText(src, true); got != "" {
		t.Errorf("empty-row line selection = %q, want empty", got)
	}
	s2.startCharDrag(idx, 3)
	s2.extendDrag(idx+1, cols-1, src)

	// Search.
	st := scrollback.New()
	st.Query = "beta"
	st.Search(src, rows)
	found := false
	for _, m := range st.Matches {
		if m.Col == 6 && m.Len == 4 {
			found = true
		}
	}
	if !found {
		t.Errorf("search for beta on the short row: matches = %+v", st.Matches)
	}

	// Link hit-testing: on the URL, and past the end of the row.
	off := sb - (idx + 2)
	if hit := detectLinkAt(src, 6, 0, off); hit == nil || hit.URL != link {
		t.Errorf("link hit on the short row = %+v, want %s", hit, link)
	}
	if hit := detectLinkAt(src, cols-1, 0, off); hit != nil {
		t.Errorf("link hit past the row end = %+v, want none", hit)
	}
	if hit := detectLinkAt(src, 5, 0, sb-(idx+1)); hit != nil { // the empty row
		t.Errorf("link hit on the empty row = %+v, want none", hit)
	}
}
