package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/testutil"
)

// fakeMCP is a scripted MCP socket. Each wait call (wait_for_events
// or events/wait) gets the next reply from replies; once they run out
// the call blocks until the connection closes, like an idle server.
// dropAfter > 0 closes each connection after that many wait calls.
type fakeMCP struct {
	t         *testing.T
	path      string
	name      string // serverInfo name
	dropAfter int

	mu      sync.Mutex
	replies []map[string]any
	params  []map[string]any // params of each wait call, in order
	dials   int
}

func newFakeMCP(t *testing.T, name string, replies ...map[string]any) *fakeMCP {
	f := &fakeMCP{t: t, name: name, replies: replies, path: filepath.Join(testutil.SockDir(t), "m.sock")}
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.dials++
			f.mu.Unlock()
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeMCP) serve(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	waits := 0
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"serverInfo": map[string]any{"name": f.name}}
		case "wait_for_events", "events/wait":
			f.mu.Lock()
			f.params = append(f.params, req.Params)
			var r map[string]any
			if len(f.replies) > 0 {
				r, f.replies = f.replies[0], f.replies[1:]
			}
			f.mu.Unlock()
			if r == nil {
				// Idle: hold the call until the client hangs up.
				_, _ = c.Read(make([]byte, 1))
				return
			}
			if msg, ok := r["rpc_error"]; ok {
				b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": msg}})
				_, _ = c.Write(append(b, '\n'))
				continue
			}
			result = r
			waits++
		default:
			f.t.Errorf("unexpected method %q", req.Method)
			return
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		_, _ = c.Write(append(b, '\n'))
		if f.dropAfter > 0 && waits >= f.dropAfter {
			return
		}
	}
}

func (f *fakeMCP) waitParams() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.params...)
}

// syncBuffer is a bytes.Buffer safe to read while runEvents writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) lines() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			panic("not a JSON line: " + l)
		}
		out = append(out, m)
	}
	return out
}

// runFor runs runEvents against f until it has printed want lines
// (follow mode) or returned, then stops it.
func runFor(t *testing.T, f *fakeMCP, o eventsOpts, want int) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- runEvents(ctx, o, func() (*eventsConn, error) { return dialEvents(f.path) }, &out, &errOut)
	}()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("runEvents: %v (stderr %s)", err, errOut.b.String())
			}
			return out.lines()
		case <-deadline:
			t.Fatalf("timed out; got %d lines: %v", len(out.lines()), out.lines())
		case <-time.After(10 * time.Millisecond):
		}
		if o.follow && len(out.lines()) >= want {
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("runEvents after cancel: %v", err)
			}
			return out.lines()
		}
	}
}

func ev(host string, seq int, kind string, tab int) map[string]any {
	return map[string]any{"host": host, "tab_id": host + ":" + strconv.Itoa(tab), "seq": seq, "time": "2026-10-22T02:00:00Z", "kind": kind, "tab": tab}
}

func TestEventsGUIOnce(t *testing.T) {
	f := newFakeMCP(t, "xerotty-gui", map[string]any{
		"events": []any{ev("kh", 4, "bell", 2), ev("local", 7, "tab_created", 1)},
		"cursor": map[string]any{"kh": map[string]any{"epoch": "e1", "seq": 4}, "local": map[string]any{"epoch": "e2", "seq": 7}},
		"errors": map[string]any{"xp": "host not connected"},
	})
	lines := runFor(t, f, eventsOpts{hosts: []string{"kh", "local", "xp"}, kinds: []string{"bell", "tab_created"}, tabs: []string{"kh:2"}}, 0)
	if len(lines) != 3 {
		t.Fatalf("lines = %v", lines)
	}
	if lines[0]["kind"] != "bell" || lines[0]["tab_id"] != "kh:2" || lines[1]["kind"] != "tab_created" {
		t.Errorf("events = %v", lines[:2])
	}
	if lines[2]["kind"] != "host_error" || lines[2]["host"] != "xp" {
		t.Errorf("error line = %v", lines[2])
	}
	p := f.waitParams()
	if len(p) != 1 || p[0]["timeout_ms"] != float64(0) || p[0]["cursor"] != nil {
		t.Fatalf("one-shot must read without blocking and without a cursor: %v", p)
	}
	if !equalStrs(p[0]["hosts"], "kh", "local", "xp") || !equalStrs(p[0]["kinds"], "bell", "tab_created") || !equalStrs(p[0]["tab_ids"], "kh:2") {
		t.Errorf("filters not passed through: %v", p[0])
	}
}

func equalStrs(v any, want ...string) bool {
	arr, _ := v.([]any)
	if len(arr) != len(want) {
		return false
	}
	for i := range arr {
		if arr[i] != want[i] {
			return false
		}
	}
	return true
}

// --follow passes each reply's cursor back, prints a missed line for
// a host whose daemon restarted, and prints a host error only when it
// changes.
func TestEventsGUIFollow(t *testing.T) {
	c1 := map[string]any{"kh": map[string]any{"epoch": "e1", "seq": 4}}
	c2 := map[string]any{"kh": map[string]any{"epoch": "e9", "seq": 1}, "local": map[string]any{"epoch": "e2", "seq": 8}}
	f := newFakeMCP(t, "xerotty-gui",
		map[string]any{"events": []any{ev("kh", 4, "bell", 2)}, "cursor": c1, "errors": map[string]any{"xp": "down"}},
		map[string]any{"events": []any{}, "cursor": c1, "errors": map[string]any{"xp": "down"}},
		map[string]any{"events": []any{ev("kh", 1, "tab_created", 1), ev("local", 8, "title_changed", 3)}, "cursor": c2, "missed": map[string]any{"kh": "restarted"}},
	)
	lines := runFor(t, f, eventsOpts{follow: true}, 5)
	kinds := []string{}
	for _, l := range lines {
		kinds = append(kinds, l["kind"].(string))
	}
	want := []string{"bell", "host_error", "events_missed", "tab_created", "title_changed"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if lines[2]["host"] != "kh" || lines[2]["reason"] != "restarted" {
		t.Errorf("missed line = %v", lines[2])
	}
	p := f.waitParams()
	if len(p) < 4 {
		t.Fatalf("wait calls = %d", len(p))
	}
	if p[0]["timeout_ms"] != float64(0) || p[1]["timeout_ms"] != float64(followWait.Milliseconds()) {
		t.Errorf("first read must not block, later ones must: %v %v", p[0]["timeout_ms"], p[1]["timeout_ms"])
	}
	if got, _ := json.Marshal(p[1]["cursor"]); string(got) != `{"kh":{"epoch":"e1","seq":4}}` {
		t.Errorf("cursor not passed back: %s", got)
	}
	if got, _ := json.Marshal(p[3]["cursor"]); !strings.Contains(string(got), `"kh":{"epoch":"e9","seq":1}`) {
		t.Errorf("cursor after restart: %s", got)
	}
}

// --new drops what the first read returns but keeps its cursor.
func TestEventsFollowNew(t *testing.T) {
	c1 := map[string]any{"local": map[string]any{"epoch": "e", "seq": 50}}
	f := newFakeMCP(t, "xerotty-gui",
		map[string]any{"events": []any{ev("local", 49, "bell", 1), ev("local", 50, "bell", 1)}, "cursor": c1, "missed": map[string]any{"local": "overflow"}},
		map[string]any{"events": []any{ev("local", 51, "child_exited", 1)}, "cursor": map[string]any{"local": map[string]any{"epoch": "e", "seq": 51}}},
	)
	lines := runFor(t, f, eventsOpts{follow: true, fromNow: true}, 1)
	if len(lines) != 1 || lines[0]["kind"] != "child_exited" {
		t.Fatalf("lines = %v", lines)
	}
	if got, _ := json.Marshal(f.waitParams()[1]["cursor"]); string(got) != `{"local":{"epoch":"e","seq":50}}` {
		t.Errorf("cursor = %s", got)
	}
}

// A dropped connection is redialed with the cursor kept.
func TestEventsFollowRedialKeepsCursor(t *testing.T) {
	f := newFakeMCP(t, "xerotty-gui",
		map[string]any{"events": []any{ev("local", 3, "bell", 1)}, "cursor": map[string]any{"local": map[string]any{"epoch": "e", "seq": 3}}},
		map[string]any{"events": []any{ev("local", 4, "bell", 1)}, "cursor": map[string]any{"local": map[string]any{"epoch": "e", "seq": 4}}},
	)
	f.dropAfter = 1
	lines := runFor(t, f, eventsOpts{follow: true}, 2)
	if len(lines) != 2 || lines[1]["seq"] != float64(4) {
		t.Fatalf("lines = %v", lines)
	}
	f.mu.Lock()
	dials := f.dials
	f.mu.Unlock()
	if dials < 2 {
		t.Errorf("dials = %d, want a redial", dials)
	}
	if got, _ := json.Marshal(f.waitParams()[1]["cursor"]); string(got) != `{"local":{"epoch":"e","seq":3}}` {
		t.Errorf("cursor after redial = %s", got)
	}
}

// A request the server rejects (bad kind) ends the stream instead of
// retrying it forever.
func TestEventsRPCErrorStops(t *testing.T) {
	f := newFakeMCP(t, "xerotty-gui", map[string]any{"rpc_error": "unknown kind"})
	err := runEvents(context.Background(), eventsOpts{follow: true}, func() (*eventsConn, error) { return dialEvents(f.path) }, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("err = %v", err)
	}
}

// On a single daemon's socket, events are reported as host "local"
// with namespaced tab ids, the cursor maps to after_seq + epoch,
// local tab filters become plain ids, and other hosts are errors.
func TestEventsDaemonSocket(t *testing.T) {
	f := newFakeMCP(t, "xerotty",
		map[string]any{"events": []any{map[string]any{"seq": 5, "kind": "bell", "tab": 3, "time": "2026-10-22T02:00:00Z"}}, "last_seq": 5, "epoch": "d1", "missed": false},
		map[string]any{"events": []any{map[string]any{"seq": 1, "kind": "tab_created", "tab": 1, "time": "2026-10-22T02:00:01Z"}}, "last_seq": 1, "epoch": "d2", "missed": true, "missed_reason": "restarted"},
	)
	lines := runFor(t, f, eventsOpts{follow: true, hosts: []string{"local", "kh"}, tabs: []string{"local:3", "kh:1"}, kinds: []string{"bell"}}, 4)
	if len(lines) != 4 {
		t.Fatalf("lines = %v", lines)
	}
	if lines[0]["host"] != "local" || lines[0]["tab_id"] != "local:3" || lines[0]["kind"] != "bell" {
		t.Errorf("event = %v", lines[0])
	}
	if lines[1]["kind"] != "host_error" || lines[1]["host"] != "kh" {
		t.Errorf("host error = %v", lines[1])
	}
	if lines[2]["kind"] != "events_missed" || lines[2]["reason"] != "restarted" || lines[3]["tab_id"] != "local:1" {
		t.Errorf("after restart = %v", lines[2:])
	}
	p := f.waitParams()
	if p[0]["after_seq"] != float64(0) || p[0]["epoch"] != "" || !equalNums(p[0]["tab_ids"], 3) || !equalStrs(p[0]["kinds"], "bell") {
		t.Errorf("first call = %v", p[0])
	}
	if p[1]["after_seq"] != float64(5) || p[1]["epoch"] != "d1" {
		t.Errorf("second call cursor = %v", p[1])
	}
}

func equalNums(v any, want ...float64) bool {
	arr, _ := v.([]any)
	if len(arr) != len(want) {
		return false
	}
	for i := range arr {
		if arr[i] != want[i] {
			return false
		}
	}
	return true
}

func TestEventsFlagParsing(t *testing.T) {
	var l listFlag
	_ = l.Set("bell, notification")
	_ = l.Set("tab_created")
	if strings.Join(l, "|") != "bell|notification|tab_created" {
		t.Errorf("listFlag = %v", l)
	}
	for in, want := range map[string]string{"3": "local:3", "kh:2": "kh:2", "u@h:2222:4": "u@h:2222:4"} {
		if got, err := normalizeTabID(in); err != nil || got != want {
			t.Errorf("normalizeTabID(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "kh", ":3", "kh:x"} {
		if _, err := normalizeTabID(bad); err == nil {
			t.Errorf("normalizeTabID(%q) accepted", bad)
		}
	}
}

// End to end against a real daemon: `xerotty events --follow --new`
// prints a line when a tab is created and renamed.
func TestEventsCLIRealDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "xerotty")
	if out, err := exec.Command("go", "build", "-tags", "headless", "-o", bin, "./../../cmd/xerotty").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	sd := testutil.SockDir(t)
	sock := filepath.Join(sd, "d.sock")
	mcpSock := filepath.Join(sd, "d.mcp.sock")
	srv := exec.Command(bin, "serve", "--socket", sock, "--mcp-socket", mcpSock)
	srv.Env = append(os.Environ(), "SHELL=/bin/sh")
	srv.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := srv.Start(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer func() { _ = syscall.Kill(-srv.Process.Pid, syscall.SIGKILL); _, _ = srv.Process.Wait() }()

	var cli *clientproto.Client
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var err error
		if cli, err = clientproto.Dial(sock); err == nil {
			break
		}
	}
	if cli == nil {
		t.Fatal("wire dial never succeeded")
	}
	defer cli.Close()
	if _, err := cli.Hello("events-e2e"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	<-cli.Attached()

	ev := exec.Command(bin, "events", "--socket", mcpSock, "--follow", "--new", "--kind", "tab_created,tab_renamed")
	stdout, _ := ev.StdoutPipe()
	var stderr bytes.Buffer
	ev.Stderr = &stderr
	if err := ev.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ev.Process.Kill(); _ = ev.Wait() }()
	lines := make(chan map[string]any, 8)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("not JSON: %q", sc.Text())
				continue
			}
			lines <- m
		}
		close(lines)
	}()
	// Give the stream time to take its starting cursor.
	time.Sleep(300 * time.Millisecond)

	c := dialRPC(t, mcpSock)
	defer c.conn.Close()
	c.call(t, "initialize", map[string]any{"protocolVersion": "2024-11-05", "clientInfo": map[string]any{"name": "events-e2e"}})
	c.tool(t, "set_agent_mode", map[string]any{"mode": "auto"})
	var created struct {
		TabID uint32 `json:"tab_id"`
	}
	if err := json.Unmarshal([]byte(c.tool(t, "create_tab", map[string]any{})), &created); err != nil {
		t.Fatal(err)
	}
	c.tool(t, "rename_tab", map[string]any{"tab_id": created.TabID, "name": "build"})

	wantTab := "local:" + strconv.Itoa(int(created.TabID))
	var got []string
	timeout := time.After(10 * time.Second)
	for len(got) < 2 {
		select {
		case m, ok := <-lines:
			if !ok {
				t.Fatalf("stream ended; got %v; stderr %s", got, stderr.String())
			}
			if m["tab_id"] != wantTab {
				t.Fatalf("unexpected line %v (want tab %s)", m, wantTab)
			}
			got = append(got, m["kind"].(string))
			if m["kind"] == "tab_renamed" && m["new_name"] != "build" {
				t.Errorf("rename line = %v", m)
			}
		case <-timeout:
			t.Fatalf("timed out; got %v; stderr %s", got, stderr.String())
		}
	}
	if strings.Join(got, ",") != "tab_created,tab_renamed" {
		t.Errorf("kinds = %v", got)
	}
}
