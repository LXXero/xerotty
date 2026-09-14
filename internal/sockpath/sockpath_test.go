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

// TestRecordRoundTrip runs against XEROTTY_CACHE_DIR rather than
// XDG_CACHE_HOME: os.UserCacheDir ignores the latter on macOS, so the
// test used to leave test-sock.path in the developer's real cache dir
// and fail its own "empty before record" check on the next run.
func TestRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCacheDir, dir)
	if got := Recorded("test-sock"); got != "" {
		t.Fatalf("expected empty before record, got %q", got)
	}
	if err := Record("test-sock", "/some/where/x.sock"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if got := Recorded("test-sock"); got != "/some/where/x.sock" {
		t.Fatalf("round trip: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "test-sock.path")); err != nil {
		t.Fatalf("recording not under XEROTTY_CACHE_DIR: %v", err)
	}
}

func TestDaemonLogFileHonorsCacheDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCacheDir, dir)
	f := DaemonLogFile()
	if f == nil {
		t.Fatal("DaemonLogFile returned nil")
	}
	f.Close()
	if _, err := os.Stat(filepath.Join(dir, "xerottyd.log")); err != nil {
		t.Fatalf("log not under XEROTTY_CACHE_DIR: %v", err)
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
