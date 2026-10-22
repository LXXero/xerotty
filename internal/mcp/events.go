package mcp

import (
	"encoding/json"

	"github.com/LXXero/xerotty/internal/tabevents"
)

// eventsResult is the result of events/wait (wait_for_events).
// last_seq + epoch are the cursor for the next call; missed_reason is
// "restarted" (the cursor came from an earlier daemon process) or
// "overflow" (the ring dropped events before they were read).
type eventsResult struct {
	Events       []tabevents.Event `json:"events"`
	LastSeq      uint64            `json:"last_seq"`
	Epoch        string            `json:"epoch"`
	Missed       bool              `json:"missed"`
	MissedReason string            `json:"missed_reason,omitempty"`
}

// handleEventsWait returns the tab events after the caller's cursor,
// blocking up to timeout_ms for at least one. Read-only, so allowed in
// observe mode. Runs on its own goroutine (see isBlocking), so a long
// wait doesn't hold up other calls on this connection; it ends early
// when the connection closes.
func (c *agentConn) handleEventsWait(req *rpcRequest) *rpcResponse {
	var p struct {
		AfterSeq  uint64       `json:"after_seq"`
		Epoch     string       `json:"epoch"`
		TimeoutMs *int64       `json:"timeout_ms"`
		TabIDs    []tabIDParam `json:"tab_ids"`
		Kinds     []string     `json:"kinds"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return invalidParams(req.ID, err.Error())
		}
	}
	kinds, err := tabevents.ParseKinds(p.Kinds)
	if err != nil {
		return invalidParams(req.ID, err.Error())
	}
	q := tabevents.Query{
		Epoch:   p.Epoch,
		After:   p.AfterSeq,
		Timeout: tabevents.ClampWait(p.TimeoutMs),
		Filter:  tabevents.Filter{Kinds: kinds},
	}
	for _, id := range p.TabIDs {
		q.Filter.Tabs = append(q.Filter.Tabs, uint32(id))
	}
	res := c.srv.d.Events().Read(c.ctx, q)
	out := eventsResult{
		Events:       res.Events,
		LastSeq:      res.Last,
		Epoch:        res.Epoch,
		Missed:       res.Missed,
		MissedReason: tabevents.GapReason(p.Epoch, p.AfterSeq, res),
	}
	if out.Events == nil {
		out.Events = []tabevents.Event{}
	}
	return ok(req.ID, out)
}

// isBlocking reports whether req may block for a long time (an
// events wait), so run serves it on its own goroutine.
func isBlocking(req *rpcRequest) bool {
	switch req.Method {
	case "events/wait":
		return true
	case "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(req.Params, &p)
		return p.Name == "wait_for_events"
	}
	return false
}
