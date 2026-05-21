package clientproto_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/protocol"
)

// TestSubprocessDial spawns `go run ./cmd/xerotty serve --stdio` and
// drives it end-to-end through the same DialCommand path that
// DialSSH uses in production. Exercises:
//
//   - process spawn + pipe setup
//   - hello/attach over a real subprocess transport
//   - PTY echo round-trip through the full daemon stack
//   - clean shutdown on Close (subprocess reaps, no zombie)
//
// Skipped when `go` isn't on PATH or we're cross-compiled to a
// platform where the test runner can't execute it.
func TestSubprocessDial(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not on PATH")
	}
	if runtime.GOOS == "windows" {
		t.Skip("PTY-backed daemon path is unix-only")
	}

	// Find repo root by walking up from this file until we see
	// go.mod, then chdir there so `go run ./cmd/xerottyd` resolves.
	root := findRepoRoot(t)
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir %s: %v", root, err)
	}

	c, err := clientproto.DialCommand("go", "run", "./cmd/xerotty", "serve", "--stdio")
	if err != nil {
		t.Fatalf("dial subprocess: %v", err)
	}
	defer c.Close()

	if _, err := c.Hello("subprocess-test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go c.Run()

	if err := c.Attach("", true); err != nil {
		t.Fatalf("attach: %v", err)
	}

	var attached *protocol.Attached
	select {
	case attached = <-c.Attached():
	case <-time.After(15 * time.Second): // first `go run` compile can be slow
		t.Fatal("timed out waiting for Attached from subprocess daemon")
	}
	if len(attached.Tabs) == 0 {
		t.Fatal("attached with zero tabs")
	}
	tabID := attached.Tabs[0].ID

	var mirror [][]protocol.Cell
	select {
	case full := <-c.CellFull():
		mirror = full.Grid
	case <-time.After(5 * time.Second):
		t.Fatal("no initial CellFull from subprocess")
	}

	marker := "XEROTTY_SUBPROC_OK"
	if err := c.SendInput(tabID, []byte("echo "+marker+"\r")); err != nil {
		t.Fatalf("input: %v", err)
	}

	deadline := time.After(8 * time.Second)
	for {
		if gridContains(mirror, marker) {
			return
		}
		select {
		case full := <-c.CellFull():
			mirror = full.Grid
		case diff := <-c.CellDiff():
			for _, e := range diff.Cells {
				if int(e.Row) < len(mirror) && int(e.Col) < len(mirror[e.Row]) {
					mirror[e.Row][e.Col] = e.Cell
				}
			}
		case <-deadline:
			t.Fatalf("marker %q not seen via subprocess transport", marker)
		}
	}
}

func gridContains(grid [][]protocol.Cell, needle string) bool {
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

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}
