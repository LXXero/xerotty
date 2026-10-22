package guimcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LXXero/xerotty/internal/tabevents"
)

// HostCursor is one host's position in its daemon's event ring: the
// ring's epoch (one per daemon process) and the last seq read.
type HostCursor struct {
	Epoch string `json:"epoch"`
	Seq   uint64 `json:"seq"`
}

// Cursor is the aggregator's position across every host, keyed by
// host namespace. Each wait_for_events reply returns the updated
// cursor; the caller passes it back unchanged on the next call.
type Cursor map[string]HostCursor

// hostEvent is one event in the merged stream: the daemon's event
// plus the host it came from and the namespaced tab id.
type hostEvent struct {
	Host  string `json:"host"`
	TabID string `json:"tab_id"`
	tabevents.Event
}

// eventsReply is the result of wait_for_events.
type eventsReply struct {
	Events []hostEvent `json:"events"`
	Cursor Cursor      `json:"cursor"`
	// Missed names hosts whose events after the cursor are gone:
	// "restarted" (new daemon process, sequence reset) or "overflow"
	// (the ring wrapped before they were read).
	Missed map[string]string `json:"missed,omitempty"`
	// Errors names hosts that could not be read this call.
	Errors map[string]string `json:"errors,omitempty"`
}

// eventsWait is the state of one wait_for_events call.
type eventsWait struct {
	backend    Backend
	kinds      []tabevents.Kind
	tabsByHost map[string][]uint32
	cursor     Cursor
	events     map[string][]tabevents.Event
	missed     map[string]string
	errs       map[string]string
}

func (w *eventsWait) query(host string, timeout time.Duration) tabevents.Query {
	c := w.cursor[host]
	return tabevents.Query{
		Epoch: c.Epoch, After: c.Seq, Timeout: timeout,
		Filter: tabevents.Filter{Tabs: w.tabsByHost[host], Kinds: w.kinds},
	}
}

// apply records one host's read and advances its cursor.
func (w *eventsWait) apply(host string, res tabevents.Result) {
	c := w.cursor[host]
	if reason := tabevents.GapReason(c.Epoch, c.Seq, res); reason != "" {
		w.missed[host] = reason
	}
	w.events[host] = append(w.events[host], res.Events...)
	w.cursor[host] = HostCursor{Epoch: res.Epoch, Seq: res.Last}
}

func (w *eventsWait) found() bool {
	if len(w.missed) > 0 {
		return true
	}
	for _, evs := range w.events {
		if len(evs) > 0 {
			return true
		}
	}
	return false
}

// readAll reads every host without blocking, in parallel, and
// returns the hosts that answered.
func (w *eventsWait) readAll(ctx context.Context, hosts []string) []string {
	type hostRes struct {
		res tabevents.Result
		err error
	}
	out := make([]hostRes, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			out[i].res, out[i].err = w.backend.WaitEvents(ctx, h, w.query(h, 0))
		}(i, h)
	}
	wg.Wait()
	var ok []string
	for i, h := range hosts {
		if out[i].err != nil {
			w.errs[h] = out[i].err.Error()
			continue
		}
		w.apply(h, out[i].res)
		ok = append(ok, h)
	}
	return ok
}

// waitAny blocks on every host at once until one has a matching
// event or a gap, or timeout passes. Returns the host that woke it
// ("" on timeout). The other hosts' waits are cancelled without
// advancing their cursors, so nothing they had is lost.
func (w *eventsWait) waitAny(ctx context.Context, hosts []string, timeout time.Duration) string {
	// The daemons time out on their own and reply with an advanced
	// cursor; the margin lets those replies (over SSH for remote
	// hosts) win over our deadline.
	wctx, cancel := context.WithTimeout(ctx, timeout+3*time.Second)
	defer cancel()
	type hostRes struct {
		host string
		res  tabevents.Result
		err  error
	}
	ch := make(chan hostRes, len(hosts))
	for _, h := range hosts {
		q := w.query(h, timeout)
		go func(h string) {
			res, err := w.backend.WaitEvents(wctx, h, q)
			ch <- hostRes{h, res, err}
		}(h)
	}
	for range hosts {
		r := <-ch
		if r.err != nil {
			switch {
			case errors.Is(r.err, context.Canceled):
			case errors.Is(r.err, context.DeadlineExceeded):
				w.errs[r.host] = "no reply within timeout_ms (the daemon may predate tab events)"
			default:
				w.errs[r.host] = r.err.Error()
			}
			continue
		}
		w.apply(r.host, r.res)
		if len(r.res.Events) > 0 || r.res.Missed {
			return r.host
		}
	}
	return ""
}

// merged interleaves every host's events by time. Each host's own
// events stay in seq order even if its clock stepped backwards.
func (w *eventsWait) merged() []hostEvent {
	hosts := make([]string, 0, len(w.events))
	n := 0
	for h, evs := range w.events {
		hosts = append(hosts, h)
		n += len(evs)
	}
	sort.Strings(hosts)
	out := make([]hostEvent, 0, n)
	next := make(map[string]int, len(hosts))
	for len(out) < n {
		best := ""
		for _, h := range hosts {
			i := next[h]
			if i >= len(w.events[h]) {
				continue
			}
			if best == "" || w.events[h][i].Time.Before(w.events[best][next[best]].Time) {
				best = h
			}
		}
		e := w.events[best][next[best]]
		next[best]++
		out = append(out, hostEvent{Host: best, TabID: MakeNSID(best, e.Tab), Event: e})
	}
	return out
}

func (w *eventsWait) reply() eventsReply {
	r := eventsReply{Events: w.merged(), Cursor: w.cursor}
	if len(w.missed) > 0 {
		r.Missed = w.missed
	}
	if len(w.errs) > 0 {
		r.Errors = w.errs
	}
	return r
}

// splitNSID parses "<host>:<tabid>", splitting on the last colon
// (a host key may itself contain one, e.g. "user@host:2222").
func splitNSID(nsID string) (string, uint32, bool) {
	i := strings.LastIndex(nsID, ":")
	if i <= 0 {
		return "", 0, false
	}
	id, err := strconv.ParseUint(nsID[i+1:], 10, 32)
	if err != nil {
		return "", 0, false
	}
	return nsID[:i], uint32(id), true
}

// waitForEvents implements wait_for_events: the tab events after the
// caller's cursor across every host, blocking up to timeout_ms for at
// least one.
//
// It first reads every host without blocking. If nothing is there, it
// blocks on all hosts at once; when one wakes it, the others are read
// once more without blocking so the reply carries whatever every host
// has at that moment.
func (s *Server) waitForEvents(ctx context.Context, id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		Cursor    Cursor   `json:"cursor"`
		TimeoutMs *int64   `json:"timeout_ms"`
		TabIDs    []string `json:"tab_ids"`
		Kinds     []string `json:"kinds"`
		Hosts     []string `json:"hosts"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return rpcErrResp(id, -32602, err.Error())
		}
	}
	kinds, err := tabevents.ParseKinds(p.Kinds)
	if err != nil {
		return rpcErrResp(id, -32602, err.Error())
	}
	w := &eventsWait{
		backend:    s.backend,
		kinds:      kinds,
		tabsByHost: map[string][]uint32{},
		cursor:     Cursor{},
		events:     map[string][]tabevents.Event{},
		missed:     map[string]string{},
		errs:       map[string]string{},
	}
	for _, ns := range p.TabIDs {
		host, tab, ok := splitNSID(ns)
		if !ok {
			return rpcErrResp(id, -32602, fmt.Sprintf("tab_id %q is not a namespaced id like \"local:3\"", ns))
		}
		w.tabsByHost[host] = append(w.tabsByHost[host], tab)
	}
	// Hosts not read this call keep their cursor entries.
	for h, c := range p.Cursor {
		w.cursor[h] = c
	}

	connected := map[string]bool{}
	for _, h := range s.backend.EventHosts() {
		connected[h] = true
	}
	want := map[string]bool{}
	switch {
	case len(w.tabsByHost) > 0:
		for h := range w.tabsByHost {
			want[h] = true
		}
		if len(p.Hosts) > 0 {
			only := map[string]bool{}
			for _, h := range p.Hosts {
				only[h] = true
			}
			for h := range want {
				if !only[h] {
					delete(want, h)
				}
			}
		}
	case len(p.Hosts) > 0:
		for _, h := range p.Hosts {
			want[h] = true
		}
	default:
		want = connected
	}
	var hosts []string
	for h := range want {
		if !connected[h] {
			w.errs[h] = "host not connected"
			continue
		}
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	timeout := tabevents.ClampWait(p.TimeoutMs)
	live := w.readAll(ctx, hosts)
	if w.found() || timeout == 0 {
		return okResp(id, w.reply())
	}
	if len(live) == 0 {
		// Nothing to wait on. Wait out the timeout anyway, so a caller
		// looping on this tool doesn't spin while every host is down.
		t := time.NewTimer(timeout)
		select {
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
		return okResp(id, w.reply())
	}
	if woke := w.waitAny(ctx, live, timeout); woke != "" {
		var rest []string
		for _, h := range live {
			if h != woke {
				rest = append(rest, h)
			}
		}
		w.readAll(ctx, rest)
	}
	return okResp(id, w.reply())
}

const eventsToolDescription = "Wait for something to happen in any tab on any host instead of polling screens. Returns the events after `cursor`, or blocks until one arrives or timeout_ms passes (then events is empty). Each event has host, tab_id (namespaced), seq (per-host), time, kind and kind-specific fields. Kinds: tab_created, tab_closed, tab_renamed (old_name/new_name), child_exited (exit_code), title_changed (old/new title, the leading status glyph of each, and old_state/new_state: \"working\" for spinner glyphs, \"idle\" for Claude Code's ✳, \"\" when unknown; an agent going idle often shows as new_state idle or the glyph disappearing), bell, notification (OSC 9 / 777 / 99 desktop notifications a program sent: protocol, notify_title, notify_body). Events carry no screen contents; read the screen with get_screen when an event says to look. Every reply has `cursor` (host -> {epoch, seq}): pass it back unchanged on the next call; omit it on the first call to get everything the daemons still hold (or call once with timeout_ms 0 and keep only the cursor to start from now). `missed` names hosts whose events after the cursor are gone: \"restarted\" (that daemon restarted, so its sequence reset) or \"overflow\" (more events than its ring holds happened since your last call); the reply then holds everything that host still has. `errors` names hosts that could not be read."

func eventsToolSchema() map[string]any {
	kinds := make([]string, len(tabevents.AllKinds))
	for i, k := range tabevents.AllKinds {
		kinds[i] = string(k)
	}
	strArray := func(desc string, items map[string]any) map[string]any {
		return map[string]any{"type": "array", "items": items, "description": desc}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cursor": map[string]any{
				"type":                 "object",
				"description":          "The cursor from the previous reply, unchanged. Omit on the first call.",
				"additionalProperties": map[string]any{"type": "object", "properties": map[string]any{"epoch": map[string]any{"type": "string"}, "seq": map[string]any{"type": "integer"}}},
			},
			"timeout_ms": map[string]any{"type": "integer", "description": "How long to block for a matching event. Default 30000, max 300000; 0 returns at once."},
			"tab_ids":    strArray("Only events for these tabs (namespaced ids from list_tabs). Omit for all tabs.", map[string]any{"type": "string"}),
			"kinds":      strArray("Only these event kinds. Omit for all kinds.", map[string]any{"type": "string", "enum": kinds}),
			"hosts":      strArray("Only these hosts (namespaces from list_tabs). Omit for all hosts.", map[string]any{"type": "string"}),
		},
	}
}
