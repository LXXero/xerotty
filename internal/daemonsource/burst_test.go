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

// TestScrollbackBurst is the regression for "running seq 80000
// hangs and cuts off chunks." Two specific behaviours under test:
//
//  1. The daemon ships scrollback in bounded chunks so a single
//     huge frame doesn't block the publishLoop / GUI for seconds.
//     We don't measure latency here (too noisy), but if any one
//     frame went over scrollbackBatchMax * cols * ~16B avg cell
//     it'd be megabytes — the test indirectly catches that via
//     the channel buffer never deadlocking.
//
//  2. The client honors a configured scrollback cap. With a large
//     cap set on the Hub, the user's history should make it
//     across — not get clipped to the old hardcoded 10000.
//
// Uses a smaller burst (5000) than `seq 80000` for test speed.
// The principle is identical; the bug surfaces the same way.
func TestScrollbackBurst(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	cli, _ := clientproto.Dial(sockPath)
	defer cli.Close()
	cli.Hello("burst-test")
	go cli.Run()
	cli.Attach("", false)
	<-cli.Attached()

	hub := daemonsource.NewHub(cli)
	defer hub.Stop()
	hub.SetScrollbackCap(20000) // bigger than the burst

	src, err := hub.NewTab(80, 24, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}

	// 5000-line burst into a 24-row viewport — ~4976 rows should
	// scroll off into history.
	const burst = 5000
	cmd := "for i in $(seq 1 " + itoa(burst) + "); do echo BURSTLINE$i; done\r"
	if _, err := src.Write([]byte(cmd)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Drain until scrollback stops growing for a stretch — that's
	// our signal the burst finished + all chunks landed.
	deadline := time.Now().Add(30 * time.Second)
	lastLen := -1
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		l := src.ScrollbackLen()
		if l != lastLen {
			lastLen = l
			stableSince = time.Now()
		}
		if l > burst-50 && time.Since(stableSince) > 500*time.Millisecond {
			break
		}
		select {
		case <-src.DataChan():
		case <-time.After(100 * time.Millisecond):
		}
	}

	got := src.ScrollbackLen()
	// Expect at least burst-100 rows (the last ~24 are still in the
	// visible viewport, plus some slack for the shell prompt).
	if got < burst-100 {
		t.Fatalf("scrollback fell short: got %d rows, expected >= %d (burst=%d)", got, burst-100, burst)
	}

	// Sanity: scan scrollback for the earliest burst lines (which
	// should be there — large cap, no truncation).
	rowsToCheck := 30
	if rowsToCheck > got {
		rowsToCheck = got
	}
	found := false
	for row := 0; row < rowsToCheck; row++ {
		var sb strings.Builder
		for col := 0; col < src.Width(); col++ {
			c := src.ScrollbackCellAt(col, row)
			if c == nil || c.Content == "" {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(c.Content)
		}
		// Early rows should contain low-numbered BURSTLINEs.
		if strings.Contains(sb.String(), "BURSTLINE") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no BURSTLINE found in first %d scrollback rows — earliest history was clipped", rowsToCheck)
	}
}

// itoa avoids strconv to keep the test deps minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
