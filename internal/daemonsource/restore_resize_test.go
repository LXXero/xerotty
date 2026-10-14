package daemonsource_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/terminal"
	"github.com/LXXero/xerotty/internal/testutil"
)

// drawFullScreen fills every row of an 80x24 tab once and then sleeps,
// so nothing repaints a row the daemon or the client loses. The first
// sleep lets the Hub register the new tab's Source before output
// starts: frames that arrive before that are held in a queue capped at
// pendingCap that drops its oldest, which can lose the top rows.
const drawFullScreen = `sleep 1; printf '\033[2J\033[H'; i=1; while [ $i -le 23 ]; do printf 'ROW%02d\r\n' $i; i=$((i+1)); done; printf ROW24; sleep 60`

// TestRestoredWindowTabsGetFullScreen reproduces the half-drawn tabs in
// a GUI window restored after a restart. The window's tabs are adopted
// at the daemon's size, resized to a provisional grid computed before
// the window's layout is final, then to the real one. Every tab must
// end up with its whole screen: rows the provisional shrink cut from
// the client's mirror come back, and the daemon's own grid keeps them.
func TestRestoredWindowTabsGetFullScreen(t *testing.T) {
	cases := []struct {
		name                 string
		finalCols, finalRows int
	}{
		{"back to the old size", 80, 24},
		{"to a bigger size", 100, 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sockPath := filepath.Join(testutil.SockDir(t), "xerottyd.sock")
			cfg := config.Default()
			d := daemon.New(&cfg, sockPath)
			doneRun := make(chan error, 1)
			go func() { doneRun <- d.Run() }()
			defer func() { _ = d.Stop(); <-doneRun }()
			time.Sleep(50 * time.Millisecond)

			attach := func(name string) (*clientproto.Client, *daemonsource.Hub) {
				t.Helper()
				cli, err := clientproto.Dial(sockPath)
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				if _, err := cli.Hello(name); err != nil {
					t.Fatalf("hello: %v", err)
				}
				go cli.Run()
				if err := cli.Attach("", false); err != nil {
					t.Fatalf("attach: %v", err)
				}
				<-cli.Attached()
				return cli, daemonsource.NewHub(cli)
			}

			// The session before the restart: two tabs, fully drawn.
			cli1, hub1 := attach("gui-before")
			var ids []uint32
			for i := 0; i < 2; i++ {
				src, err := hub1.NewTabIn(0, 80, 24, "", &terminal.LaunchCmd{Argv: []string{drawFullScreen}, Shell: true})
				if err != nil {
					t.Fatalf("new tab: %v", err)
				}
				waitRows(t, src, 80, 24, "before restart")
				ids = append(ids, src.TabID())
				src.Detach()
			}
			hub1.Stop()
			cli1.Close()

			// The restarted GUI's second window adopts them at the
			// daemon's size, then sizes them provisionally, then for
			// real, back to back as its first frames do.
			cli2, hub2 := attach("gui-after")
			defer cli2.Close()
			defer hub2.Stop()
			var srcs []*daemonsource.Source
			for _, id := range ids {
				src := hub2.Adopt(id, 80, 24)
				// The attach-time screen lands before the window's
				// first layout pass, as it does in the GUI.
				waitRows(t, src, 80, 24, "adopted")
				srcs = append(srcs, src)
			}
			for _, src := range srcs {
				src.Resize(80, 12)
			}
			for _, src := range srcs {
				src.Resize(tc.finalCols, tc.finalRows)
			}
			for _, src := range srcs {
				waitRows(t, src, tc.finalCols, tc.finalRows, "after restore")
			}
		})
	}
}

// waitRows waits until src's mirror is cols×rows and shows ROW01 to
// ROW24, each on its own row.
func waitRows(t *testing.T, src *daemonsource.Source, cols, rows int, when string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		missing := ""
		grid := src.SnapshotViewport()
		if len(grid) != rows || len(grid[0]) != cols {
			missing = fmt.Sprintf("size %dx%d", len(grid[0]), len(grid))
		} else {
			for r := 0; r < 24; r++ {
				var sb strings.Builder
				for _, cell := range grid[r] {
					sb.WriteString(cell.Content)
				}
				if want := fmt.Sprintf("ROW%02d", r+1); !strings.HasPrefix(sb.String(), want) {
					missing = want
					break
				}
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tab %d %s: missing %s; mirror:\n%s", src.TabID(), when, missing, emulatorDump(src))
		}
		select {
		case <-src.DataChan():
		case <-time.After(50 * time.Millisecond):
		}
	}
}
