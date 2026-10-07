package supervise

import (
	"encoding/json"
	"testing"
	"time"
)

// TestForwardShellExitsOnce: wait4 and kqueue can both report one
// shell (an adopted shell on macOS is our child and watched). The
// child hears about the exit once; a pid no tab knows still goes
// through.
func TestForwardShellExitsOnce(t *testing.T) {
	parent, childFile, err := Pair()
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	child, err := Wrap(childFile)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()

	s := New(Config{})
	s.tabs[1] = &tabPlumb{pid: 4242}
	s.ctl = parent
	go s.forwardShellExits()

	got := make(chan ExitMsg, 8)
	go func() {
		for {
			kind, payload, _, err := child.Recv()
			if err != nil {
				return
			}
			if kind != KindExit {
				continue
			}
			var m ExitMsg
			_ = json.Unmarshal(payload, &m)
			got <- m
		}
	}()

	s.shellExit <- ExitMsg{PID: 4242, Code: 3}
	s.shellExit <- ExitMsg{PID: 4242, Code: 3} // the second reporter
	s.shellExit <- ExitMsg{PID: 9999, Code: 0} // unknown pid: forwarded
	want := []ExitMsg{{PID: 4242, Code: 3}, {PID: 9999, Code: 0}}
	for i, w := range want {
		select {
		case m := <-got:
			if m != w {
				t.Fatalf("exit %d = %+v, want %+v", i, m, w)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("exit %d never forwarded", i)
		}
	}
	select {
	case m := <-got:
		t.Fatalf("duplicate exit forwarded: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if tp := s.tabs[1]; !tp.exited || tp.code != 3 {
		t.Fatalf("tab not marked exited with code 3: %+v", tp)
	}
}
