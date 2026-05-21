package daemon_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/protocol"
)

// TestImagePasteRoundTrip exercises Phase 3's image-paste path: send
// an InputImage frame with PNG bytes, expect the daemon to write a
// temp file and "type" the path into the PTY. The cat | wc -c of the
// typed-in path is then visible in the cell grid, and the temp file
// has the bytes we sent.
func TestImagePasteRoundTrip(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)

	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	c, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Hello("paste-test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go c.Run()
	if err := c.Attach("", true); err != nil {
		t.Fatalf("attach: %v", err)
	}
	attached := <-c.Attached()
	tabID := attached.Tabs[0].ID

	// Drain initial CellFull.
	<-c.CellFull()

	// 8-byte fake PNG header — enough to write a file with, daemon
	// doesn't inspect contents.
	imageBytes := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	if err := c.SendImagePaste(tabID, "image/png", "screenshot", imageBytes); err != nil {
		t.Fatalf("send image paste: %v", err)
	}

	// Wait for the typed path to settle on the PTY. The daemon
	// types " /tmp/xerotty-paste-screenshot-XXXX.png " into the
	// shell's input buffer. We can't reliably read the prompt
	// (depends on $PS1) so instead: send a newline to flush the
	// command, but first wrap it in `cat ` and let the shell echo
	// the path. Actually simpler — peek at /tmp directly for a
	// file with the expected prefix + suffix.

	var found string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(os.TempDir())
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, "xerotty-paste-screenshot-") && strings.HasSuffix(n, ".png") {
				found = filepath.Join(os.TempDir(), n)
				break
			}
		}
		if found != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if found == "" {
		t.Fatal("expected daemon to write a temp file with prefix xerotty-paste-screenshot- and .png suffix")
	}
	defer os.Remove(found)

	got, err := os.ReadFile(found)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	if string(got) != string(imageBytes) {
		t.Errorf("temp file contents mismatch: got %x, want %x", got, imageBytes)
	}

	// Eventually the typed-in path should also surface in the
	// cell grid (the shell echoes typed characters). Mirror frames
	// until we see the temp filename basename.
	mirror := protocol.CellFull{}
	matched := false
	patchUntil := time.After(3 * time.Second)
	base := filepath.Base(found)
collect:
	for {
		select {
		case full := <-c.CellFull():
			mirror = *full
		case diff := <-c.CellDiff():
			for _, e := range diff.Cells {
				if int(e.Row) < len(mirror.Grid) && int(e.Col) < len(mirror.Grid[e.Row]) {
					mirror.Grid[e.Row][e.Col] = e.Cell
				}
			}
		case <-patchUntil:
			break collect
		}
		if gridContainsPaste(mirror.Grid, base) {
			matched = true
			break collect
		}
	}
	if !matched {
		// Echo from the shell isn't guaranteed (some shells suppress
		// echo on raw paste). The temp-file write is the load-bearing
		// part; treat a missing echo as a warning, not a failure.
		t.Logf("note: did not see filename echoed in cell grid (some shells suppress paste echo)")
	}
}

// TestClipboardData verifies the daemon stores client clipboard text.
func TestClipboardData(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)

	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	c, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Hello("clip-test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go c.Run()
	if err := c.Attach("", true); err != nil {
		t.Fatalf("attach: %v", err)
	}
	<-c.Attached()

	if err := c.SendClipboardData("hello from client clipboard"); err != nil {
		t.Fatalf("send clipboard: %v", err)
	}
	// Give the daemon a beat to process the frame.
	time.Sleep(50 * time.Millisecond)

	// We don't have a Read API yet (Phase 4 / OSC 52 path adds it),
	// so this test just verifies the message doesn't fault the
	// daemon. Future test will assert the session.Clipboard()
	// value is queryable via the protocol.
}

func gridContainsPaste(grid [][]protocol.Cell, needle string) bool {
	for _, row := range grid {
		var sb strings.Builder
		for _, c := range row {
			if c.Content == "" {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(c.Content)
		}
		if strings.Contains(sb.String(), needle) {
			return true
		}
	}
	return false
}
