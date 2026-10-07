package supervise

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunSeesDaemonThatExitsRightAfterStart: a child that dies before
// spawn records its pid is still reported as the daemon, so Run sees
// its exit instead of mistaking it for a shell and waiting forever.
func TestRunSeesDaemonThatExitsRightAfterStart(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	s := New(Config{
		Binary:     "/bin/true",
		SocketPath: filepath.Join(t.TempDir(), "sock"),
		NoMCP:      true,
		Listener:   r,
	})
	// Hold the window between Start and the pid assignment open long
	// enough for /bin/true to exit and SIGCHLD to reach the reaper.
	s.afterStart = func() { time.Sleep(300 * time.Millisecond) }

	done := make(chan error, 1)
	go func() { done <- s.Run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run never saw the daemon exit")
	}
}
