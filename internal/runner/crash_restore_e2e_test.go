package runner

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/testutil"
)

// TestCrashRestoreE2E is the supervisor's acceptance test: a daemon
// child dies hard (SIGKILL — no handoff written by the daemon itself),
// and the supervisor brings a new daemon up on the SAME socket with
// the SAME shell in the same tab, scrollback intact, input working.
func TestCrashRestoreE2E(t *testing.T) {
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

	srv := exec.Command(bin, "serve", "--socket", sock, "--mcp-socket", mcpSock)
	srv.Env = append(os.Environ(), "XDG_CACHE_HOME="+tmp, "SHELL=/bin/sh")
	srv.Stdout = logF
	srv.Stderr = logF
	srv.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := srv.Start(); err != nil {
		t.Skipf("start: %v", err)
	}
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
	if !waitSock(sock) {
		t.Fatal("socket never came up")
	}
	wcDone, instanceA := attachWireClient(t, sock)
	defer wcDone()
	if !waitSock(mcpSock) {
		t.Fatal("mcp socket never came up")
	}
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
	tabID := int(tabs[0].ID)

	// Shell pid on screen first (read it before anything scrolls it
	// away), then a marker pushed into scrollback by filler lines.
	if _, err := probe.call("tab/input", map[string]any{"tab_id": tabID, "bytes": "echo PIDIS_$$\r"}); err != nil {
		t.Fatal(err)
	}
	pidRe := regexp.MustCompile(`PIDIS_(\d+)`)
	var shellPID string
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && shellPID == "" {
		if scr, err := probe.screen(uint32(tabID)); err == nil {
			if mm := pidRe.FindStringSubmatch(scr); mm != nil {
				shellPID = mm[1]
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if shellPID == "" {
		scr, serr := probe.screen(uint32(tabID))
		t.Fatalf("shell pid never echoed (tab %d, screen err %v):\n%s", tabID, serr, scr)
	}
	if _, err := probe.call("tab/input", map[string]any{"tab_id": tabID,
		"bytes": "i=0; while [ $i -lt 40 ]; do echo FILLER_$i; i=$((i+1)); done\r"}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if scr, err := probe.screen(uint32(tabID)); err == nil && strings.Contains(scr, "FILLER_39") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Let the daemon's debounced topology push reach the supervisor.
	time.Sleep(500 * time.Millisecond)

	// ---- THE CRASH: find the supervised child and SIGKILL it.
	childPID := childOf(t, srv.Process.Pid)
	if err := syscall.Kill(childPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill child %d: %v", childPID, err)
	}

	// The socket must stay bound (the supervisor holds it) and a new
	// daemon must answer on it with the same tab.
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
		t.Fatal("no daemon answered after the crash")
	}
	newChild := childOf(t, srv.Process.Pid)
	if newChild == childPID {
		t.Fatalf("child pid %d did not change — was it really killed?", childPID)
	}
	if err := syscall.Kill(int(mustAtoi(shellPID)), 0); err != nil {
		t.Fatalf("shell %s did not survive the daemon's death: %v", shellPID, err)
	}

	// Same instance, same tab, same shell answering new input.
	wcDone2, instanceB := attachWireClient(t, sock)
	defer wcDone2()
	if instanceA == "" || instanceA != instanceB {
		t.Fatalf("InstanceID changed across crash resume: %q -> %q", instanceA, instanceB)
	}
	if _, err := probe2.call("tab/input", map[string]any{"tab_id": tabID, "bytes": "echo AGAIN_$$\r"}); err != nil {
		t.Fatalf("post-crash input: %v", err)
	}
	want := "AGAIN_" + shellPID
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		scr, err := probe2.screen(uint32(tabID))
		if err == nil && strings.Contains(scr, want) {
			// And scrollback came back through the rebuilt index.
			sb, err := probe2.call("tab/scrollback", map[string]any{"tab_id": tabID, "lines": 100})
			if err != nil {
				t.Fatalf("tab/scrollback after resume: %v", err)
			}
			if !strings.Contains(string(sb), "FILLER_0") {
				t.Fatalf("scrollback lost across the crash: %.200s", sb)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	scr, _ := probe2.screen(uint32(tabID))
	t.Fatalf("same shell did not answer after the crash (want %s):\n%s", want, scr)
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// childOf returns the daemon child pid under supervisor ppid, polling
// briefly so a just-respawned child is found. Filtered on the child's
// argv: after an adopting upgrade the supervisor's children include
// the shells too (they were the old daemon's), and the first pgrep
// hit would be one of those.
func childOf(t *testing.T, ppid int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("pgrep", "-P", strconv.Itoa(ppid), "-f", "serve --child").Output()
		if err == nil {
			for _, f := range strings.Fields(string(out)) {
				if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
					return pid
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no child process of %d", ppid)
	return 0
}
