package daemon_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/testutil"
)

func wireRowText(row []protocol.Cell) string {
	var b strings.Builder
	for _, c := range row {
		if c.Content == "" {
			b.WriteByte(' ')
		} else {
			b.WriteString(c.Content)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// TestScrollbackRowWidthByClientVersion: a version-10 client of this
// daemon still gets scrollback rows padded to the grid width, in
// appends and in range replies; a version-11 client gets each row at
// its stored length. The text is the same either way.
func TestScrollbackRowWidthByClientVersion(t *testing.T) {
	if protocol.MinProtocolVersion > 10 {
		t.Skip("version 10 is no longer served")
	}
	const cols = 40
	for _, version := range []uint16{10, protocol.ProtocolVersion} {
		sockPath := filepath.Join(testutil.SockDir(t), "xerottyd.sock")
		cfg := config.Default()
		d := daemon.New(&cfg, sockPath)
		doneRun := make(chan error, 1)
		go func() { doneRun <- d.Run() }()
		time.Sleep(50 * time.Millisecond)

		func() {
			defer func() { _ = d.Stop(); <-doneRun }()
			c, err := clientproto.Dial(sockPath)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			if _, err := c.HelloVersion("row-width", version); err != nil {
				t.Fatalf("v%d hello: %v", version, err)
			}
			go c.Run()
			if err := c.Attach("", true); err != nil {
				t.Fatalf("attach: %v", err)
			}
			tabID := (<-c.Attached()).Tabs[0].ID
			go func() {
				for {
					select {
					case <-c.CellFull():
					case <-c.CellDiff():
					case <-c.Cursor():
					case <-c.Title():
					case <-c.TabState():
					case <-c.Closed():
						return
					}
				}
			}()
			if err := c.SendResize(tabID, cols, 10); err != nil {
				t.Fatalf("resize: %v", err)
			}
			time.Sleep(300 * time.Millisecond)
			if err := c.SendInput(tabID, []byte("clear; printf 'PADROW\\n\\n'; seq 1 40\r")); err != nil {
				t.Fatalf("input: %v", err)
			}

			check := func(where string, rows [][]protocol.Cell) (found bool) {
				for _, row := range rows {
					text := wireRowText(row)
					if version >= protocol.ShortRowsVersion {
						// A zero cell at the end of a row is padding.
						if n := len(row); n > 0 && row[n-1].Content == "" {
							t.Errorf("v%d %s: row %q sent with %d cells (padded)", version, where, text, len(row))
						}
					} else if len(row) != cols {
						t.Errorf("v%d %s: row %q sent with %d cells, want %d", version, where, text, len(row), cols)
					}
					if text == "PADROW" {
						found = true
						want := cols
						if version >= protocol.ShortRowsVersion {
							want = len("PADROW")
						}
						if len(row) != want {
							t.Errorf("v%d %s: PADROW row has %d cells, want %d", version, where, len(row), want)
						}
					}
				}
				return found
			}

			deadline := time.After(10 * time.Second)
			found, total := false, 0
			for !found || total < 30 {
				select {
				case f := <-c.ScrollbackAppend():
					total = int(f.BaseIdx) + len(f.Rows)
					if check("append", f.Rows) {
						found = true
					}
				case <-deadline:
					t.Fatalf("v%d: PADROW never arrived in a ScrollbackAppend (%d rows seen)", version, total)
				}
			}

			if err := c.SendScrollbackRequest(tabID, 0, total); err != nil {
				t.Fatalf("range request: %v", err)
			}
			select {
			case f := <-c.ScrollbackRange():
				if !check("range", f.Rows) {
					t.Errorf("v%d: PADROW missing from the range reply (%d rows)", version, len(f.Rows))
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("v%d: no ScrollbackRange reply", version)
			}
		}()
	}
}
