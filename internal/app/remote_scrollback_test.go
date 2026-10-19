package app

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/testutil"
)

// A hub registered by the async SSH dial path must get the user's
// scrollback strategy. It used to be left at the Hub defaults (a fixed
// 10000-row mirror) because only the default and local hubs were
// configured, so on a remote tab "unlimited" never windowed: history
// past 10000 rows was unreachable and a scrolled-up view moved with
// new output.
func TestRegisterRemoteHubAppliesScrollbackConfig(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		lines    int
		windowed bool
		cap      int
	}{
		{"unlimited", 10000, true, 0},
		{"memory", 5000, false, 5000},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			sockPath := filepath.Join(testutil.SockDir(t), "xerottyd.sock")
			dcfg := config.Default()
			d := daemon.New(&dcfg, sockPath)
			done := make(chan error, 1)
			go func() { done <- d.Run() }()
			defer func() { _ = d.Stop(); <-done }()
			time.Sleep(50 * time.Millisecond)

			cli, err := clientproto.Dial(sockPath)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer cli.Close()
			cli.Hello("remote-scrollback")
			go cli.Run()
			cli.Attach("", false)
			attached := <-cli.Attached()

			cfg := config.Default()
			cfg.Scrollback.Mode = tc.mode
			cfg.Scrollback.Lines = tc.lines
			a := &App{cfg: cfg, remoteHubs: map[string]*remoteHubEntry{}}
			entry := a.registerRemoteHub("xryzen", cli, attached)
			defer entry.hub.Stop()

			if got := entry.hub.ScrollbackWindowed(); got != tc.windowed {
				t.Errorf("windowed = %v, want %v", got, tc.windowed)
			}
			if got := entry.hub.ScrollbackCap(); got != tc.cap {
				t.Errorf("cap = %d, want %d", got, tc.cap)
			}
		})
	}
}
