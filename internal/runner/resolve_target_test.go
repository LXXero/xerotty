package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// A bare target name is resolved on PATH, not stat'd in the cwd: ps
// on macOS reports a supervisor launched from PATH as plain "xerotty".
func TestResolveTargetBareNameUsesPATH(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "xt-resolve-probe")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	// cwd holds a same-named file that must NOT be chosen.
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "xt-resolve-probe"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })

	fallback := func() string { t.Fatal("fallback used although PATH resolves"); return "" }
	if got := resolveTarget("xt-resolve-probe", fallback); got != bin {
		t.Fatalf("resolveTarget = %q, want %q", got, bin)
	}
}

func TestResolveTargetBareNameFallsBack(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	want := filepath.Join(t.TempDir(), "xerotty")
	got := resolveTarget("xerotty-not-on-path", func() string { return want })
	if got != want {
		t.Fatalf("resolveTarget = %q, want fallback %q", got, want)
	}
	// A fallback that is itself bare leaves the name for statFile to reject.
	if got := resolveTarget("xerotty-not-on-path", func() string { return "xerotty" }); got != "xerotty-not-on-path" {
		t.Fatalf("resolveTarget = %q, want bare name kept", got)
	}
}

func TestResolveTargetRelativePathMadeAbsolute(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	got := resolveTarget("./xerotty", func() string { t.Fatal("no fallback for a path"); return "" })
	want, _ := filepath.Abs("./xerotty")
	if got != want {
		t.Fatalf("resolveTarget = %q, want %q", got, want)
	}
}
