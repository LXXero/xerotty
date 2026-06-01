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
	"github.com/LXXero/xerotty/internal/terminal"
)

// TestSourceRoundTrip runs a daemon in-process, attaches a Hub +
// Source, sends an echo via Source.Write, and verifies the marker
// shows up in the shadow emulator that backs Source. Exercises the
// full GUI ↔ daemon path the SDL3 app will use in daemon mode.
func TestSourceRoundTrip(t *testing.T) {
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
	if _, err := cli.Hello("daemonsource-test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	<-cli.Attached()

	hub := daemonsource.NewHub(cli)
	defer hub.Stop()

	src, err := hub.NewTab(80, 24, "")
	if err != nil {
		t.Fatalf("new tab via hub: %v", err)
	}
	// Compile-time assertion already checked, but confirm runtime
	// shape too — every Source method should be reachable through
	// the terminal.Source interface.
	var iface terminal.Source = src
	_ = iface

	marker := "XEROTTY_DSOURCE_OK"
	if _, err := src.Write([]byte("echo " + marker + "\r")); err != nil {
		t.Fatalf("Source.Write: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if emulatorContains(src, marker) {
			return
		}
		select {
		case <-src.DataChan():
		case <-time.After(100 * time.Millisecond):
		}
	}
	dump := emulatorDump(src)
	t.Fatalf("marker %q never reached shadow emulator. Last grid:\n%s", marker, dump)
}

// TestSourceTabState confirms a TabState push from the daemon
// surfaces on the Source's GetCWD / ForegroundProcessName /
// AppCursorMode getters.
func TestSourceTabState(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	cli, _ := clientproto.Dial(sockPath)
	defer cli.Close()
	cli.Hello("ts-test")
	go cli.Run()
	cli.Attach("", false)
	<-cli.Attached()

	hub := daemonsource.NewHub(cli)
	defer hub.Stop()
	src, err := hub.NewTab(80, 24, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}

	// Initial state push happens on attach to the new tab. CWD
	// should populate within a beat. Foreground proc name may be
	// empty briefly if the shell hasn't fully started.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if src.GetCWD() != "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("never got a non-empty CWD via TabState (daemon should push at attach)")
}

// emulatorContains walks a SnapshotViewport (not the live emu) so
// the test doesn't race the router goroutine writing into the
// shadow. Going through emu.CellAt directly returns live pointers
// that the router can mutate between dereferences.
func emulatorContains(s *daemonsource.Source, needle string) bool {
	grid := s.SnapshotViewport()
	for _, row := range grid {
		var sb strings.Builder
		for _, cell := range row {
			if cell.Content == "" {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(cell.Content)
		}
		if strings.Contains(sb.String(), needle) {
			return true
		}
	}
	return false
}

func emulatorDump(s *daemonsource.Source) string {
	grid := s.SnapshotViewport()
	var sb strings.Builder
	for _, row := range grid {
		for _, cell := range row {
			if cell.Content == "" {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(cell.Content)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}
