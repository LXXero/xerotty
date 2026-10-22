package tabevents

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AllKinds lists every event kind.
var AllKinds = []Kind{TabCreated, TabClosed, TabRenamed, ChildExited, TitleChanged, Bell, Notification}

// ParseKinds converts kind names from a request into Kinds, rejecting
// unknown names so a typo doesn't silently filter out everything.
func ParseKinds(names []string) ([]Kind, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]Kind, 0, len(names))
	for _, n := range names {
		k := Kind(n)
		if !containsKind(AllKinds, k) {
			valid := make([]string, len(AllKinds))
			for i, a := range AllKinds {
				valid[i] = string(a)
			}
			return nil, fmt.Errorf("unknown event kind %q (valid: %s)", n, strings.Join(valid, ", "))
		}
		out = append(out, k)
	}
	return out, nil
}

// DefaultWait is the wait used when a request gives no timeout.
const DefaultWait = 30 * time.Second

// MaxWait caps how long one wait may block, so a forgotten request
// can't hold a daemon goroutine (or an MCP client's call) for hours.
const MaxWait = 5 * time.Minute

// ClampWait applies DefaultWait to a missing timeout (ms == nil) and
// bounds the result to [0, MaxWait]. 0 means "don't block".
func ClampWait(ms *int64) time.Duration {
	if ms == nil {
		return DefaultWait
	}
	d := time.Duration(*ms) * time.Millisecond
	if d < 0 {
		return 0
	}
	if d > MaxWait {
		return MaxWait
	}
	return d
}

// Query is one read of a ring through a cursor. Epoch and After are
// the cursor from the previous reply (Epoch "" = no previous reply).
// Timeout 0 reads without blocking.
type Query struct {
	Epoch   string
	After   uint64
	Timeout time.Duration
	Filter  Filter
}

// Read answers q: the matching events after the cursor, blocking up
// to q.Timeout (or until ctx ends) for at least one. A cursor whose
// epoch is not this ring's came from an earlier daemon process: its
// seq means nothing here, so Read returns everything the ring holds
// that matches, at once, with Missed set.
func (r *Ring) Read(ctx context.Context, q Query) Result {
	if q.Epoch != "" && q.Epoch != r.epoch {
		res := r.Since(0, q.Filter)
		res.Missed = true
		return res
	}
	if q.Timeout <= 0 {
		return r.Since(q.After, q.Filter)
	}
	ctx, cancel := context.WithTimeout(ctx, q.Timeout)
	defer cancel()
	return r.Wait(ctx, q.After, q.Filter)
}

// Gap reasons reported with a Missed result.
const (
	// GapRestarted: the cursor came from an earlier daemon process
	// (restart or serve --upgrade), whose events are gone.
	GapRestarted = "restarted"
	// GapOverflow: the ring overwrote events after the cursor before
	// they were read.
	GapOverflow = "overflow"
)

// GapReason says why res.Missed is set for a read made with the
// cursor (epoch, after), or "" when nothing was missed.
func GapReason(epoch string, after uint64, res Result) string {
	if !res.Missed {
		return ""
	}
	if (epoch != "" && epoch != res.Epoch) || after > res.Last {
		return GapRestarted
	}
	return GapOverflow
}
