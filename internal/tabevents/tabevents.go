// Package tabevents is the daemon's record of things that happen in its
// tabs: tab created / closed / renamed, child exit, program title
// changes, the bell and desktop notifications. Each event gets a
// per-daemon sequence number and a timestamp and lives in a bounded
// in-memory ring, so an agent can ask "what happened since seq N"
// (or block until something does) instead of polling every screen.
//
// Order is the order the daemon recorded events in. Within a single
// PTY read, titles and notifications (parsed in the OSC pre-pass) are
// recorded before a bell in that same read (parsed by the emulator
// after it), so a bell may come after an OSC event the program sent
// just after it.
//
// Events never carry screen contents. Everything in them comes from
// what the daemon already sees in the PTY stream (OSC titles, BEL,
// notification OSCs) or from its own tab bookkeeping.
package tabevents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/LXXero/xerotty/internal/tabs"
)

// Kind names what happened.
type Kind string

const (
	TabCreated   Kind = "tab_created"
	TabClosed    Kind = "tab_closed"
	TabRenamed   Kind = "tab_renamed"
	ChildExited  Kind = "child_exited"
	TitleChanged Kind = "title_changed"
	Bell         Kind = "bell"
	Notification Kind = "notification"
)

// Event is one recorded occurrence. Fields that don't apply to a kind
// are left empty and omitted from JSON.
type Event struct {
	Seq  uint64    `json:"seq"`
	Time time.Time `json:"time"`
	Kind Kind      `json:"kind"`
	Tab  uint32    `json:"tab"`
	// Name is the tab's assigned name when the event was recorded
	// ("" = unnamed).
	Name string `json:"name,omitempty"`

	// child_exited
	ExitCode *int `json:"exit_code,omitempty"`

	// tab_renamed
	OldName string `json:"old_name,omitempty"`
	NewName string `json:"new_name,omitempty"`

	// title_changed. Glyphs are the leading status symbols of each
	// title (tabs.StatusGlyphs); State is their classification
	// (tabs.GlyphState: "working", "idle", or "" when unknown).
	OldTitle string `json:"old_title,omitempty"`
	NewTitle string `json:"new_title,omitempty"`
	OldGlyph string `json:"old_glyph,omitempty"`
	NewGlyph string `json:"new_glyph,omitempty"`
	OldState string `json:"old_state,omitempty"`
	NewState string `json:"new_state,omitempty"`

	// notification. Protocol is "osc9", "osc777" or "osc99".
	Protocol    string `json:"protocol,omitempty"`
	NotifyTitle string `json:"notify_title,omitempty"`
	NotifyBody  string `json:"notify_body,omitempty"`
}

// TitleChange builds the title_changed event for a tab whose title
// went from old to new, or reports false when the change is not worth
// recording. Spinner animation is the reason for the filter: Claude
// Code and Codex rewrite the title several times a second while
// working, changing only the spinner frame, and recording each frame
// would flush the ring in a minute and wake every waiter for nothing.
// So a change is recorded when the title text (glyphs stripped)
// changes, when the classified state changes, or when an unclassified
// glyph changes (unknown glyphs pass through as-is).
func TitleChange(tab uint32, name, old, new string) (Event, bool) {
	if old == new {
		return Event{}, false
	}
	og, ng := tabs.StatusGlyphs(old), tabs.StatusGlyphs(new)
	ost, nst := tabs.GlyphState(og), tabs.GlyphState(ng)
	sameText := tabs.StripStatusGlyphs(old) == tabs.StripStatusGlyphs(new)
	sameGlyph := og == ng || (ost != "" && ost == nst)
	if sameText && sameGlyph {
		return Event{}, false
	}
	return Event{
		Kind: TitleChanged, Tab: tab, Name: name,
		OldTitle: old, NewTitle: new,
		OldGlyph: og, NewGlyph: ng,
		OldState: ost, NewState: nst,
	}, true
}

// Filter selects events. Empty slices match everything.
type Filter struct {
	Tabs  []uint32
	Kinds []Kind
}

func (f Filter) match(e *Event) bool {
	if len(f.Tabs) > 0 && !containsTab(f.Tabs, e.Tab) {
		return false
	}
	if len(f.Kinds) > 0 && !containsKind(f.Kinds, e.Kind) {
		return false
	}
	return true
}

func containsTab(s []uint32, v uint32) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func containsKind(s []Kind, v Kind) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// DefaultCapacity is the ring size the daemon uses. Events are small
// (no screen contents) and the filtered title stream produces a few
// per agent turn, so this covers hours of a busy fleet host.
const DefaultCapacity = 4096

// Ring is a bounded, concurrency-safe event log. The oldest events
// are overwritten once it is full.
type Ring struct {
	mu    sync.Mutex
	buf   []Event // len == capacity once allocated
	head  int     // index of the oldest event
	n     int     // events held
	last  uint64  // seq of the newest event; 0 = none yet
	epoch string
	// wake is closed (and replaced) on every Append so Wait callers
	// blocked on it re-check the ring.
	wake chan struct{}
	now  func() time.Time
}

// New returns an empty ring holding up to capacity events
// (DefaultCapacity when capacity <= 0).
func New(capacity int) *Ring {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Ring{
		buf:   make([]Event, capacity),
		epoch: newEpoch(),
		wake:  make(chan struct{}),
		now:   time.Now,
	}
}

// newEpoch mints a random id for this ring. Sequence numbers restart at
// 1 whenever the daemon process starts (including serve --upgrade), so
// a reader holding a cursor uses the epoch to tell "same log" from "new
// log whose seq happens to be past my cursor".
func newEpoch() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("pid%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Epoch identifies this ring (one per daemon process).
func (r *Ring) Epoch() string { return r.epoch }

// Append stamps e with the next sequence number and the current time,
// stores it, wakes waiters and returns the stored event. Nil-safe so
// callers without a daemon (tests) can record unconditionally.
func (r *Ring) Append(e Event) Event {
	if r == nil {
		return e
	}
	r.mu.Lock()
	r.last++
	e.Seq = r.last
	e.Time = r.now()
	idx := (r.head + r.n) % len(r.buf)
	if r.n == len(r.buf) {
		r.head = (r.head + 1) % len(r.buf)
	} else {
		r.n++
	}
	r.buf[idx] = e
	close(r.wake)
	r.wake = make(chan struct{})
	r.mu.Unlock()
	return e
}

// Result is a read of the ring.
type Result struct {
	Events []Event
	// Last is the newest seq in the ring when the read was made. A
	// caller passes it as the next after value; it advances even when
	// the filter matched nothing, so filtered-out events are not
	// re-scanned.
	Last  uint64
	Epoch string
	// Missed reports that events after the caller's cursor are no
	// longer available: either they were overwritten (the ring wrapped
	// past after+1) or after is beyond Last, meaning the cursor came
	// from an earlier daemon process whose log is gone (seq reset).
	// Events then holds everything the ring still has that matches.
	Missed bool
}

// Since returns the events with seq > after that match f, oldest
// first, without blocking.
func (r *Ring) Since(after uint64, f Filter) Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, _ := r.sinceLocked(after, f)
	return res
}

func (r *Ring) sinceLocked(after uint64, f Filter) (Result, <-chan struct{}) {
	res := Result{Last: r.last, Epoch: r.epoch}
	if after > r.last {
		res.Missed = true
		after = 0
	}
	oldest := r.last - uint64(r.n) + 1 // seq of buf[head] when n > 0
	if r.n > 0 && after+1 < oldest {
		res.Missed = true
	}
	for i := 0; i < r.n; i++ {
		e := &r.buf[(r.head+i)%len(r.buf)]
		if e.Seq > after && f.match(e) {
			res.Events = append(res.Events, *e)
		}
	}
	return res, r.wake
}

// Wait is Since that blocks until at least one matching event exists
// or ctx ends. On ctx end it returns what it has (possibly no events)
// with the current Last, so a timed-out wait still advances the
// caller's cursor past events the filter rejected. A Missed read
// returns at once, so the caller learns about the gap without waiting.
func (r *Ring) Wait(ctx context.Context, after uint64, f Filter) Result {
	for {
		r.mu.Lock()
		res, wake := r.sinceLocked(after, f)
		r.mu.Unlock()
		if len(res.Events) > 0 || res.Missed {
			return res
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return r.Since(after, f)
		}
	}
}
