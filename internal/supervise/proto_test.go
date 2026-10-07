package supervise

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// TestFrameAndRightsRoundTrip: frames carry their payload and the
// descriptors attached to them across the socketpair, and a plain
// frame carries none.
func TestFrameAndRightsRoundTrip(t *testing.T) {
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

	f, err := os.CreateTemp(t.TempDir(), "rights")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("hello through scm_rights"); err != nil {
		t.Fatal(err)
	}

	big := bytes.Repeat([]byte("s"), 300000) // bigger than one socket buffer
	sendErr := make(chan error, 2)
	go func() {
		// Stream socket: a frame larger than the socket buffer blocks
		// the sender until the receiver drains, so send off-thread.
		sendErr <- child.SendJSON(KindTab, TabMsg{ID: 7, PID: 4242}, int(f.Fd()))
		sendErr <- child.Send(KindState, big)
	}()

	kind, payload, files, err := parent.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if kind != KindTab || len(files) != 1 {
		t.Fatalf("kind=%d files=%d, want tab frame with one fd", kind, len(files))
	}
	var tm TabMsg
	if err := json.Unmarshal(payload, &tm); err != nil || tm.ID != 7 || tm.PID != 4242 {
		t.Fatalf("payload mangled: %s (%v)", payload, err)
	}
	// Read by offset: the received descriptor shares the sender's
	// file offset, which sits at end-of-file after the WriteString.
	got := make([]byte, 64)
	n, _ := files[0].ReadAt(got, 0)
	got = got[:n]
	if !bytes.Contains(got, []byte("hello through scm_rights")) {
		t.Fatalf("received fd does not read the sender's file: %q", got)
	}
	files[0].Close()

	kind, payload, files, err = parent.Recv()
	if err != nil {
		t.Fatalf("recv state: %v", err)
	}
	if kind != KindState || len(files) != 0 || !bytes.Equal(payload, big) {
		t.Fatalf("state frame mangled: kind=%d files=%d len=%d", kind, len(files), len(payload))
	}

	for i := 0; i < 2; i++ {
		if err := <-sendErr; err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	child.Close()
	if _, _, _, err := parent.Recv(); err == nil {
		t.Fatal("expected EOF after the child closed")
	}
}
