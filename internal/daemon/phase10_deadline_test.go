package daemon

import (
	"net"
	"testing"
	"time"
)

// TestDeadlineWriterBigButFlowing verifies Phase 10 layer 3: a large
// write that keeps making progress is NEVER killed, even though its
// total duration far exceeds the idle window — because no single gap
// with zero bytes moving reaches the window. The reader drains in small
// chunks with small gaps; total time >> window, per-gap << window.
func TestDeadlineWriterBigButFlowing(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()

	const window = 60 * time.Millisecond
	dw := &deadlineWriter{conn: cConn, window: window}

	payload := make([]byte, 256*1024) // big enough that draining > window

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, err := sConn.Read(buf)
			if n > 0 {
				time.Sleep(3 * time.Millisecond) // steady progress, gap << window
			}
			if err != nil {
				return
			}
		}
	}()

	start := time.Now()
	n, err := dw.Write(payload)
	if err != nil {
		t.Fatalf("big-but-flowing write was killed: %v (wrote %d/%d)", err, n, len(payload))
	}
	if n != len(payload) {
		t.Fatalf("short write: %d/%d", n, len(payload))
	}
	if elapsed := time.Since(start); elapsed <= window {
		t.Fatalf("test didn't exercise the refresh path: write took %s (<= window %s)", elapsed, window)
	}
	_ = cConn.Close()
	<-readDone
}

// TestDeadlineWriterStalledKilled verifies the converse: a write where
// NO bytes move for the window is declared dead (timeout error) within
// roughly the window — so a genuinely stuck client unblocks the writer.
func TestDeadlineWriterStalledKilled(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	// No reader on sConn → the write makes zero progress.

	dw := &deadlineWriter{conn: cConn, window: 60 * time.Millisecond}
	start := time.Now()
	_, err := dw.Write([]byte("payload that nobody reads"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a stalled write was not killed")
	}
	if !isTimeout(err) {
		t.Fatalf("expected a timeout error, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("stalled write took too long to fail: %s", elapsed)
	}
}
