package runner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// binaryReplaced gates the bridge's exec-in-place self-upgrade, so it
// must fire on a real change and never on a missing snapshot/file
// (which would risk exec'ing into nothing).
func TestBinaryReplaced(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "xerotty")
	if err := os.WriteFile(p, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	start, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	if binaryReplaced(start, p) {
		t.Error("unchanged binary reported as replaced")
	}
	if binaryReplaced(nil, p) {
		t.Error("nil snapshot must be false (no baseline to compare)")
	}
	if binaryReplaced(start, filepath.Join(dir, "missing")) {
		t.Error("missing file must be false (don't exec into nothing)")
	}

	// Size change → replaced.
	if err := os.WriteFile(p, []byte("v2-larger"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !binaryReplaced(start, p) {
		t.Error("size change not detected")
	}

	// Same size, newer mtime → replaced.
	if err := os.WriteFile(p, []byte("v3"), 0o755); err != nil {
		t.Fatal(err)
	}
	snap, _ := os.Stat(p)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if !binaryReplaced(snap, p) {
		t.Error("mtime change not detected")
	}
}
