package guimcp_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/guimcp"
	"github.com/LXXero/xerotty/internal/tabevents"
)

type aggReply struct {
	Events []struct {
		Host  string `json:"host"`
		TabID string `json:"tab_id"`
		Seq   uint64 `json:"seq"`
		Kind  string `json:"kind"`
	} `json:"events"`
	Cursor guimcp.Cursor     `json:"cursor"`
	Missed map[string]string `json:"missed"`
	Errors map[string]string `json:"errors"`
}

func aggWait(t *testing.T, c *mcpConn, id int, args map[string]any) aggReply {
	t.Helper()
	res, rerr := c.call(t, id, "tools/call", map[string]any{"name": "wait_for_events", "arguments": args})
	if rerr != nil {
		t.Fatalf("wait_for_events: %+v", rerr)
	}
	var env struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(res, &env); err != nil || len(env.Content) != 1 {
		t.Fatalf("envelope %s: %v", res, err)
	}
	var r aggReply
	if err := json.Unmarshal([]byte(env.Content[0].Text), &r); err != nil {
		t.Fatalf("decode %s: %v", env.Content[0].Text, err)
	}
	return r
}

func twoHosts() (*fakeBackend, *tabevents.Ring, *tabevents.Ring) {
	local, kh := tabevents.New(8), tabevents.New(8)
	return &fakeBackend{rings: map[string]*tabevents.Ring{"local": local, "kh": kh}, deaf: map[string]bool{}}, local, kh
}

// The aggregator merges two hosts' events under namespaced tab ids,
// keeps one cursor entry per host, and blocks until any host has
// something.
func TestGUIMCPWaitForEventsMergesHosts(t *testing.T) {
	b, local, kh := twoHosts()
	local.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 1})
	kh.Append(tabevents.Event{Kind: tabevents.TitleChanged, Tab: 3})
	c, closeC := dial(t, startServer(t, b))
	defer closeC()

	r := aggWait(t, c, 1, map[string]any{"timeout_ms": 0})
	if len(r.Events) != 2 || r.Events[0].TabID != "local:1" || r.Events[1].TabID != "kh:3" || r.Events[1].Host != "kh" {
		t.Fatalf("first read events = %+v", r.Events)
	}
	if r.Cursor["local"] != (guimcp.HostCursor{Epoch: local.Epoch(), Seq: 1}) || r.Cursor["kh"] != (guimcp.HostCursor{Epoch: kh.Epoch(), Seq: 1}) {
		t.Fatalf("cursor = %+v", r.Cursor)
	}
	if len(r.Missed) != 0 || len(r.Errors) != 0 {
		t.Fatalf("unexpected missed/errors: %+v", r)
	}

	// Block until kh has something; local's cursor stays put.
	go func() {
		time.Sleep(100 * time.Millisecond)
		kh.Append(tabevents.Event{Kind: tabevents.Notification, Tab: 3})
	}()
	start := time.Now()
	r2 := aggWait(t, c, 2, map[string]any{"cursor": r.Cursor, "timeout_ms": 5000})
	if el := time.Since(start); el < 80*time.Millisecond || el > 3*time.Second {
		t.Errorf("blocking wait returned after %v", el)
	}
	if len(r2.Events) != 1 || r2.Events[0].TabID != "kh:3" || r2.Events[0].Kind != "notification" {
		t.Fatalf("blocking wait events = %+v", r2.Events)
	}
	if r2.Cursor["kh"].Seq != 2 || r2.Cursor["local"] != r.Cursor["local"] {
		t.Fatalf("cursor after wait = %+v", r2.Cursor)
	}

	// Nothing new: times out empty with the cursor unchanged.
	start = time.Now()
	r3 := aggWait(t, c, 3, map[string]any{"cursor": r2.Cursor, "timeout_ms": 200})
	if el := time.Since(start); el < 200*time.Millisecond {
		t.Errorf("timeout wait returned after %v", el)
	}
	if len(r3.Events) != 0 || r3.Cursor["kh"] != r2.Cursor["kh"] || r3.Cursor["local"] != r2.Cursor["local"] {
		t.Fatalf("timed-out wait = %+v", r3)
	}
}

// When one host wakes the wait, the reply also carries what the other
// hosts have at that moment, and nothing a cancelled host had is lost.
func TestGUIMCPWaitForEventsReadsOtherHostsOnWake(t *testing.T) {
	b, local, kh := twoHosts()
	c, closeC := dial(t, startServer(t, b))
	defer closeC()
	cur := aggWait(t, c, 1, map[string]any{"timeout_ms": 0}).Cursor

	// local never wakes a blocking wait, so its event can only arrive
	// through the read that follows kh waking.
	b.evMu.Lock()
	b.deaf["local"] = true
	b.evMu.Unlock()
	go func() {
		time.Sleep(50 * time.Millisecond)
		local.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 1})
		time.Sleep(50 * time.Millisecond)
		kh.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 3})
	}()
	r := aggWait(t, c, 2, map[string]any{"cursor": cur, "timeout_ms": 5000})
	if len(r.Events) != 2 || r.Events[0].TabID != "local:1" || r.Events[1].TabID != "kh:3" {
		t.Fatalf("events = %+v, want local:1 then kh:3", r.Events)
	}
	if r.Cursor["local"].Seq != 1 || r.Cursor["kh"].Seq != 1 {
		t.Fatalf("cursor = %+v", r.Cursor)
	}
}

// A daemon that restarts starts a new ring: same host, new epoch, seq
// from 1. The aggregator must report the gap instead of trusting the
// old seq, return what the new daemon has, and move the cursor to it.
func TestGUIMCPWaitForEventsSequenceReset(t *testing.T) {
	b, local, kh := twoHosts()
	for i := 0; i < 5; i++ {
		kh.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 3})
	}
	local.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 1})
	c, closeC := dial(t, startServer(t, b))
	defer closeC()
	cur := aggWait(t, c, 1, map[string]any{"timeout_ms": 0}).Cursor
	if cur["kh"].Seq != 5 {
		t.Fatalf("kh cursor = %+v", cur["kh"])
	}

	// kh restarts; its new daemon has recorded 2 events so far, so
	// its seq (2) is below the old cursor (5) — and would be above it
	// after a few more, which the epoch still catches.
	restarted := tabevents.New(8)
	restarted.Append(tabevents.Event{Kind: tabevents.TabCreated, Tab: 1})
	restarted.Append(tabevents.Event{Kind: tabevents.TabCreated, Tab: 2})
	b.evMu.Lock()
	b.rings["kh"] = restarted
	b.evMu.Unlock()

	start := time.Now()
	r := aggWait(t, c, 2, map[string]any{"cursor": cur, "timeout_ms": 5000})
	if time.Since(start) > 2*time.Second {
		t.Errorf("a gap must be reported at once, not after the timeout")
	}
	if r.Missed["kh"] != tabevents.GapRestarted || r.Missed["local"] != "" {
		t.Fatalf("missed = %+v", r.Missed)
	}
	if len(r.Events) != 2 || r.Events[0].TabID != "kh:1" || r.Events[1].TabID != "kh:2" {
		t.Fatalf("events = %+v", r.Events)
	}
	if r.Cursor["kh"] != (guimcp.HostCursor{Epoch: restarted.Epoch(), Seq: 2}) || r.Cursor["local"] != cur["local"] {
		t.Fatalf("cursor = %+v", r.Cursor)
	}

	// The new cursor is clean on the next call.
	r2 := aggWait(t, c, 3, map[string]any{"cursor": r.Cursor, "timeout_ms": 0})
	if len(r2.Missed) != 0 || len(r2.Events) != 0 {
		t.Fatalf("after reset = %+v", r2)
	}

	// Many restarts later the new daemon's seq can pass the old
	// cursor; the epoch still marks it as a different log.
	again := tabevents.New(8)
	for i := 0; i < 7; i++ {
		again.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 9})
	}
	b.evMu.Lock()
	b.rings["kh"] = again
	b.evMu.Unlock()
	r3 := aggWait(t, c, 4, map[string]any{"cursor": r.Cursor, "timeout_ms": 0})
	if r3.Missed["kh"] != tabevents.GapRestarted || len(r3.Events) != 7 {
		t.Fatalf("seq past old cursor after restart = %+v", r3)
	}
}

func TestGUIMCPWaitForEventsOverflow(t *testing.T) {
	b, local, _ := twoHosts()
	local.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 1})
	c, closeC := dial(t, startServer(t, b))
	defer closeC()
	cur := aggWait(t, c, 1, map[string]any{"timeout_ms": 0}).Cursor
	for i := 0; i < 20; i++ { // ring holds 8
		local.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 1})
	}
	r := aggWait(t, c, 2, map[string]any{"cursor": cur, "timeout_ms": 0})
	if r.Missed["local"] != tabevents.GapOverflow || len(r.Events) != 8 || r.Cursor["local"].Seq != 21 {
		t.Fatalf("overflow = missed %+v, %d events, cursor %+v", r.Missed, len(r.Events), r.Cursor)
	}
}

// Filters: tab_ids limits the hosts read (others keep their cursor),
// kinds filters events, hosts limits hosts, and cursor entries for
// hosts not connected right now are carried forward.
func TestGUIMCPWaitForEventsFilters(t *testing.T) {
	b, local, kh := twoHosts()
	local.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 1})
	kh.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 3})
	kh.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 4})
	kh.Append(tabevents.Event{Kind: tabevents.TabClosed, Tab: 3})
	c, closeC := dial(t, startServer(t, b))
	defer closeC()

	gone := guimcp.HostCursor{Epoch: "e-gone", Seq: 9}
	r := aggWait(t, c, 1, map[string]any{"tab_ids": []string{"kh:3"}, "kinds": []string{"bell"}, "timeout_ms": 0,
		"cursor": map[string]any{"gone": gone}})
	if len(r.Events) != 1 || r.Events[0].TabID != "kh:3" || r.Events[0].Kind != "bell" {
		t.Fatalf("filtered events = %+v", r.Events)
	}
	if _, ok := r.Cursor["local"]; ok {
		t.Errorf("local was not asked for but has a cursor entry: %+v", r.Cursor)
	}
	if r.Cursor["gone"] != gone || r.Cursor["kh"].Seq != 3 {
		t.Errorf("cursor = %+v", r.Cursor)
	}

	r = aggWait(t, c, 2, map[string]any{"hosts": []string{"local", "nope"}, "timeout_ms": 0})
	if len(r.Events) != 1 || r.Events[0].Host != "local" || r.Errors["nope"] == "" {
		t.Fatalf("hosts filter = %+v", r)
	}

	if _, rerr := c.call(t, 3, "wait_for_events", map[string]any{"tab_ids": []string{"3"}}); rerr == nil || rerr.Code != -32602 {
		t.Errorf("un-namespaced tab id: %+v", rerr)
	}
	if _, rerr := c.call(t, 4, "wait_for_events", map[string]any{"kinds": []string{"bel"}}); rerr == nil || rerr.Code != -32602 {
		t.Errorf("unknown kind: %+v", rerr)
	}
}

// A blocked wait must not hold up other calls on the same connection.
func TestGUIMCPWaitForEventsDoesNotBlockConnection(t *testing.T) {
	b, local, _ := twoHosts()
	c, closeC := dial(t, startServer(t, b))
	defer closeC()
	cur := aggWait(t, c, 1, map[string]any{"timeout_ms": 0}).Cursor
	curJSON, _ := json.Marshal(cur)
	fmt.Fprintf(c.conn, `{"jsonrpc":"2.0","id":50,"method":"wait_for_events","params":{"cursor":%s,"timeout_ms":5000}}`+"\n", curJSON)
	fmt.Fprintln(c.conn, `{"jsonrpc":"2.0","id":51,"method":"list_tabs"}`)
	readID := func() int {
		_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		line, err := c.br.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var resp struct {
			ID int `json:"id"`
		}
		_ = json.Unmarshal(line, &resp)
		return resp.ID
	}
	if id := readID(); id != 51 {
		t.Fatalf("first reply id %d, want 51", id)
	}
	local.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 1})
	if id := readID(); id != 50 {
		t.Fatalf("second reply id %d, want 50", id)
	}
}
