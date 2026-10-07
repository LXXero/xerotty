package runner

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/testutil"
)

// TestUpgradeAdoptsSupervisorE2E: an UNSUPERVISED daemon that is
// hot-upgraded comes back as supervisor + child with its sessions
// intact — the zero-loss migration path for a fleet that predates the
// supervisor. Then the child is SIGKILLed to prove the adopted tabs
// (whose shells are the supervisor's own children) resume too.
func TestUpgradeAdoptsSupervisorE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "xerotty-test")
	build := exec.Command("go", "build", "-tags", "headless", "-o", bin, "./cmd/xerotty")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("test binary build failed: %v\n%s", err, out)
	}
	sd := testutil.SockDir(t)
	sock := filepath.Join(sd, "d.sock")
	mcpSock := filepath.Join(sd, "d.mcp.sock")
	logPath := filepath.Join(tmp, "daemon.log")
	logF, _ := os.Create(logPath)
	defer logF.Close()

	srv := exec.Command(bin, "serve", "--no-supervisor", "--socket", sock, "--mcp-socket", mcpSock)
	srv.Env = append(os.Environ(), "XEROTTY_UPGRADE_BINARY="+bin, "XDG_CACHE_HOME="+tmp, "SHELL=/bin/sh")
	srv.Stdout = logF
	srv.Stderr = logF
	srv.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := srv.Start(); err != nil {
		t.Skipf("start: %v", err)
	}
	daemonPID := srv.Process.Pid
	defer func() {
		_ = syscall.Kill(-srv.Process.Pid, syscall.SIGKILL)
		_, _ = srv.Process.Wait()
		if t.Failed() {
			if b, err := os.ReadFile(logPath); err == nil {
				t.Logf("daemon log:\n%s", b)
			}
		}
	}()
	waitSock := func(path string) bool {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if c, err := net.Dial("unix", path); err == nil {
				c.Close()
				return true
			}
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}
	if !waitSock(sock) || !waitSock(mcpSock) {
		t.Fatal("sockets never came up")
	}
	wcDone, instanceA := attachWireClient(t, sock)
	defer wcDone()
	probe, err := dialProbe(mcpSock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.call("agent/mode", map[string]any{"mode": "auto"}); err != nil {
		t.Fatal(err)
	}
	tabsRes, err := probe.call("tabs/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	var tabs []struct {
		ID uint32 `json:"id"`
	}
	if err := json.Unmarshal(tabsRes, &tabs); err != nil || len(tabs) == 0 {
		t.Fatalf("no tabs: %v %s", err, tabsRes)
	}
	tabID := tabs[0].ID
	if _, err := probe.call("tab/input", map[string]any{"tab_id": tabID, "bytes": "echo PIDIS_$$\r"}); err != nil {
		t.Fatal(err)
	}
	pidRe := regexp.MustCompile(`PIDIS_(\d+)`)
	shellPID := ""
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && shellPID == "" {
		if scr, err := probe.screen(tabID); err == nil {
			if m := pidRe.FindStringSubmatch(scr); m != nil {
				shellPID = m[1]
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if shellPID == "" {
		t.Fatal("shell pid never echoed")
	}

	// A new build installed over the path, as `make build` does: the
	// upgrade has to end with the new inode mapped.
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin+".new", data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bin+".new", bin); err != nil {
		t.Fatal(err)
	}
	newID, err := statFile(bin)
	if err != nil {
		t.Fatal(err)
	}

	// ---- THE UPGRADE: the single process execs into a supervisor.
	up := exec.Command(bin, "serve", "--upgrade", "--socket", sock)
	up.Env = srv.Env
	out, err := up.CombinedOutput()
	if err != nil {
		t.Fatalf("serve --upgrade: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "upgraded to "+bin) {
		t.Fatalf("no success line:\n%s", out)
	}
	mustMap(t, daemonPID, newID, true, "supervisor (the old daemon pid)")
	freshProbe := func() *mcpProbe {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			p, err := dialProbe(mcpSock)
			if err == nil {
				if _, err := p.call("agent/mode", map[string]any{"mode": "auto"}); err == nil {
					return p
				}
				p.conn.Close()
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	}
	probe2 := freshProbe()
	if probe2 == nil {
		t.Fatal("no daemon answered after the upgrade")
	}
	// The old pid is now the supervisor; a daemon child hangs off it.
	child := childOf(t, daemonPID)
	mustMap(t, child, newID, true, "daemon child")
	sameShell := func(p *mcpProbe, marker string) {
		t.Helper()
		if _, err := p.call("tab/input", map[string]any{"tab_id": tabID, "bytes": "echo " + marker + "_$$\r"}); err != nil {
			t.Fatalf("input: %v", err)
		}
		want := marker + "_" + shellPID
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if scr, err := p.screen(tabID); err == nil && strings.Contains(scr, want) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		scr, _ := p.screen(tabID)
		t.Fatalf("same shell did not answer (%s):\n%s", want, scr)
	}
	sameShell(probe2, "ADOPTED")
	scr, _ := probe2.screen(tabID)
	if !strings.Contains(scr, "PIDIS_"+shellPID) {
		t.Fatalf("screen contents lost across the adopting upgrade:\n%s", scr)
	}
	_, instanceB := attachWireClient(t, sock)
	if instanceA != instanceB {
		t.Fatalf("InstanceID changed across adopting upgrade: %q -> %q", instanceA, instanceB)
	}

	// ---- Then a crash of the adopted child: the shells are the
	// supervisor's own children now, and must still come back.
	if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	probe3 := freshProbe()
	if probe3 == nil {
		t.Fatal("no daemon answered after the post-adoption crash")
	}
	if nc := childOf(t, daemonPID); nc == child {
		t.Fatalf("child %d did not change after SIGKILL", child)
	}
	sameShell(probe3, "RESUMED")
}
