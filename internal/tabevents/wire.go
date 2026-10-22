package tabevents

import (
	"time"

	"github.com/LXXero/xerotty/internal/protocol"
)

// ToWire converts an event to its wire form (protocol.TabEvent).
func ToWire(e Event) protocol.TabEvent {
	w := protocol.TabEvent{
		Seq: e.Seq, TimeUnixNano: e.Time.UnixNano(), Kind: string(e.Kind), Tab: e.Tab, Name: e.Name,
		OldName: e.OldName, NewName: e.NewName,
		OldTitle: e.OldTitle, NewTitle: e.NewTitle,
		OldGlyph: e.OldGlyph, NewGlyph: e.NewGlyph,
		OldState: e.OldState, NewState: e.NewState,
		Protocol: e.Protocol, NotifyTitle: e.NotifyTitle, NotifyBody: e.NotifyBody,
	}
	if e.ExitCode != nil {
		w.HasExitCode = true
		w.ExitCode = int32(*e.ExitCode)
	}
	return w
}

// FromWire converts a wire event back to an Event.
func FromWire(w protocol.TabEvent) Event {
	e := Event{
		Seq: w.Seq, Time: time.Unix(0, w.TimeUnixNano), Kind: Kind(w.Kind), Tab: w.Tab, Name: w.Name,
		OldName: w.OldName, NewName: w.NewName,
		OldTitle: w.OldTitle, NewTitle: w.NewTitle,
		OldGlyph: w.OldGlyph, NewGlyph: w.NewGlyph,
		OldState: w.OldState, NewState: w.NewState,
		Protocol: w.Protocol, NotifyTitle: w.NotifyTitle, NotifyBody: w.NotifyBody,
	}
	if w.HasExitCode {
		c := int(w.ExitCode)
		e.ExitCode = &c
	}
	return e
}

// ResultToWire builds the Events reply for a request.
func ResultToWire(reqID uint64, r Result) *protocol.Events {
	out := &protocol.Events{ReqID: reqID, Last: r.Last, Epoch: r.Epoch, Missed: r.Missed,
		Events: make([]protocol.TabEvent, len(r.Events))}
	for i, e := range r.Events {
		out.Events[i] = ToWire(e)
	}
	return out
}

// ResultFromWire converts an Events reply back to a Result.
func ResultFromWire(w *protocol.Events) Result {
	r := Result{Last: w.Last, Epoch: w.Epoch, Missed: w.Missed}
	if len(w.Events) > 0 {
		r.Events = make([]Event, len(w.Events))
		for i, e := range w.Events {
			r.Events[i] = FromWire(e)
		}
	}
	return r
}

// QueryToWire builds the EventsWaitReq for q.
func QueryToWire(reqID uint64, q Query) *protocol.EventsWaitReq {
	req := &protocol.EventsWaitReq{ReqID: reqID, Epoch: q.Epoch, After: q.After,
		TimeoutMs: q.Timeout.Milliseconds(), Tabs: q.Filter.Tabs}
	for _, k := range q.Filter.Kinds {
		req.Kinds = append(req.Kinds, string(k))
	}
	return req
}

// QueryFromWire converts a request to a Query, clamping its timeout
// to MaxWait. Unknown kind names are kept: they match nothing, which
// is what a newer client asking for a kind this daemon doesn't record
// should get.
func QueryFromWire(req *protocol.EventsWaitReq) Query {
	ms := req.TimeoutMs
	q := Query{Epoch: req.Epoch, After: req.After, Timeout: ClampWait(&ms),
		Filter: Filter{Tabs: req.Tabs}}
	for _, k := range req.Kinds {
		q.Filter.Kinds = append(q.Filter.Kinds, Kind(k))
	}
	return q
}
