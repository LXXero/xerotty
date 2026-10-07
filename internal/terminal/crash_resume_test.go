package terminal

import (
	"syscall"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
	uv "github.com/charmbracelet/ultraviolet"
)

// TestDiskScrollbackRebuildIndex: a store adopted without its offset
// index (crash resume) rebuilds it from the length-prefixed records
// and reads every line back; a torn trailing record is dropped, not
// fatal.
func TestDiskScrollbackRebuildIndex(t *testing.T) {
	d, err := NewDiskScrollback()
	if err != nil {
		t.Skipf("no temp store: %v", err)
	}
	defer d.Close()
	want := []string{"alpha", "beta", "宽字", "delta"}
	for _, w := range want {
		line := make(uv.Line, 1)
		line[0] = uv.Cell{Content: w, Width: 1}
		if err := d.Append(line); err != nil {
			t.Fatal(err)
		}
	}
	f, _, size := d.Handoff()
	// Torn tail: half a header, as if the daemon died mid-Append.
	if _, err := f.WriteAt([]byte{0xff}, size); err != nil {
		t.Fatal(err)
	}
	dup, err := d.DupFile()
	if err != nil {
		t.Fatal(err)
	}
	r := AdoptDiskScrollback(dup, nil, -1)
	defer r.Close()
	if r.Len() != len(want) {
		t.Fatalf("rebuilt Len = %d, want %d", r.Len(), len(want))
	}
	for i, w := range want {
		if got := r.LineAt(i); len(got) == 0 || got[0].Content != w {
			t.Fatalf("line %d = %v, want %q", i, got, w)
		}
	}
	// Appending after the rebuild overwrites the torn tail.
	line := make(uv.Line, 1)
	line[0] = uv.Cell{Content: "after", Width: 1}
	if err := r.Append(line); err != nil {
		t.Fatal(err)
	}
	if got := r.LineAt(len(want)); len(got) == 0 || got[0].Content != "after" {
		t.Fatalf("append after rebuild: %v", got)
	}
}

// TestAdoptForeignChildExit: a terminal adopted around a shell that is
// not our child must not waitpid (it would fail or block forever); it
// reports the exit the supervisor relays through NotifyExit, and Close
// still kills the shell by pid.
func TestAdoptForeignChildExit(t *testing.T) {
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	old, err := NewDaemonHosted(&cfg, 80, 24, "")
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}
	ptmx, pid, disk, err := old.ReleaseForHandoff()
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	neu, err := Adopt(AdoptSpec{Ptmx: ptmx, ChildPID: pid, Cols: 80, Rows: 24, Disk: disk, ForeignChild: true})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !neu.ForeignChild() || neu.ChildPID() != pid {
		t.Fatalf("foreign=%v pid=%d, want true/%d", neu.ForeignChild(), neu.ChildPID(), pid)
	}
	exited := make(chan int, 1)
	neu.SetOnChildExit(func(code int) { exited <- code })

	// The shell is still alive and nothing has been relayed.
	select {
	case code := <-exited:
		t.Fatalf("premature exit %d", code)
	case <-time.After(200 * time.Millisecond):
	}
	neu.Write([]byte("exit 7\r"))
	// The released terminal's old waitChild goroutine still reaps the
	// shell (it is our process child here), so just watch the pid
	// disappear, then relay the code the way a supervisor would.
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	neu.NotifyExit(7)
	select {
	case got := <-exited:
		if got != 7 {
			t.Fatalf("relayed exit code %d, want 7", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnChildExit never fired from NotifyExit")
	}
	neu.Close()
}
