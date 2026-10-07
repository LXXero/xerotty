//go:build darwin

package supervise

import (
	"os/exec"
	"testing"
	"time"
)

// TestWatchPIDBeforeRunReportsExit: a shell registered by
// AdoptHandoff, before Run, still reports its exit. The sink used to
// be installed in Run, and a watchPID made earlier was dropped, so a
// shell orphaned by a replaced daemon child exited into a tab that
// never closed and ate every keystroke.
func TestWatchPIDBeforeRunReportsExit(t *testing.T) {
	s := New(Config{})
	cmd := exec.Command("sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	watchPID(pid) // as AdoptHandoff does, with Run not started
	go cmd.Wait()
	select {
	case m := <-s.shellExit:
		if m.PID != pid || m.Code != 7 {
			t.Fatalf("exit notice %+v, want pid %d code 7", m, pid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exit of a pid watched before Run never reached the supervisor")
	}
}
