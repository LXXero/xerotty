package mcp_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/mcp"
	"github.com/LXXero/xerotty/internal/tabevents"
	"github.com/LXXero/xerotty/internal/testutil"
)

type waitReply struct {
	Events []struct {
		Seq      uint64 `json:"seq"`
		Kind     string `json:"kind"`
		Tab      uint32 `json:"tab"`
		NewState string `json:"new_state"`
	} `json:"events"`
	LastSeq      uint64 `json:"last_seq"`
	Epoch        string `json:"epoch"`
	Missed       bool   `json:"missed"`
	MissedReason string `json:"missed_reason"`
}

func startEventsDaemon(t *testing.T) (*daemon.Daemon, *mcpClient) {
	t.Helper()
	dir := testutil.SockDir(t)
	cfg := config.Default()
	d := daemon.New(&cfg, filepath.Join(dir, "xerottyd.sock"))
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	t.Cleanup(func() { _ = d.Stop(); <-doneRun })
	srv := mcp.New(d, filepath.Join(dir, "xerottyd.mcp.sock"))
	doneMCP := make(chan error, 1)
	go func() { doneMCP <- srv.Run() }()
	t.Cleanup(func() { _ = srv.Stop(); <-doneMCP })
	time.Sleep(80 * time.Millisecond)
	c := dialMCP(t, srv.SocketPath())
	t.Cleanup(func() { c.Close() })
	return d, c
}

func waitTool(t *testing.T, c *mcpClient, id int, args map[string]any) waitReply {
	t.Helper()
	res := c.call(t, id, "tools/call", map[string]any{"name": "wait_for_events", "arguments": args})
	var env struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(res, &env); err != nil || len(env.Content) != 1 {
		t.Fatalf("tools/call envelope %s: %v", res, err)
	}
	var r waitReply
	if err := json.Unmarshal([]byte(env.Content[0].Text), &r); err != nil {
		t.Fatalf("decode reply %s: %v", env.Content[0].Text, err)
	}
	return r
}

// wait_for_events is a read: it works in the default observe mode,
// honours after_seq, the kinds and tab filters, blocks until an event
// arrives, times out empty, and reports a cursor from another daemon
// process as missed/restarted.
func TestWaitForEvents(t *testing.T) {
	d, c := startEventsDaemon(t)
	ring := d.Events()

	r0 := waitTool(t, c, 1, map[string]any{"timeout_ms": 0})
	if r0.Epoch != ring.Epoch() || r0.Missed {
		t.Fatalf("first read = %+v (ring epoch %s)", r0, ring.Epoch())
	}

	ring.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 5})
	ring.Append(tabevents.Event{Kind: tabevents.TitleChanged, Tab: 6, NewState: "idle"})
	r1 := waitTool(t, c, 2, map[string]any{"after_seq": r0.LastSeq, "epoch": r0.Epoch, "kinds": []string{"bell"}, "timeout_ms": 0})
	if len(r1.Events) != 1 || r1.Events[0].Kind != "bell" || r1.Events[0].Tab != 5 {
		t.Fatalf("kinds filter: %+v", r1)
	}
	if r1.LastSeq != r0.LastSeq+2 {
		t.Errorf("last_seq = %d, want %d (advances past filtered-out events)", r1.LastSeq, r0.LastSeq+2)
	}

	// Blocks until an event arrives.
	go func() {
		time.Sleep(100 * time.Millisecond)
		ring.Append(tabevents.Event{Kind: tabevents.Notification, Tab: 7})
	}()
	start := time.Now()
	r2 := waitTool(t, c, 3, map[string]any{"after_seq": r1.LastSeq, "epoch": r1.Epoch, "timeout_ms": 5000})
	if el := time.Since(start); el < 80*time.Millisecond || el > 3*time.Second {
		t.Errorf("blocking wait returned after %v", el)
	}
	if len(r2.Events) != 1 || r2.Events[0].Kind != "notification" {
		t.Fatalf("blocking wait: %+v", r2)
	}

	// Tab filter.
	r3 := waitTool(t, c, 4, map[string]any{"after_seq": r0.LastSeq, "epoch": r0.Epoch, "tab_ids": []int{6}, "timeout_ms": 0})
	if len(r3.Events) != 1 || r3.Events[0].Tab != 6 || r3.Events[0].NewState != "idle" {
		t.Fatalf("tab filter: %+v", r3)
	}

	// Times out empty.
	start = time.Now()
	r4 := waitTool(t, c, 5, map[string]any{"after_seq": r2.LastSeq, "epoch": r2.Epoch, "timeout_ms": 200})
	if el := time.Since(start); el < 200*time.Millisecond {
		t.Errorf("timeout wait returned after %v", el)
	}
	if len(r4.Events) != 0 || r4.Missed || r4.LastSeq != r2.LastSeq {
		t.Fatalf("timed-out wait: %+v", r4)
	}

	// A cursor from another daemon process: missed, at once.
	start = time.Now()
	r5 := waitTool(t, c, 6, map[string]any{"after_seq": 1, "epoch": "earlier-daemon", "timeout_ms": 5000})
	if time.Since(start) > 2*time.Second {
		t.Errorf("restart read blocked")
	}
	if !r5.Missed || r5.MissedReason != "restarted" || len(r5.Events) < 3 {
		t.Fatalf("restart read: %+v", r5)
	}

	if e := c.callExpectErr(t, 7, "events/wait", map[string]any{"kinds": []string{"bel"}}); e.Code != -32602 {
		t.Errorf("unknown kind: code %d (%s)", e.Code, e.Message)
	}
}

// A blocked wait must not hold up other calls on the same connection:
// MCP clients send tool calls concurrently over one bridge.
func TestWaitForEventsDoesNotBlockConnection(t *testing.T) {
	d, c := startEventsDaemon(t)
	last := d.Events().Since(0, tabevents.Filter{}).Last
	fmt.Fprintf(c.conn, `{"jsonrpc":"2.0","id":50,"method":"events/wait","params":{"after_seq":%d,"timeout_ms":5000}}`+"\n", last)
	fmt.Fprintln(c.conn, `{"jsonrpc":"2.0","id":51,"method":"server/info"}`)

	readID := func() int {
		_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		line, err := c.br.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read reply: %v", err)
		}
		var resp struct {
			ID int `json:"id"`
		}
		_ = json.Unmarshal(line, &resp)
		return resp.ID
	}
	if id := readID(); id != 51 {
		t.Fatalf("first reply id %d, want 51 (server/info answered while the wait blocks)", id)
	}
	d.Events().Append(tabevents.Event{Kind: tabevents.Bell, Tab: 1})
	if id := readID(); id != 50 {
		t.Fatalf("second reply id %d, want 50", id)
	}
}
