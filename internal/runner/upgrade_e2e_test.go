package runner

import (
	"bufio"
	"encoding/json"
	"fmt"
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
)

// mcpProbe is a minimal line-JSON-RPC client for poking the test
// daemon's MCP socket.
type mcpProbe struct {
	conn net.Conn
	br   *bufio.Reader
	id   int
}

func dialProbe(path string) (*mcpProbe, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	return &mcpProbe{conn: conn, br: bufio.NewReader(conn)}, nil
}

func (p *mcpProbe) call(method string, params any) (json.RawMessage, error) {
	p.id++
	req := map[string]any{"jsonrpc": "2.0", "id": p.id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if _, err := fmt.Fprintln(p.conn, string(b)); err != nil {
		return nil, err
	}
	line, err := p.br.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("rpc %s: %s", method, resp.Error.Message)
	}
	return resp.Result, nil
}

func (p *mcpProbe) screen(tabID uint32) (string, error) {
	res, err := p.call("tab/screen", map[string]any{"tab_id": tabID})
	if err != nil {
		return "", err
	}
	var sc struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(res, &sc); err != nil {
		return "", err
	}
	return strings.Join(sc.Lines, "\n"), nil
}

// TestHotUpgradeE2E is THE hot-upgrade acceptance test: a real
// `xerotty serve` process receives SIGUSR2, execs itself in place,
// and the shell inside survives — same shell PID, same daemon PID,
// restored screen, working input.
func TestHotUpgradeE2E(t *testing.T) {
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

	sock := filepath.Join(tmp, "d.sock")
	mcpSock := filepath.Join(tmp, "d.mcp.sock")
	logPath := filepath.Join(tmp, "daemon.log")
	logF, _ := os.Create(logPath)
	defer logF.Close()

	srv := exec.Command(bin, "serve", "--socket", sock, "--mcp-socket", mcpSock)
	srv.Env = append(os.Environ(),
		"XEROTTY_UPGRADE_BINARY="+bin, // upgrade to ourselves
		"XDG_CACHE_HOME="+tmp,         // keep recordings out of the real cache
		"SHELL=/bin/sh",
	)
	srv.Stdout = logF
	srv.Stderr = logF
	if err := srv.Start(); err != nil {
		t.Skipf("start daemon: %v", err)
	}
	daemonPID := srv.Process.Pid
	defer func() {
		if t.Failed() {
			// SIGQUIT first: the Go runtime dumps all goroutine
			// stacks to stderr (our log file) — names the wedge.
			_ = srv.Process.Signal(syscall.SIGQUIT)
			time.Sleep(500 * time.Millisecond)
		}
		_ = srv.Process.Kill()
		_, _ = srv.Process.Wait()
		if t.Failed() {
			if b, err := os.ReadFile(logPath); err == nil {
				t.Logf("daemon log:\n%s", b)
			}
		}
	}()

	// Wait for the MCP socket, then bootstrap a session + tab. The
	// daemon only materializes "default" on a wire attach, so create
	// the session the way a GUI would: a wire client attach.
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
		t.Fatal("daemon socket never came up")
	}
	wcDone, instanceA := attachWireClient(t, sock)
	defer wcDone()

	if !waitSock(mcpSock) {
		t.Fatal("mcp socket never came up")
	}
	probe, err := dialProbe(mcpSock)
	if err != nil {
		t.Fatalf("dial mcp: %v", err)
	}
	if _, err := probe.call("agent/mode", map[string]any{"mode": "auto"}); err != nil {
		t.Fatalf("mode: %v", err)
	}

	// Find the tab the attach created.
	tabsRes, err := probe.call("tabs/list", nil)
	if err != nil {
		t.Fatalf("tabs/list: %v", err)
	}
	var tabs []struct {
		ID uint32 `json:"id"`
	}
	if err := json.Unmarshal(tabsRes, &tabs); err != nil || len(tabs) == 0 {
		t.Fatalf("no tabs: %v %s", err, tabsRes)
	}
	tabID := tabs[0].ID

	// Capture the shell's pid on screen.
	if _, err := probe.call("tab/input", map[string]any{"tab_id": tabID, "bytes": "echo PIDIS_$$\r"}); err != nil {
		t.Fatalf("input: %v", err)
	}
	pidRe := regexp.MustCompile(`PIDIS_(\d+)`)
	var shellPID string
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && shellPID == "" {
		scr, err := probe.screen(tabID)
		if err == nil {
			if m := pidRe.FindStringSubmatch(scr); m != nil {
				shellPID = m[1]
			}
		}
		if shellPID == "" {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if shellPID == "" {
		t.Fatal("shell pid never echoed")
	}

	// ---- THE UPGRADE ----
	// Through the real CLI: peer-pid discovery via SO_PEERCRED, the
	// SIGUSR2 trigger, the pre-exec validation gate, the exec.
	up := exec.Command(bin, "serve", "--upgrade", "--socket", sock)
	up.Env = srv.Env
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("serve --upgrade: %v\n%s", err, out)
	}

	// Wait for the OLD listener to actually go down first —
	// otherwise the dial below can win the race against the signal
	// handler, bind to the old image, and ride it into the exec.
	downBy := time.Now().Add(5 * time.Second)
	for time.Now().Before(downBy) {
		c, err := net.Dial("unix", mcpSock)
		if err != nil {
			break // old mcp gone
		}
		c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	// Now retry full dial+rpc until the NEW image answers.
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
		t.Fatal("new image's MCP never answered after upgrade")
	}
	// The daemon PID must NOT change (exec-in-place).
	if err := syscall.Kill(daemonPID, 0); err != nil {
		t.Fatalf("daemon process vanished across upgrade: %v", err)
	}

	// Same tab ID, restored screen (the pre-upgrade pid line), and —
	// the whole point — the SAME shell answering new input.
	scr, err := probe2.screen(tabID)
	if err != nil {
		t.Fatalf("screen post-upgrade (tab %d should survive): %v", tabID, err)
	}
	if !strings.Contains(scr, "PIDIS_"+shellPID) {
		t.Fatalf("restored screen lost pre-upgrade contents:\n%s", scr)
	}
	if _, err := probe2.call("tab/input", map[string]any{"tab_id": tabID, "bytes": "echo AGAIN_$$\r"}); err != nil {
		t.Fatalf("post-upgrade input: %v", err)
	}

	// InstanceID continuity: a fresh wire attach must see the SAME
	// instance — a hot upgrade is the same logical daemon, so
	// clients keep their tombstones instead of dropping them like
	// they would for a genuine restart.
	wcDone2, instanceB := attachWireClient(t, sock)
	defer wcDone2()
	if instanceA == "" || instanceA != instanceB {
		t.Fatalf("InstanceID changed across hot upgrade: %q -> %q", instanceA, instanceB)
	}
	want := "AGAIN_" + shellPID
	deadline = time.Now().Add(8 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		scr, err := probe2.screen(tabID)
		if err == nil && strings.Contains(scr, want) {
			return // SAME shell pid answered after the exec. Victory.
		}
		if err != nil {
			// Connection-level error: re-dial (the daemon is fine;
			// the probe's conn may have raced the quiesce).
			if p := freshProbe(); p != nil {
				probe2 = p
			}
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	scr, ferr := probe2.screen(tabID)
	t.Fatalf("shell did not answer with the same pid after upgrade (want %s, lastErr %v, finalErr %v):\n%q", want, lastErr, ferr, scr)
}

// attachWireClient creates the "default" session + initial tab the
// way a GUI would (the daemon only materializes a session on wire
// attach). Keeps draining frames until the daemon hangs up on it at
// upgrade time — which is expected and fine.
func attachWireClient(t *testing.T, sock string) (cleanup func(), instanceID string) {
	t.Helper()
	cli, err := clientproto.Dial(sock)
	if err != nil {
		t.Fatalf("wire dial: %v", err)
	}
	if _, err := cli.Hello("upgrade-e2e"); err != nil {
		t.Fatalf("wire hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", true); err != nil {
		t.Fatalf("wire attach: %v", err)
	}
	select {
	case att := <-cli.Attached():
		instanceID = att.InstanceID
	case <-time.After(5 * time.Second):
		t.Fatal("never attached")
	}
	go func() {
		for {
			select {
			case <-cli.Closed():
				return
			case <-cli.CellFull():
			case <-cli.CellDiff():
			case <-cli.Cursor():
			case <-cli.TabState():
			case <-cli.ScrollbackAppend():
			case <-cli.Topology():
			case <-cli.TabCreated():
			case <-cli.Title():
			case <-cli.Errors():
			}
		}
	}()
	return func() { cli.Close() }, instanceID
}
