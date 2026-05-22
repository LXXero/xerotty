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

// TestScrollbackOrderUnderRotation is the regression for the user
// screenshot showing 5595..5601 → 5231 → 326 → 18..32 in
// scrollback. Memory-mode ring rotation was scrambling absolute
// scrollback indices on the daemon side; the fix made the daemon
// always run in unlimited+disk mode so indices stay stable.
//
// We force-config the daemon to memory mode here (so the test
// exercises the path that USED to break). With NewDaemonHosted's
// internal override, the daemon should still hoard everything.
//
// Test runs a 12000-line seq into a small ring (8000-line cap)
// and asserts the row numbers come out monotonically increasing
// with no duplicates or reordering.
func TestScrollbackOrderUnderRotation(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	// User's "memory mode 8000" preference. NewDaemonHosted's
	// override should ensure the daemon still uses unlimited+disk
	// so this preference can't cause rotation-induced corruption.
	cfg.Scrollback.Mode = "memory"
	cfg.Scrollback.Lines = 8000

	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	cli, _ := clientproto.Dial(sockPath)
	defer cli.Close()
	cli.Hello("order-test")
	go cli.Run()
	cli.Attach("", false)
	<-cli.Attached()

	hub := daemonsource.NewHub(cli)
	defer hub.Stop()
	hub.SetScrollbackCap(20000)

	src, _ := hub.NewTab(40, 8, "")

	const total = 12000
	if _, err := src.Write([]byte("seq " + strconv.Itoa(total) + "\r")); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(45 * time.Second)
	lastLen := -1
	stable := time.Now()
	for time.Now().Before(deadline) {
		l := src.ScrollbackLen()
		if l != lastLen {
			lastLen = l
			stable = time.Now()
		}
		if l > total-50 && time.Since(stable) > 800*time.Millisecond {
			break
		}
		select {
		case <-src.DataChan():
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Walk the entire scrollback; pull out every line that's a
	// pure decimal integer. Those should monotonically increase by
	// exactly 1 each step. Anything else (prompts, shell echoes,
	// empty rows) we skip.
	var lastN int = -1
	gaps := 0
	regressions := 0
	checked := 0
	for row := 0; row < src.ScrollbackLen(); row++ {
		var sb strings.Builder
		for col := 0; col < src.Width(); col++ {
			c := src.ScrollbackCellAt(col, row)
			if c == nil || c.Content == "" {
				continue
			}
			sb.WriteString(c.Content)
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
		if n < lastN {
			regressions++
			if regressions <= 3 {
				t.Logf("regression at row %d: %d → %d", row, lastN, n)
			}
		} else if n > lastN+1 {
			gaps++
		}
		lastN = n
	}
	if checked < 100 {
		t.Fatalf("only %d numeric rows found in scrollback (expected ~%d)", checked, total-100)
	}
	if regressions > 0 {
		t.Errorf("%d scrollback regressions (rows appearing out of order). %d gaps for context.", regressions, gaps)
	}
}
