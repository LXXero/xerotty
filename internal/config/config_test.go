package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConfig drops a config.toml in a scratch dir and points
// XEROTTY_CONFIG_DIR at it, so Load() reads our fixture on every
// platform. (It used to set XDG_CONFIG_HOME, which os.UserConfigDir
// honors only on Linux — on macOS these tests read the developer's
// real config and failed.)
func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvConfigDir, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDirOverride pins the override contract: XEROTTY_CONFIG_DIR IS
// the directory holding config.toml (no xerotty/ nesting), and the
// unset default nests under the platform config dir.
func TestDirOverride(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv(EnvConfigDir, scratch)
	if got := Dir(); got != scratch {
		t.Fatalf("Dir() = %q, want override %q", got, scratch)
	}
	if got := Path(); got != filepath.Join(scratch, "config.toml") {
		t.Fatalf("Path() = %q, want %q", got, filepath.Join(scratch, "config.toml"))
	}

	t.Setenv(EnvConfigDir, "")
	if got := Dir(); filepath.Base(got) != "xerotty" {
		t.Fatalf("default Dir() = %q, want .../xerotty", got)
	}
	if got := Path(); got != filepath.Join(Dir(), "config.toml") {
		t.Fatalf("default Path() = %q, want Dir()/config.toml", got)
	}
}

func TestDefaultMCPAndTabs(t *testing.T) {
	c := Default()
	if c.MCP.DefaultMode != "observe" {
		t.Errorf("default MCP mode = %q, want observe", c.MCP.DefaultMode)
	}
	if !c.MCP.AllowModeChange {
		t.Errorf("default allow_mode_change = false, want true")
	}
	if c.MCP.ApprovalToken != "" {
		t.Errorf("default approval_token = %q, want empty", c.MCP.ApprovalToken)
	}
	// Tabs.Source defaults to "" (App treats empty as in-process pty).
	if c.Tabs.Source != "" {
		t.Errorf("default Tabs.Source = %q, want empty", c.Tabs.Source)
	}
}

func TestLoadDaemonSourceAndHosts(t *testing.T) {
	writeConfig(t, `
[tabs]
source = "daemon:kh"
daemon_socket = "/run/x.sock"

[[hosts]]
name = "kh"
ssh_dest = "kh.zaxxon.cc"
ssh_args = ["-i", "~/.ssh/id_ed25519", "-p", "2222"]

[[hosts]]
name = "vps"
ssh_dest = "vps"
remote_cmd = "/opt/xerotty/bin/xerotty serve --stdio"
`)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Tabs.Source != "daemon:kh" {
		t.Errorf("Tabs.Source = %q, want daemon:kh", c.Tabs.Source)
	}
	if c.Tabs.DaemonSocket != "/run/x.sock" {
		t.Errorf("DaemonSocket = %q", c.Tabs.DaemonSocket)
	}
	if len(c.Hosts) != 2 {
		t.Fatalf("want 2 hosts, got %d", len(c.Hosts))
	}
	kh := c.Hosts[0]
	if kh.Name != "kh" || kh.SSHDest != "kh.zaxxon.cc" {
		t.Errorf("host[0] = %+v", kh)
	}
	if len(kh.SSHArgs) != 4 || kh.SSHArgs[0] != "-i" || kh.SSHArgs[3] != "2222" {
		t.Errorf("host[0] ssh_args = %v", kh.SSHArgs)
	}
	vps := c.Hosts[1]
	if vps.RemoteCmd != "/opt/xerotty/bin/xerotty serve --stdio" {
		t.Errorf("host[1] remote_cmd = %q", vps.RemoteCmd)
	}
}

func TestLoadMCPTrustBlock(t *testing.T) {
	writeConfig(t, `
[mcp]
default_mode = "propose"
allow_mode_change = false
approval_token = "s3cret"
`)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MCP.DefaultMode != "propose" {
		t.Errorf("DefaultMode = %q, want propose", c.MCP.DefaultMode)
	}
	if c.MCP.AllowModeChange {
		t.Errorf("AllowModeChange = true, want false")
	}
	if c.MCP.ApprovalToken != "s3cret" {
		t.Errorf("ApprovalToken = %q, want s3cret", c.MCP.ApprovalToken)
	}
}

// TestLoadPartialMCPPreservesDefaults guards the decode-merge
// footgun: a [mcp] block that sets ONLY default_mode must not zero
// out allow_mode_change (the toml decoder leaves absent fields at
// their Default() value rather than Go's bool zero). If this ever
// regresses, a user enabling propose-as-default would silently
// also lose the mode-change lock's "true" default.
func TestLoadPartialMCPPreservesDefaults(t *testing.T) {
	writeConfig(t, `
[mcp]
default_mode = "propose"
`)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MCP.DefaultMode != "propose" {
		t.Errorf("DefaultMode = %q, want propose", c.MCP.DefaultMode)
	}
	if !c.MCP.AllowModeChange {
		t.Errorf("partial [mcp] zeroed AllowModeChange; want preserved default true")
	}
}

// TestLoadNoConfigReturnsDefaults — missing file → Default(), no error.
func TestLoadNoConfigReturnsDefaults(t *testing.T) {
	t.Setenv(EnvConfigDir, t.TempDir()) // empty dir, no config.toml
	c, err := Load()
	if err != nil {
		t.Fatalf("Load with no file: %v", err)
	}
	if c.MCP.DefaultMode != "observe" || c.Tabs.Source != "" {
		t.Errorf("missing config didn't fall back to defaults: %+v", c.MCP)
	}
}
