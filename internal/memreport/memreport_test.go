package memreport

import (
	"os"
	"runtime/debug"
	"testing"
)

func TestApplyLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer func() { debug.SetMemoryLimit(prev); limitSource = "none" }()

	// The config setting is applied.
	t.Setenv("GOMEMLIMIT", "")
	os.Unsetenv("GOMEMLIMIT")
	lim, src := ApplyLimit(300)
	if lim != 300<<20 || src != "config" {
		t.Fatalf("ApplyLimit(300) = %d, %q; want %d, config", lim, src, 300<<20)
	}
	if got := debug.SetMemoryLimit(-1); got != 300<<20 {
		t.Fatalf("runtime limit = %d, want %d", got, 300<<20)
	}
	if r := Read("test", false); r.MemoryLimitBytes != 300<<20 || r.LimitSource != "config" {
		t.Fatalf("report limit = %d (%s)", r.MemoryLimitBytes, r.LimitSource)
	}

	// GOMEMLIMIT wins: the runtime applied it at start (stood in for
	// here by setting the limit directly), and the config value must
	// not replace it.
	debug.SetMemoryLimit(700 << 20)
	t.Setenv("GOMEMLIMIT", "700MiB")
	lim, src = ApplyLimit(300)
	if lim != 700<<20 || src != "GOMEMLIMIT" {
		t.Fatalf("with GOMEMLIMIT: ApplyLimit(300) = %d, %q; want %d, GOMEMLIMIT", lim, src, 700<<20)
	}

	// 0 means no limit.
	os.Unsetenv("GOMEMLIMIT")
	debug.SetMemoryLimit(noLimit)
	lim, src = ApplyLimit(0)
	if lim != 0 || src != "none" {
		t.Fatalf("ApplyLimit(0) = %d, %q; want 0, none", lim, src)
	}
}

func TestReportAndProfile(t *testing.T) {
	r, err := ReportWithProfile("test", t.TempDir(), true, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.LiveHeapBytes == 0 || r.HeapInUseBytes == 0 || r.ProfilePath == "" {
		t.Fatalf("report incomplete: %+v", r)
	}
	fi, err := os.Stat(r.ProfilePath)
	if err != nil || fi.Size() == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("profile file: %v %v", fi, err)
	}
}
