package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/tabevents"
	"github.com/LXXero/xerotty/internal/terminal"
	"github.com/LXXero/xerotty/internal/testutil"
)

// TestTabEventsFromPTY drives a real tab whose program emits titles,
// a bell and the three notification OSCs, then exits with code 3, and
// checks the daemon recorded exactly the events the agent-facing
// tools will report: lifecycle, the title changes minus spinner
// frames, one bell, the notifications, the exit code.
func TestTabEventsFromPTY(t *testing.T) {
	cfg := config.Default()
	d := New(&cfg, filepath.Join(testutil.SockDir(t), "xerottyd.sock"))
	sess := d.session("default")

	script := "printf '" +
		"\033]0;⠐ Claude Code\007" +
		"\033]0;⠂ Claude Code\007" + // spinner frame: not recorded
		"\033]0;✳ Claude Code\007" +
		"\007" +
		"\033]9;Build finished\007" +
		"\033]777;notify;Codex;Turn complete\007" +
		"\033]99;i=1:d=0;Tests\007\033]99;i=1:p=body;3 failed\007" +
		"'; sleep 0.3; exit 3"
	tab, _, err := sess.NewTab(0, 80, 24, "", "agent", &terminal.LaunchCmd{Argv: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatalf("NewTab: %v", err)
	}
	defer sess.CloseTab(tab.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exit := d.Events().Wait(ctx, 0, tabevents.Filter{Kinds: []tabevents.Kind{tabevents.ChildExited}})
	if len(exit.Events) != 1 {
		t.Fatalf("no child_exited event: %+v", exit)
	}
	if c := exit.Events[0].ExitCode; c == nil || *c != 3 {
		t.Errorf("exit code = %v, want 3", c)
	}

	all := d.Events().Since(0, tabevents.Filter{Tabs: []uint32{tab.ID}})
	// The bell comes from the vt emulator, which parses a read only
	// after the OSC pre-pass has dispatched that read's titles and
	// notifications, so its place among them is not fixed. Check it
	// separately and the rest in order.
	var kinds []tabevents.Kind
	var ev []tabevents.Event
	bells := 0
	for _, e := range all.Events {
		if e.Name != "agent" {
			t.Errorf("event %d (%s) name = %q, want agent", e.Seq, e.Kind, e.Name)
		}
		if e.Kind == tabevents.Bell {
			bells++
			continue
		}
		kinds = append(kinds, e.Kind)
		ev = append(ev, e)
	}
	if bells != 1 {
		t.Errorf("bell events = %d, want 1", bells)
	}
	want := []tabevents.Kind{
		tabevents.TabCreated,
		tabevents.TitleChanged, // "" → ⠐ Claude Code
		tabevents.TitleChanged, // ⠂ → ✳
		tabevents.Notification,
		tabevents.Notification,
		tabevents.Notification,
		tabevents.ChildExited,
	}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if ev[1].NewTitle != "⠐ Claude Code" || ev[1].NewState != "working" || ev[1].NewGlyph != "⠐" {
		t.Errorf("first title event = %+v", ev[1])
	}
	if ev[2].OldTitle != "⠂ Claude Code" || ev[2].NewTitle != "✳ Claude Code" ||
		ev[2].OldState != "working" || ev[2].NewState != "idle" {
		t.Errorf("idle title event = %+v", ev[2])
	}
	if ev[3].Protocol != "osc9" || ev[3].NotifyBody != "Build finished" {
		t.Errorf("osc9 event = %+v", ev[3])
	}
	if ev[4].Protocol != "osc777" || ev[4].NotifyTitle != "Codex" || ev[4].NotifyBody != "Turn complete" {
		t.Errorf("osc777 event = %+v", ev[4])
	}
	if ev[5].Protocol != "osc99" || ev[5].NotifyTitle != "Tests" || ev[5].NotifyBody != "3 failed" {
		t.Errorf("osc99 event = %+v", ev[5])
	}
	for i := 1; i < len(ev); i++ {
		if ev[i].Seq <= ev[i-1].Seq || ev[i].Time.Before(ev[i-1].Time) {
			t.Errorf("event %d not after %d: %+v / %+v", i, i-1, ev[i-1], ev[i])
		}
	}
}

// Rename and close are recorded with the names involved; a rename to
// the same name or one refused for a collision records nothing.
func TestTabEventsRenameClose(t *testing.T) {
	cfg := config.Default()
	d := New(&cfg, filepath.Join(testutil.SockDir(t), "xerottyd.sock"))
	sess := d.session("default")
	sleep := &terminal.LaunchCmd{Argv: []string{"sleep", "30"}}

	a, _, err := sess.NewTab(0, 80, 24, "", "", sleep)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := sess.NewTab(0, 80, 24, "", "taken", sleep)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.CloseTab(b.ID)
	start := d.Events().Since(0, tabevents.Filter{}).Last

	if !sess.RenameTab(a.ID, "logs") {
		t.Fatal("rename failed")
	}
	sess.RenameTab(a.ID, "logs")  // no change
	sess.RenameTab(a.ID, "taken") // collision, refused
	sess.CloseTab(a.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.Events().Wait(ctx, start, tabevents.Filter{Kinds: []tabevents.Kind{tabevents.ChildExited}})
	got := d.Events().Since(start, tabevents.Filter{Tabs: []uint32{a.ID}}).Events
	if len(got) != 3 {
		t.Fatalf("events = %+v, want renamed, closed, child_exited", got)
	}
	if got[0].Kind != tabevents.TabRenamed || got[0].OldName != "" || got[0].NewName != "logs" {
		t.Errorf("rename event = %+v", got[0])
	}
	if got[1].Kind != tabevents.TabClosed || got[1].Name != "logs" {
		t.Errorf("close event = %+v", got[1])
	}
	if got[2].Kind != tabevents.ChildExited || got[2].ExitCode == nil {
		t.Errorf("exit event = %+v", got[2])
	}
}

// A program ringing the bell in a loop records one event per
// bellEventGap, so it can't flush the rest of the ring.
func TestTabEventsBellDebounce(t *testing.T) {
	cfg := config.Default()
	d := New(&cfg, filepath.Join(testutil.SockDir(t), "xerottyd.sock"))
	sess := d.session("default")
	tab, _, err := sess.NewTab(0, 80, 24, "", "", &terminal.LaunchCmd{Argv: []string{"sh", "-c", "for i in 1 2 3 4 5 6 7 8 9 10; do printf '\\007'; done; sleep 0.3"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.CloseTab(tab.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.Events().Wait(ctx, 0, tabevents.Filter{Kinds: []tabevents.Kind{tabevents.ChildExited}})
	bells := d.Events().Since(0, tabevents.Filter{Kinds: []tabevents.Kind{tabevents.Bell}}).Events
	if len(bells) != 1 {
		t.Errorf("bell events = %d, want 1", len(bells))
	}
}
