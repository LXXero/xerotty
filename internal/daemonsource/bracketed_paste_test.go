package daemonsource

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/testutil"
)

// TestSourceBracketedPasteMirrored: the tab's DECSET 2004 state
// reaches the client Source over TabState, both off and on, so the
// GUI MCP's send_keys knows when to wrap its text in paste markers.
func TestSourceBracketedPasteMirrored(t *testing.T) {
	sockPath := filepath.Join(testutil.SockDir(t), "xerottyd.sock")
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
	if _, err := cli.Hello("bracketed-test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	<-cli.Attached()

	hub := NewHub(cli)
	defer hub.Stop()
	src, err := hub.NewTab(80, 24, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// Both transitions happen while the command runs, so a shell that
	// toggles 2004 at its own prompt can't change the result.
	cmd := `printf '\033[?2004l'; sleep 2; printf '\033[?2004h'; sleep 5` + "\r"
	if _, err := src.Write([]byte(cmd)); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor := func(want bool) {
		deadline := time.Now().Add(4 * time.Second)
		for src.BracketedPasteMode() != want {
			if time.Now().After(deadline) {
				t.Fatalf("BracketedPasteMode never became %v", want)
			}
			select {
			case <-src.DataChan():
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	waitFor(false)
	waitFor(true)
}
