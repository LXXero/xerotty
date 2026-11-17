package daemonspawn

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The launchd job must name a WorkingDirectory: launchd otherwise
// starts it in "/", and every tab created without a cwd opens there.
func TestLaunchdPlistWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "a & <b>")
	plist := launchdPlist("com.example.test", []string{"/bin/true"}, []string{"K=v"}, filepath.Join(dir, "log"), work)
	path := filepath.Join(dir, "job.plist")
	if err := os.WriteFile(path, plist, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s", err, out)
	}
	out, err := exec.Command("plutil", "-extract", "WorkingDirectory", "raw", "-o", "-", path).Output()
	if err != nil {
		t.Fatalf("plutil -extract WorkingDirectory: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != work {
		t.Fatalf("WorkingDirectory = %q, want %q", got, work)
	}
}

func TestSpawnWorkingDirNotRoot(t *testing.T) {
	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir("/"); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if got := spawnWorkingDir(); got != home {
		t.Fatalf("from /: got %q, want $HOME %q", got, home)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	want, _ := os.Getwd()
	if got := spawnWorkingDir(); got != want {
		t.Fatalf("from %s: got %q", want, got)
	}
}
