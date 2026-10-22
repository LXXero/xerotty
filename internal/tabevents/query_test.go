package tabevents_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/tabevents"
)

func TestReadTimeoutZeroDoesNotBlock(t *testing.T) {
	r := tabevents.New(8)
	start := time.Now()
	res := r.Read(context.Background(), tabevents.Query{})
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("Read with Timeout 0 blocked")
	}
	if len(res.Events) != 0 || res.Missed || res.Epoch != r.Epoch() {
		t.Fatalf("empty ring read = %+v", res)
	}
}

func TestReadTimesOut(t *testing.T) {
	r := tabevents.New(8)
	r.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 1})
	start := time.Now()
	res := r.Read(context.Background(), tabevents.Query{Epoch: r.Epoch(), After: 1, Timeout: 150 * time.Millisecond})
	if el := time.Since(start); el < 150*time.Millisecond {
		t.Fatalf("returned after %v, before the timeout", el)
	}
	if len(res.Events) != 0 || res.Missed || res.Last != 1 {
		t.Fatalf("timed-out read = %+v", res)
	}
}

func TestReadWakesOnAppend(t *testing.T) {
	r := tabevents.New(8)
	go func() {
		time.Sleep(50 * time.Millisecond)
		r.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 2})
	}()
	start := time.Now()
	res := r.Read(context.Background(), tabevents.Query{Timeout: 5 * time.Second})
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Read did not wake on Append")
	}
	if len(res.Events) != 1 || res.Events[0].Tab != 2 {
		t.Fatalf("woke with %+v", res)
	}
}

// A cursor from another ring (an earlier daemon process) must not be
// trusted even when its seq is below this ring's newest: the read
// returns at once with everything the ring holds and Missed set.
func TestReadEpochMismatchIsRestart(t *testing.T) {
	old := tabevents.New(8)
	old.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 1})
	r := tabevents.New(8)
	for i := 0; i < 3; i++ {
		r.Append(tabevents.Event{Kind: tabevents.Bell, Tab: uint32(10 + i)})
	}
	q := tabevents.Query{Epoch: old.Epoch(), After: 1, Timeout: 5 * time.Second}
	start := time.Now()
	res := r.Read(context.Background(), q)
	if time.Since(start) > time.Second {
		t.Fatalf("restart read blocked")
	}
	if !res.Missed || len(res.Events) != 3 || res.Events[0].Seq != 1 {
		t.Fatalf("restart read = %+v", res)
	}
	if got := tabevents.GapReason(q.Epoch, q.After, res); got != tabevents.GapRestarted {
		t.Fatalf("GapReason = %q, want restarted", got)
	}
}

func TestGapReason(t *testing.T) {
	r := tabevents.New(4)
	for i := 0; i < 10; i++ {
		r.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 1})
	}
	// Same ring, cursor behind the oldest held event: overflow.
	res := r.Since(2, tabevents.Filter{})
	if got := tabevents.GapReason(r.Epoch(), 2, res); got != tabevents.GapOverflow {
		t.Errorf("overflow: GapReason = %q", got)
	}
	// No epoch, cursor past the newest: a sequence reset.
	res = r.Since(50, tabevents.Filter{})
	if got := tabevents.GapReason("", 50, res); got != tabevents.GapRestarted {
		t.Errorf("seq past newest: GapReason = %q", got)
	}
	// Cursor current: nothing missed.
	res = r.Since(10, tabevents.Filter{})
	if got := tabevents.GapReason(r.Epoch(), 10, res); got != "" {
		t.Errorf("current cursor: GapReason = %q", got)
	}
}

func TestParseKinds(t *testing.T) {
	ks, err := tabevents.ParseKinds([]string{"bell", "title_changed"})
	if err != nil || !reflect.DeepEqual(ks, []tabevents.Kind{tabevents.Bell, tabevents.TitleChanged}) {
		t.Fatalf("ParseKinds = %v, %v", ks, err)
	}
	if ks, err := tabevents.ParseKinds(nil); err != nil || ks != nil {
		t.Fatalf("ParseKinds(nil) = %v, %v", ks, err)
	}
	if _, err := tabevents.ParseKinds([]string{"bel"}); err == nil {
		t.Fatalf("unknown kind accepted")
	}
}

func TestClampWait(t *testing.T) {
	ms := func(v int64) *int64 { return &v }
	cases := []struct {
		in   *int64
		want time.Duration
	}{
		{nil, tabevents.DefaultWait},
		{ms(0), 0},
		{ms(-5), 0},
		{ms(1500), 1500 * time.Millisecond},
		{ms(24 * 3600 * 1000), tabevents.MaxWait},
	}
	for _, c := range cases {
		if got := tabevents.ClampWait(c.in); got != c.want {
			t.Errorf("ClampWait(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWireRoundTrip(t *testing.T) {
	code := 3
	in := tabevents.Result{
		Last: 7, Epoch: "abc", Missed: true,
		Events: []tabevents.Event{
			{Seq: 6, Time: time.Unix(1700000000, 123), Kind: tabevents.ChildExited, Tab: 4, Name: "build", ExitCode: &code},
			{Seq: 7, Time: time.Unix(1700000001, 0), Kind: tabevents.TitleChanged, Tab: 4,
				OldTitle: "⠐ Claude Code", NewTitle: "✳ Claude Code", OldGlyph: "⠐", NewGlyph: "✳", OldState: "working", NewState: "idle"},
		},
	}
	w := tabevents.ResultToWire(9, in)
	raw, err := w.MarshalMsg(nil)
	if err != nil {
		t.Fatal(err)
	}
	w2 := *w
	w2.Events = nil
	if _, err := w2.UnmarshalMsg(raw); err != nil {
		t.Fatal(err)
	}
	if w2.ReqID != 9 {
		t.Errorf("ReqID = %d", w2.ReqID)
	}
	out := tabevents.ResultFromWire(&w2)
	if len(out.Events) != 2 || out.Last != 7 || out.Epoch != "abc" || !out.Missed {
		t.Fatalf("round trip = %+v", out)
	}
	if !out.Events[0].Time.Equal(in.Events[0].Time) || out.Events[0].ExitCode == nil || *out.Events[0].ExitCode != 3 {
		t.Errorf("event 0 = %+v", out.Events[0])
	}
	if out.Events[1].ExitCode != nil || out.Events[1].NewState != "idle" || out.Events[1].OldGlyph != "⠐" {
		t.Errorf("event 1 = %+v", out.Events[1])
	}

	q := tabevents.Query{Epoch: "abc", After: 5, Timeout: 2 * time.Second,
		Filter: tabevents.Filter{Tabs: []uint32{4}, Kinds: []tabevents.Kind{tabevents.Bell}}}
	if got := tabevents.QueryFromWire(tabevents.QueryToWire(1, q)); !reflect.DeepEqual(got, q) {
		t.Errorf("query round trip = %+v, want %+v", got, q)
	}
}

// Both MCP servers describe the event kinds with KindsDescription, so
// a new kind must be explained there or agents won't know it exists.
func TestKindsDescriptionNamesEveryKind(t *testing.T) {
	for _, k := range tabevents.AllKinds {
		if !strings.Contains(tabevents.KindsDescription, string(k)) {
			t.Errorf("KindsDescription does not mention %q", k)
		}
	}
}
