package daemonsource

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/protocol"
)

// fakeDaemon is the server end of an in-process net.Pipe. It records
// the TabCreate requests it receives and lets the test send back
// TabCreated frames with chosen ReqIDs — so the request-correlation
// logic can be exercised deterministically (including late/foreign
// acks that a real daemon would never produce).
type fakeDaemon struct {
	conn    net.Conn
	creates chan *protocol.TabCreate
	writeMu sync.Mutex
}

func newFakeDaemon(conn net.Conn) *fakeDaemon {
	f := &fakeDaemon{conn: conn, creates: make(chan *protocol.TabCreate, 16)}
	go f.readLoop()
	return f
}

func (f *fakeDaemon) readLoop() {
	fr := protocol.NewFrameReader(f.conn)
	for {
		t, body, err := fr.ReadFrame()
		if err != nil {
			return
		}
		if t == protocol.MsgTabCreate {
			var m protocol.TabCreate
			if _, err := m.UnmarshalMsg(body); err != nil {
				return
			}
			f.creates <- &m
		}
	}
}

func (f *fakeDaemon) sendTabCreated(reqID uint64, tabID uint32) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_ = protocol.WriteFrame(f.conn, protocol.MsgTabCreated, &protocol.TabCreated{
		Info:  protocol.TabInfo{ID: tabID, Cols: 80, Rows: 24},
		ReqID: reqID,
	})
}

// TestNewTabInDropsLateAck verifies request correlation: a TabCreated
// that arrives after its create timed out (a different ReqID than the
// pending one) is dropped — it does NOT adopt the wrong tab or poison
// the next create.
func TestNewTabInDropsLateAck(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	fake := newFakeDaemon(sConn)

	cli := clientproto.Wrap(cConn)
	go cli.Run()
	h := NewHub(cli)
	defer h.Stop()
	h.createTimeout = 200 * time.Millisecond

	// Create #1: the fake daemon never replies → NewTabIn times out.
	if _, err := h.NewTabIn(0, 80, 24, ""); err == nil {
		t.Fatal("expected timeout error when no ack arrives")
	}
	c1 := <-fake.creates // the create #1 request (its ReqID)

	// A LATE ack for create #1 arrives now (after the waiter gave up).
	fake.sendTabCreated(c1.ReqID, 99)

	// Create #2: the fake replies promptly with the matching ReqID.
	go func() {
		c2 := <-fake.creates
		fake.sendTabCreated(c2.ReqID, 42)
	}()
	src, err := h.NewTabIn(0, 80, 24, "")
	if err != nil {
		t.Fatalf("create #2 unexpectedly failed: %v", err)
	}
	if src.TabID() != 42 {
		t.Fatalf("create #2 adopted tab %d, want 42 (late ack poisoned it?)", src.TabID())
	}
	// The router processes frames in order, so by the time create #2's
	// ack was handled the late ack for #1 was already handled (and
	// dropped) — so tab 99 must never have been adopted.
	if h.lookup(99) != nil {
		t.Fatal("late ack for the timed-out create adopted tab 99")
	}
}

// TestNewTabInConcurrentCreates verifies two in-flight creates each
// adopt THEIR OWN tab — the shared TabCreated stream is demuxed by
// ReqID, so neither waiter steals the other's ack.
func TestNewTabInConcurrentCreates(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	fake := newFakeDaemon(sConn)

	cli := clientproto.Wrap(cConn)
	go cli.Run()
	h := NewHub(cli)
	defer h.Stop()

	// Server: reply to each create with a distinct tab ID, but in the
	// REVERSE order they arrive — so a first-come-first-served waiter
	// would mismatch.
	go func() {
		first := <-fake.creates
		second := <-fake.creates
		fake.sendTabCreated(second.ReqID, 200)
		fake.sendTabCreated(first.ReqID, 100)
	}()

	type res struct {
		src *Source
		err error
	}
	results := make(chan res, 2)
	go func() { s, e := h.NewTabIn(0, 80, 24, ""); results <- res{s, e} }()
	// Tiny stagger so the two creates have a defined arrival order.
	time.Sleep(20 * time.Millisecond)
	go func() { s, e := h.NewTabIn(0, 80, 24, ""); results <- res{s, e} }()

	got := map[uint32]bool{}
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatalf("NewTabIn failed: %v", r.err)
			}
			got[r.src.TabID()] = true
		case <-time.After(3 * time.Second):
			t.Fatal("NewTabIn timed out")
		}
	}
	if !got[100] || !got[200] {
		t.Fatalf("concurrent creates adopted %v, want {100,200}", got)
	}
}

// TestApplyTopologyReconciles verifies the Hub reconcile: it adopts
// newly-appeared tab IDs, drops vanished ones (marking the Source
// closed), and ignores stale/duplicate snapshots via the revision
// gate.
func TestApplyTopologyReconciles(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	newFakeDaemon(sConn) // drains client writes (none expected)

	cli := clientproto.Wrap(cConn)
	go cli.Run()
	h := NewHub(cli)
	defer h.Stop()

	// Seed: pretend we adopted tab 1 at attach (revision 4).
	h.SeedRevision(4)
	h.Adopt(1, 80, 24)

	// Revision 5 adds tab 2.
	h.applyTopology(&protocol.TopologyChanged{
		Revision: 5,
		Tabs:     []protocol.TabInfo{{ID: 1, Cols: 80, Rows: 24}, {ID: 2, Cols: 80, Rows: 24}},
	})
	if h.lookup(1) == nil || h.lookup(2) == nil {
		t.Fatal("revision 5 should leave tabs 1 and 2 adopted")
	}

	// A STALE revision (3) must be ignored — no drops.
	h.applyTopology(&protocol.TopologyChanged{Revision: 3, Tabs: nil})
	if h.lookup(1) == nil || h.lookup(2) == nil {
		t.Fatal("stale revision must not mutate the adopted set")
	}

	// Revision 6 drops tab 1 (vanished).
	s1 := h.lookup(1)
	h.applyTopology(&protocol.TopologyChanged{
		Revision: 6,
		Tabs:     []protocol.TabInfo{{ID: 2, Cols: 80, Rows: 24}},
	})
	if h.lookup(1) != nil {
		t.Fatal("tab 1 should be dropped after vanishing from topology")
	}
	if !s1.IsClosed() {
		t.Fatal("vanished tab's Source should be marked closed")
	}
	if h.lookup(2) == nil {
		t.Fatal("tab 2 should remain adopted")
	}
}
