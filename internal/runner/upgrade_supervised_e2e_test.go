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

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/testutil"
)

// upgradeFixture is a supervised `xerotty serve` running from an
// installed path, with one tab whose shell pid is known and whose
// scrollback holds FILLER_0..39. "Installing" a build renames a new
// file over that path, as `make build` does: a new inode at the old
// path, which is what --upgrade has to get running.
type upgradeFixture struct {
	t        *testing.T
	build    []byte // the freshly built binary, for installs
	bin      string // installed path the daemon runs from
	cli      string // separate copy that runs `serve --upgrade`
	sock     string
	mcpSock  string
	env      []string
	srv      *exec.Cmd
	tabID    uint32
	shellPID string
	instance string
}

func startUpgradeFixture(t *testing.T, serveArgs []string, env ...string) *upgradeFixture {
	t.Helper()
	return startUpgradeFixtureFrom(t, nil, serveArgs, env...)
}

// startUpgradeFixtureFrom starts the daemon from initial (another
// build, e.g. an old release) instead of the fresh build; installs
// and the CLI still use the fresh build.
func startUpgradeFixtureFrom(t *testing.T, initial []byte, serveArgs []string, env ...string) *upgradeFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	tmp := t.TempDir()
	built := filepath.Join(tmp, "xerotty-build")
	build := exec.Command("go", "build", "-tags", "headless", "-o", built, "./cmd/xerotty")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("test binary build failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	f := &upgradeFixture{t: t, build: data, bin: filepath.Join(tmp, "xerotty"), cli: filepath.Join(tmp, "xerotty-cli")}
	if initial == nil {
		initial = data
	}
	f.install(initial)
	if err := os.WriteFile(f.cli, data, 0o755); err != nil {
		t.Fatal(err)
	}
	sd := testutil.SockDir(t)
	f.sock = filepath.Join(sd, "d.sock")
	f.mcpSock = filepath.Join(sd, "d.mcp.sock")
	logPath := filepath.Join(tmp, "daemon.log")
	logF, _ := os.Create(logPath)
	t.Cleanup(func() { logF.Close() })

	f.env = append(append(os.Environ(), "XDG_CACHE_HOME="+tmp, "SHELL=/bin/sh", "XEROTTY_UPGRADE_TIMEOUT=10s"), env...)
	args := append(append([]string{"serve"}, serveArgs...), "--socket", f.sock, "--mcp-socket", f.mcpSock)
	f.srv = exec.Command(f.bin, args...)
	f.srv.Env = f.env
	f.srv.Stdout = logF
	f.srv.Stderr = logF
	// Own process group: cleanup must take supervisor and child.
	f.srv.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := f.srv.Start(); err != nil {
		t.Skipf("start daemon: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			_ = syscall.Kill(-f.srv.Process.Pid, syscall.SIGQUIT)
			time.Sleep(500 * time.Millisecond)
		}
		_ = syscall.Kill(-f.srv.Process.Pid, syscall.SIGKILL)
		_, _ = f.srv.Process.Wait()
		if t.Failed() {
			if b, err := os.ReadFile(logPath); err == nil {
				t.Logf("daemon log:\n%s", b)
			}
		}
	})

	for _, p := range []string{f.sock, f.mcpSock} {
		if !waitDial(p) {
			t.Fatalf("%s never came up", p)
		}
	}
	wcDone, instance := attachWireClient(t, f.sock)
	t.Cleanup(wcDone)
	f.instance = instance
	probe := f.probe()
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
	f.tabID = tabs[0].ID
	if _, err := probe.call("tab/input", map[string]any{"tab_id": f.tabID, "bytes": "echo PIDIS_$$\r"}); err != nil {
		t.Fatal(err)
	}
	pidRe := regexp.MustCompile(`PIDIS_(\d+)`)
	f.waitScreen(probe, func(scr string) bool {
		if m := pidRe.FindStringSubmatch(scr); m != nil {
			f.shellPID = m[1]
		}
		return f.shellPID != ""
	}, "shell pid never echoed")
	if _, err := probe.call("tab/input", map[string]any{"tab_id": f.tabID,
		"bytes": "i=0; while [ $i -lt 40 ]; do echo FILLER_$i; i=$((i+1)); done\r"}); err != nil {
		t.Fatal(err)
	}
	f.waitScreen(probe, func(scr string) bool { return strings.Contains(scr, "FILLER_39") }, "filler never printed")
	// Let the daemon's debounced topology push reach the supervisor.
	time.Sleep(500 * time.Millisecond)
	return f
}

func waitDial(path string) bool {
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

// install renames data over the installed path and returns the new
// file's identity.
func (f *upgradeFixture) install(data []byte) fileID {
	f.t.Helper()
	tmp := f.bin + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(tmp, f.bin); err != nil {
		f.t.Fatal(err)
	}
	id, err := statFile(f.bin)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// upgrade runs `serve --upgrade [args]` from the separate CLI copy.
func (f *upgradeFixture) upgrade(args ...string) (string, error) {
	cmd := exec.Command(f.cli, append([]string{"serve", "--upgrade", "--socket", f.sock}, args...)...)
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// probe returns an MCP probe once a daemon answers on the socket.
func (f *upgradeFixture) probe() *mcpProbe {
	f.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		p, err := dialProbe(f.mcpSock)
		if err == nil {
			if _, err := p.call("agent/mode", map[string]any{"mode": "auto"}); err == nil {
				return p
			}
			p.conn.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.t.Fatal("no daemon answered on the MCP socket")
	return nil
}

func (f *upgradeFixture) waitScreen(p *mcpProbe, ok func(string) bool, msg string) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var scr string
	for time.Now().Before(deadline) {
		var err error
		if scr, err = p.screen(f.tabID); err == nil && ok(scr) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.t.Fatalf("%s:\n%s", msg, scr)
}

// sameSession proves the tab survived: the same shell answers new
// input, the pre-upgrade scrollback is there, and a wire client sees
// the same daemon instance.
func (f *upgradeFixture) sameSession(marker string) {
	f.t.Helper()
	p := f.probe()
	if _, err := p.call("tab/input", map[string]any{"tab_id": f.tabID, "bytes": "echo " + marker + "_$$\r"}); err != nil {
		f.t.Fatalf("input: %v", err)
	}
	want := marker + "_" + f.shellPID
	f.waitScreen(p, func(scr string) bool { return strings.Contains(scr, want) }, "same shell did not answer ("+want+")")
	sb, err := p.call("tab/scrollback", map[string]any{"tab_id": f.tabID, "lines": 100})
	if err != nil {
		f.t.Fatalf("tab/scrollback: %v", err)
	}
	if !strings.Contains(string(sb), "FILLER_0") {
		f.t.Fatalf("scrollback lost: %.300s", sb)
	}
	done, instance := attachWireClient(f.t, f.sock)
	done()
	if instance != f.instance {
		f.t.Fatalf("InstanceID changed: %q -> %q", f.instance, instance)
	}
}

// shellExitCloses proves the daemon still learns when the tab's shell
// exits: a wire client attached now must get the tab's ChildExit
// once `exit` is typed. After an upgrade the shell is no longer the
// daemon child's process child (it belongs to the supervisor, or to
// nobody on macOS, where only a kqueue watch reports it); a lost
// exit leaves a tab open that eats every keystroke.
func (f *upgradeFixture) shellExitCloses() {
	f.t.Helper()
	cli, err := clientproto.Dial(f.sock)
	if err != nil {
		f.t.Fatalf("wire dial: %v", err)
	}
	defer cli.Close()
	if _, err := cli.Hello("upgrade-e2e-exit"); err != nil {
		f.t.Fatalf("wire hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		f.t.Fatalf("wire attach: %v", err)
	}
	select {
	case <-cli.Attached():
	case <-time.After(5 * time.Second):
		f.t.Fatal("never attached")
	}
	p := f.probe()
	if _, err := p.call("tab/input", map[string]any{"tab_id": f.tabID, "bytes": "exit\r"}); err != nil {
		f.t.Fatalf("input: %v", err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ex := <-cli.ChildExit():
			if ex.ID == f.tabID {
				return
			}
		case <-cli.CellFull():
		case <-cli.CellDiff():
		case <-cli.Cursor():
		case <-cli.TabState():
		case <-cli.ScrollbackAppend():
		case <-cli.Topology():
		case <-cli.TabCreated():
		case <-cli.Title():
		case <-cli.Errors():
		case <-cli.Closed():
			f.t.Fatal("wire connection closed before the shell's exit was reported")
		case <-deadline:
			alive := exec.Command("kill", "-0", f.shellPID).Run() == nil
			f.t.Fatalf("shell %s typed exit (still alive: %v) but the daemon never reported tab %d's exit", f.shellPID, alive, f.tabID)
		}
	}
}

func mustMap(t *testing.T, pid int, id fileID, want bool, who string) {
	t.Helper()
	ok, mapped, err := mapsFile(pid, id)
	if err != nil {
		t.Fatalf("%s %d: maps: %v", who, pid, err)
	}
	if ok != want {
		t.Fatalf("%s %d maps target inode %d = %v, want %v (first mapping: %s)", who, pid, id.ino, ok, want, mapped)
	}
}

// TestSupervisedUpgradeE2E: `serve --upgrade` against a supervisor.
// A build the gate rejects must leave everything running and be
// reported as a failure; a real new build must end with a new daemon
// child AND the supervisor itself mapping the new inode, the same
// shell in the same tab, scrollback and screen intact.
func TestSupervisedUpgradeE2E(t *testing.T) {
	f := startUpgradeFixture(t, nil)
	sup := f.srv.Process.Pid
	child := childOf(t, sup)
	oldID, err := statFile(f.bin)
	if err != nil {
		t.Fatal(err)
	}

	// ---- A broken install: the supervisor's validation gate refuses
	// it, nothing changes, and the CLI must not claim otherwise.
	f.install([]byte("#!/bin/sh\nexit 1\n"))
	out, err := f.upgrade()
	if err == nil || strings.Contains(out, "upgraded") || !strings.Contains(out, "FAILED") {
		t.Fatalf("upgrade to a broken build must fail loudly (err %v):\n%s", err, out)
	}
	if c := childOf(t, sup); c != child {
		t.Fatalf("daemon child changed %d -> %d on a rejected upgrade", child, c)
	}
	mustMap(t, child, oldID, true, "daemon child")

	// ---- The real install.
	newID := f.install(f.build)
	out, err = f.upgrade()
	if err != nil {
		t.Fatalf("serve --upgrade: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`upgraded: supervisor \d+ and daemon child \d+ run `).MatchString(out) {
		t.Fatalf("no success line:\n%s", out)
	}
	newChild := childOf(t, sup)
	if newChild == child {
		t.Fatalf("daemon child %d was not replaced:\n%s", child, out)
	}
	mustMap(t, newChild, newID, true, "daemon child")
	mustMap(t, sup, newID, true, "supervisor")
	mustMap(t, sup, oldID, false, "supervisor")

	// The handoff carried the screen, not just the shell: the last
	// filler line is back before anything new is typed (a crash
	// resume would show a blank screen and a fresh prompt).
	p := f.probe()
	f.waitScreen(p, func(scr string) bool { return strings.Contains(scr, "FILLER_39") }, "screen lost across the upgrade")
	f.sameSession("AFTER")
	// The shell was spawned by the replaced daemon child: on Linux it
	// re-parented to the supervisor (subreaper), on macOS to launchd,
	// where only the kqueue watch AdoptHandoff registered reports it.
	f.shellExitCloses()
}

// TestUpgradeOldSupervisorFallbackE2E: a supervisor that predates
// SIGUSR2 upgrades (every host before f026be9) is left alone, and its
// daemon child gets SIGUSR2 instead: it hands off in full and execs
// the new binary in place — same pid, screen intact, no crash resume.
// A build that fails the handoff gate is refused before anything is
// signalled.
func TestUpgradeOldSupervisorFallbackE2E(t *testing.T) {
	f := startUpgradeFixture(t, nil, "XEROTTY_TEST_LEGACY_SUPERVISOR=1")
	sup := f.srv.Process.Pid
	child := childOf(t, sup)
	oldID, err := statFile(f.bin)
	if err != nil {
		t.Fatal(err)
	}

	f.install([]byte("#!/bin/sh\nexit 1\n"))
	out, err := f.upgrade()
	if err == nil || !strings.Contains(out, "nothing was signalled") {
		t.Fatalf("upgrade to a broken build must be refused up front (err %v):\n%s", err, out)
	}
	if c := childOf(t, sup); c != child {
		t.Fatalf("daemon child changed %d -> %d on a refused upgrade", child, c)
	}
	mustMap(t, child, oldID, true, "daemon child")

	newID := f.install(f.build)
	out, err = f.upgrade()
	if err != nil {
		t.Fatalf("serve --upgrade: %v\n%s", err, out)
	}
	for _, want := range []string{"predates in-place upgrades", "sending SIGUSR2 to its daemon child", "re-executed in place", "still runs its old code"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SIGKILL") {
		t.Fatalf("fell back to SIGKILL although the child handles SIGUSR2:\n%s", out)
	}
	if c := childOf(t, sup); c != child {
		t.Fatalf("daemon child pid changed %d -> %d: that was a restart, not an exec in place", child, c)
	}
	mustMap(t, child, newID, true, "daemon child")
	mustMap(t, sup, oldID, true, "supervisor") // can't upgrade itself
	// The full handoff carried the screen; a crash resume through
	// this supervisor would not have.
	f.waitScreen(f.probe(), func(scr string) bool { return strings.Contains(scr, "FILLER_39") }, "screen lost across the upgrade")
	f.sameSession("AFTER")
}

// TestUpgradeRealOldPairE2E runs the fallback against the code xero's
// hosts actually ran: supervisor AND child built from 7f3fb13 (set
// XEROTTY_TEST_OLD_BINARY to use a prebuilt one instead).
func TestUpgradeRealOldPairE2E(t *testing.T) {
	old := oldBuild(t, "7f3fb13")
	f := startUpgradeFixtureFrom(t, old, nil)
	sup := f.srv.Process.Pid
	child := childOf(t, sup)
	oldID, err := statFile(f.bin)
	if err != nil {
		t.Fatal(err)
	}
	newID := f.install(f.build)
	out, err := f.upgrade()
	if err != nil {
		t.Fatalf("serve --upgrade: %v\n%s", err, out)
	}
	if !strings.Contains(out, "re-executed in place") || strings.Contains(out, "SIGKILL") {
		t.Fatalf("want an in-place exec of the old child, no SIGKILL:\n%s", out)
	}
	if c := childOf(t, sup); c != child {
		t.Fatalf("daemon child pid changed %d -> %d", child, c)
	}
	mustMap(t, child, newID, true, "daemon child")
	mustMap(t, sup, oldID, true, "old supervisor")
	f.waitScreen(f.probe(), func(scr string) bool { return strings.Contains(scr, "FILLER_39") }, "screen lost across the upgrade")
	f.sameSession("AFTER")
}

// TestUpgradeKillFallbackE2E: a child that does not answer SIGUSR2 is
// SIGKILLed after the wait so the supervisor's crash resume restarts
// it from the new binary, and the CLI warns about full-screen apps.
func TestUpgradeKillFallbackE2E(t *testing.T) {
	f := startUpgradeFixture(t, nil, "XEROTTY_TEST_LEGACY_SUPERVISOR=1", "XEROTTY_TEST_IGNORE_SIGUSR2=1", "XEROTTY_UPGRADE_TIMEOUT=4s")
	sup := f.srv.Process.Pid
	child := childOf(t, sup)
	newID := f.install(f.build)
	out, err := f.upgrade()
	if err != nil {
		t.Fatalf("serve --upgrade: %v\n%s", err, out)
	}
	for _, want := range []string{"did not re-execute", "SIGKILLing daemon child", "WARNING", "full-screen apps"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	newChild := childOf(t, sup)
	if newChild == child {
		t.Fatalf("daemon child %d was not replaced:\n%s", child, out)
	}
	mustMap(t, newChild, newID, true, "daemon child")
	f.sameSession("AFTER")
	f.shellExitCloses()
}

// TestUpgradeKillFallbackSkippedE2E: when the wait sees no handoff
// but the daemon child already runs the target and serves, it is left
// alone. On macOS a child that had exec'd in place was SIGKILLed this
// way because the wait could not find it.
func TestUpgradeKillFallbackSkippedE2E(t *testing.T) {
	f := startUpgradeFixture(t, nil, "XEROTTY_TEST_LEGACY_SUPERVISOR=1", "XEROTTY_TEST_IGNORE_SIGUSR2=1", "XEROTTY_UPGRADE_TIMEOUT=4s")
	sup := f.srv.Process.Pid
	child := childOf(t, sup)
	// --force with the binary it already runs: no handoff comes, and
	// the child provably runs the target.
	out, err := f.upgrade("--force")
	if err != nil {
		t.Fatalf("serve --upgrade --force: %v\n%s", err, out)
	}
	if !strings.Contains(out, "handoff was not seen") || strings.Contains(out, "SIGKILL") {
		t.Fatalf("want the child left running, no SIGKILL:\n%s", out)
	}
	if c := childOf(t, sup); c != child {
		t.Fatalf("daemon child changed %d -> %d", child, c)
	}
	f.sameSession("KEPT")
}

// TestUpgradeForceE2E: with the installed binary already running,
// --upgrade leaves the daemon alone and --force upgrades it anyway,
// under both kinds of supervisor.
func TestUpgradeForceE2E(t *testing.T) {
	t.Run("supervisor", func(t *testing.T) {
		f := startUpgradeFixture(t, nil)
		sup := f.srv.Process.Pid
		child := childOf(t, sup)
		out, err := f.upgrade()
		if err != nil || !strings.Contains(out, "nothing to do") {
			t.Fatalf("unchanged binary: want nothing to do (err %v):\n%s", err, out)
		}
		if c := childOf(t, sup); c != child {
			t.Fatalf("daemon child changed %d -> %d without --force", child, c)
		}
		out, err = f.upgrade("--force")
		if err != nil || !strings.Contains(out, "upgraded: supervisor") {
			t.Fatalf("--force: %v\n%s", err, out)
		}
		if c := childOf(t, sup); c == child {
			t.Fatalf("--force left daemon child %d in place:\n%s", child, out)
		}
		f.sameSession("FORCED")
	})
	t.Run("old supervisor", func(t *testing.T) {
		f := startUpgradeFixture(t, nil, "XEROTTY_TEST_LEGACY_SUPERVISOR=1")
		sup := f.srv.Process.Pid
		child := childOf(t, sup)
		out, err := f.upgrade()
		if err != nil || !strings.Contains(out, "already runs") {
			t.Fatalf("unchanged binary: want already runs (err %v):\n%s", err, out)
		}
		// Same binary, same pid: only the dropped connection proves
		// the exec happened, so this is the path that needs it.
		out, err = f.upgrade("--force")
		if err != nil || !strings.Contains(out, "re-executed in place") {
			t.Fatalf("--force: %v\n%s", err, out)
		}
		if c := childOf(t, sup); c != child {
			t.Fatalf("daemon child pid changed %d -> %d", child, c)
		}
		f.waitScreen(f.probe(), func(scr string) bool { return strings.Contains(scr, "FILLER_39") }, "screen lost across the upgrade")
		f.sameSession("FORCED")
	})
}

// oldBuild returns a headless binary built from rev, or skips. The
// tree comes from `git archive`, so the build sees exactly that
// commit; its dependencies come from the module cache or the proxy.
func oldBuild(t *testing.T, rev string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds binaries; skipped in -short")
	}
	if p := os.Getenv("XEROTTY_TEST_OLD_BINARY"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("XEROTTY_TEST_OLD_BINARY: %v", err)
		}
		return b
	}
	src := t.TempDir()
	if out, err := exec.Command("sh", "-c", `git -C ../.. archive "$1" | tar -x -C "$2"`, "sh", rev, src).CombinedOutput(); err != nil {
		t.Skipf("cannot check out %s (set XEROTTY_TEST_OLD_BINARY): %v\n%s", rev, err, out)
	}
	bin := filepath.Join(t.TempDir(), "xerotty-"+rev)
	build := exec.Command("go", "build", "-buildvcs=false", "-tags", "headless", "-o", bin, "./cmd/xerotty")
	build.Dir = src
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build %s (set XEROTTY_TEST_OLD_BINARY): %v\n%s", rev, err, out)
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
