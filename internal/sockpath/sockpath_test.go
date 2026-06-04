package sockpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRuntimeDirIgnoresTMPDIR is the macOS regression: $TMPDIR
// differs per launch context (launchd vs shell vs agent-spawned), so
// the socket dir must not depend on it. Two processes with different
// TMPDIRs (simulated here by flipping the env) must agree.
func TestRuntimeDirIgnoresTMPDIR(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())
	a := RuntimeDir()
	t.Setenv("TMPDIR", t.TempDir())
	b := RuntimeDir()
	if a != b {
		t.Fatalf("RuntimeDir depends on TMPDIR: %q vs %q", a, b)
	}
	if fi, err := os.Stat(a); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("runtime dir %q not a 0700 dir (err=%v)", a, err)
	}
}

func TestRuntimeDirPrefersXDG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	if got := RuntimeDir(); got != dir {
		t.Fatalf("XDG_RUNTIME_DIR not honored: %q", got)
	}
}

func TestRecordRoundTrip(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // isolate UserCacheDir
	if got := Recorded("test-sock"); got != "" {
		t.Fatalf("expected empty before record, got %q", got)
	}
	if err := Record("test-sock", "/some/where/x.sock"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if got := Recorded("test-sock"); got != "/some/where/x.sock" {
		t.Fatalf("round trip: %q", got)
	}
}

func TestMCPSocketFor(t *testing.T) {
	if got := MCPSocketFor("/run/u/xerottyd.sock"); got != "/run/u/xerottyd.mcp.sock" {
		t.Fatalf("sock suffix: %q", got)
	}
	if got := MCPSocketFor("/run/u/custom"); !strings.HasSuffix(got, "custom.mcp.sock") || filepath.Dir(got) != "/run/u" {
		t.Fatalf("no-suffix case: %q", got)
	}
}
