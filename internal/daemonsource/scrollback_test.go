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

// TestScrollbackStream produces output that overflows the visible
// viewport, then asserts the rows that scrolled off the top reach
// the client's scrollback ring. Without scrollback streaming the
// shadow emulator would just lose history at the top edge.
//
// Uses `for i in $(seq 1 60); do echo SBLINE$i; done` so we know
// exactly how many lines to expect.
func TestScrollbackStream(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
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
	if _, err := cli.Hello("scrollback-test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	<-cli.Attached()

	hub := daemonsource.NewHub(cli)
	defer hub.Stop()

	// Small grid so we don't need a million lines to overflow.
	src, err := hub.NewTab(40, 10, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}

	// Print 60 lines into a 10-row viewport. ~50 rows should
	// roll off the top into scrollback.
	cmd := "for i in $(seq 1 60); do echo SBLINE$i; done\r"
	if _, err := src.Write([]byte(cmd)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Poll until scrollback has grown. The shell's prompt + the
	// 60 echo lines should kick at least ~50 rows into scrollback.
	// Allow a generous deadline for the shell to actually run.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if src.ScrollbackLen() >= 20 {
			break
		}
		select {
		case <-src.DataChan():
		case <-time.After(150 * time.Millisecond):
		}
	}
	if src.ScrollbackLen() < 20 {
		t.Fatalf("scrollback only grew to %d rows; expected >= 20 after 60-line burst", src.ScrollbackLen())
	}

	// Sanity: at least one of the SBLINE markers landed in
	// scrollback (rather than only in the visible viewport).
	found := false
	for row := 0; row < src.ScrollbackLen(); row++ {
		var sb strings.Builder
		for col := 0; col < src.Width(); col++ {
			c := src.ScrollbackCellAt(col, row)
			if c == nil || c.Content == "" {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(c.Content)
		}
		if strings.Contains(sb.String(), "SBLINE") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no SBLINE found in scrollback rows (had %d rows)", src.ScrollbackLen())
	}
}
