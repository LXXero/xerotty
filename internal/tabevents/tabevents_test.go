package tabevents

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func seqs(es []Event) []uint64 {
	out := make([]uint64, len(es))
	for i, e := range es {
		out[i] = e.Seq
	}
	return out
}

func equal(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAppendStampsSeqAndTime(t *testing.T) {
	r := New(4)
	before := time.Now()
	e1 := r.Append(Event{Kind: Bell, Tab: 1})
	e2 := r.Append(Event{Kind: Bell, Tab: 1, Seq: 99}) // caller seq ignored
	if e1.Seq != 1 || e2.Seq != 2 {
		t.Fatalf("seqs = %d, %d; want 1, 2", e1.Seq, e2.Seq)
	}
	if e1.Time.Before(before) || e2.Time.Before(e1.Time) {
		t.Errorf("times not stamped in order: %v %v", e1.Time, e2.Time)
	}
	if r.Epoch() == "" || New(4).Epoch() == r.Epoch() {
		t.Error("each ring needs its own non-empty epoch")
	}
	var nilRing *Ring
	nilRing.Append(Event{Kind: Bell}) // must not panic
}

func TestRingWrapAndMissed(t *testing.T) {
	r := New(3)
	for i := 0; i < 5; i++ {
		r.Append(Event{Kind: Bell, Tab: 1})
	}
	// Ring holds 3, 4, 5.
	res := r.Since(0, Filter{})
	if !equal(seqs(res.Events), []uint64{3, 4, 5}) || res.Last != 5 || !res.Missed {
		t.Errorf("Since(0) = %v last %d missed %v", seqs(res.Events), res.Last, res.Missed)
	}
	// after=2: seq 3 is the next one and still here, nothing missed.
	res = r.Since(2, Filter{})
	if !equal(seqs(res.Events), []uint64{3, 4, 5}) || res.Missed {
		t.Errorf("Since(2) = %v missed %v", seqs(res.Events), res.Missed)
	}
	res = r.Since(4, Filter{})
	if !equal(seqs(res.Events), []uint64{5}) || res.Missed {
		t.Errorf("Since(4) = %v missed %v", seqs(res.Events), res.Missed)
	}
	res = r.Since(5, Filter{})
	if len(res.Events) != 0 || res.Missed || res.Last != 5 {
		t.Errorf("Since(5) = %v missed %v last %d", seqs(res.Events), res.Missed, res.Last)
	}
}

func TestEmptyRing(t *testing.T) {
	r := New(3)
	res := r.Since(0, Filter{})
	if len(res.Events) != 0 || res.Missed || res.Last != 0 || res.Epoch != r.Epoch() {
		t.Errorf("empty Since(0) = %+v", res)
	}
}

// A cursor past the newest seq comes from an earlier daemon process
// (seq restarted at 1): report the gap and return everything held.
func TestCursorAheadIsReset(t *testing.T) {
	r := New(8)
	r.Append(Event{Kind: TabCreated, Tab: 1})
	r.Append(Event{Kind: Bell, Tab: 1})
	res := r.Since(40, Filter{})
	if !res.Missed || !equal(seqs(res.Events), []uint64{1, 2}) || res.Last != 2 {
		t.Errorf("Since(40) = %v missed %v last %d", seqs(res.Events), res.Missed, res.Last)
	}
	// Wait must not block on a reset either.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	res = r.Wait(ctx, 40, Filter{Kinds: []Kind{Notification}})
	if !res.Missed || len(res.Events) != 0 || time.Since(start) > 500*time.Millisecond {
		t.Errorf("Wait(40) = %+v after %v", res, time.Since(start))
	}
}

func TestFilters(t *testing.T) {
	r := New(16)
	r.Append(Event{Kind: TabCreated, Tab: 1})  // 1
	r.Append(Event{Kind: Bell, Tab: 1})        // 2
	r.Append(Event{Kind: TabCreated, Tab: 2})  // 3
	r.Append(Event{Kind: Bell, Tab: 2})        // 4
	r.Append(Event{Kind: ChildExited, Tab: 2}) // 5
	cases := []struct {
		f    Filter
		want []uint64
	}{
		{Filter{}, []uint64{1, 2, 3, 4, 5}},
		{Filter{Tabs: []uint32{2}}, []uint64{3, 4, 5}},
		{Filter{Kinds: []Kind{Bell}}, []uint64{2, 4}},
		{Filter{Kinds: []Kind{Bell, ChildExited}}, []uint64{2, 4, 5}},
		{Filter{Tabs: []uint32{1}, Kinds: []Kind{Bell}}, []uint64{2}},
		{Filter{Tabs: []uint32{9}}, nil},
	}
	for _, c := range cases {
		res := r.Since(0, c.f)
		if !equal(seqs(res.Events), c.want) || res.Last != 5 {
			t.Errorf("filter %+v = %v last %d, want %v", c.f, seqs(res.Events), res.Last, c.want)
		}
	}
}

func TestWaitReturnsAtOnceWhenEventsExist(t *testing.T) {
	r := New(8)
	r.Append(Event{Kind: Bell, Tab: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	res := r.Wait(ctx, 0, Filter{})
	if len(res.Events) != 1 || time.Since(start) > 500*time.Millisecond {
		t.Errorf("Wait = %+v after %v", res, time.Since(start))
	}
}

func TestWaitBlocksUntilAppend(t *testing.T) {
	r := New(8)
	r.Append(Event{Kind: Bell, Tab: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan Result, 1)
	go func() { got <- r.Wait(ctx, 1, Filter{}) }()
	select {
	case res := <-got:
		t.Fatalf("Wait returned before any new event: %+v", res)
	case <-time.After(50 * time.Millisecond):
	}
	r.Append(Event{Kind: TabClosed, Tab: 1})
	select {
	case res := <-got:
		if !equal(seqs(res.Events), []uint64{2}) || res.Last != 2 {
			t.Errorf("Wait = %v last %d", seqs(res.Events), res.Last)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not wake on Append")
	}
}

// Events the filter rejects don't end the wait, but a timed-out wait
// still reports the newest seq so the cursor moves past them.
func TestWaitFilterAndTimeout(t *testing.T) {
	r := New(8)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	got := make(chan Result, 1)
	go func() { got <- r.Wait(ctx, 0, Filter{Tabs: []uint32{2}, Kinds: []Kind{Notification}}) }()
	time.Sleep(30 * time.Millisecond)
	r.Append(Event{Kind: Notification, Tab: 1}) // wrong tab
	r.Append(Event{Kind: Bell, Tab: 2})         // wrong kind
	start := time.Now()
	select {
	case res := <-got:
		if len(res.Events) != 0 || res.Missed || res.Last != 2 {
			t.Errorf("timed-out Wait = %+v", res)
		}
		if time.Since(start) < 100*time.Millisecond {
			t.Errorf("Wait returned %v after a non-matching append; should run to the timeout", time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait ignored its timeout")
	}

	// A matching event wakes a filtered waiter.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	go func() { got <- r.Wait(ctx2, 2, Filter{Kinds: []Kind{Notification}}) }()
	time.Sleep(30 * time.Millisecond)
	r.Append(Event{Kind: Bell, Tab: 2})
	r.Append(Event{Kind: Notification, Tab: 2, NotifyBody: "done"})
	select {
	case res := <-got:
		if !equal(seqs(res.Events), []uint64{4}) || res.Last != 4 {
			t.Errorf("filtered Wait = %v last %d", seqs(res.Events), res.Last)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("filtered Wait did not wake on a matching event")
	}
}

func TestConcurrentAppendWait(t *testing.T) {
	r := New(64)
	const n = 200
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			r.Append(Event{Kind: Bell, Tab: uint32(i % 3)})
		}
	}()
	var cursor uint64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for cursor < n && ctx.Err() == nil {
		res := r.Wait(ctx, cursor, Filter{})
		for _, e := range res.Events {
			if e.Seq <= cursor {
				t.Fatalf("seq %d not after cursor %d", e.Seq, cursor)
			}
		}
		cursor = res.Last
	}
	wg.Wait()
	if cursor != n {
		t.Errorf("cursor = %d, want %d", cursor, n)
	}
}

func TestTitleChange(t *testing.T) {
	cases := []struct {
		desc, old, new string
		record         bool
		ost, nst       string
	}{
		{"same title", "✳ Claude Code", "✳ Claude Code", false, "", ""},
		{"Claude starts working", "✳ Claude Code", "⠐ Claude Code", true, "idle", "working"},
		{"spinner frame only", "⠐ Claude Code", "⠂ Claude Code", false, "", ""},
		{"half-circle frames", "◐ task", "◓ task", false, "", ""},
		{"Claude goes idle", "⠂ Claude Code", "✳ Claude Code", true, "working", "idle"},
		{"Codex spinner frame", "⠹ codexprobe", "⠸ codexprobe", false, "", ""},
		{"Codex doubled frame, text changes", "⠹ codexprobe", "⠹ ⠹ | codexprobe", true, "working", "working"},
		{"Codex goes idle: glyph removed", "⠏ | codexprobe", "codexprobe", true, "working", ""},
		{"plain text change", "vim a.txt", "vim b.txt", true, "", ""},
		{"unknown glyph changes", "🚀 deploy", "🔥 deploy", true, "", ""},
		{"first title", "", "zsh: ~/git", true, "", ""},
	}
	for _, c := range cases {
		e, ok := TitleChange(3, "build", c.old, c.new)
		if ok != c.record {
			t.Errorf("%s: recorded = %v, want %v", c.desc, ok, c.record)
			continue
		}
		if !ok {
			continue
		}
		if e.Kind != TitleChanged || e.Tab != 3 || e.Name != "build" ||
			e.OldTitle != c.old || e.NewTitle != c.new ||
			e.OldState != c.ost || e.NewState != c.nst {
			t.Errorf("%s: event = %+v, want states %q→%q", c.desc, e, c.ost, c.nst)
		}
	}
	e, _ := TitleChange(1, "", "🚀 deploy", "🔥 deploy")
	if e.OldGlyph != "🚀" || e.NewGlyph != "🔥" {
		t.Errorf("glyphs = %q→%q", e.OldGlyph, e.NewGlyph)
	}
}

// The JSON shape is what wait_for_events and `xerotty events` emit:
// snake_case keys, irrelevant fields omitted, exit code 0 kept.
func TestEventJSON(t *testing.T) {
	r := New(4)
	zero := 0
	e := r.Append(Event{Kind: ChildExited, Tab: 2, ExitCode: &zero})
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"seq":1`, `"kind":"child_exited"`, `"tab":2`, `"exit_code":0`, `"time":"`} {
		if !strings.Contains(s, want) {
			t.Errorf("JSON %s lacks %s", s, want)
		}
	}
	for _, absent := range []string{"old_title", "notify_body", "name"} {
		if strings.Contains(s, absent) {
			t.Errorf("JSON %s has empty field %s", s, absent)
		}
	}
}
