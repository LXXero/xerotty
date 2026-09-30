package daemonsource

import (
	"net"
	"sync"
	"testing"

	"github.com/LXXero/xerotty/internal/clientproto"
)

// TestAdoptConcurrentSameTab: Adopt must hand every concurrent caller
// the SAME Source for a tab ID. Regression for a lookup-then-register
// split that let the GUI MCP create path and the router's topology
// reconcile each build a Source for a freshly created tab; the loser
// of the routing slot had already drained the initial snapshot, so
// the tab on screen showed a bare cursor and never caught up.
func TestAdoptConcurrentSameTab(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	cli := clientproto.Wrap(cConn)
	go cli.Run()
	h := NewHub(cli)
	defer h.Stop()

	const callers = 32
	got := make([]*Source, callers)
	var start, done sync.WaitGroup
	start.Add(1)
	done.Add(callers)
	for i := range got {
		go func(i int) {
			defer done.Done()
			start.Wait()
			got[i] = h.Adopt(7, 80, 24)
		}(i)
	}
	start.Done()
	done.Wait()

	want := h.Lookup(7)
	if want == nil {
		t.Fatal("no Source registered for tab 7")
	}
	for i, s := range got {
		if s != want {
			t.Fatalf("caller %d got a different Source (%p) than the routed one (%p)", i, s, want)
		}
	}
}
