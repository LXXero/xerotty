package daemon

import (
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/handoff"
	"github.com/LXXero/xerotty/internal/supervise"
	"github.com/LXXero/xerotty/internal/testutil"
)

// handoffPlumbing are the TabState fields that legitimately differ
// between the crash-resume state and the in-place upgrade handoff:
// fd numbers only mean something inside one process, and the crash
// path rebuilds the scrollback index from the file instead.
var handoffPlumbing = map[string]bool{
	"PtmxFD": true, "DiskFD": true, "DiskOffsets": true, "DiskSize": true, "MemScrollback": true,
}

// TestCrashStateMatchesUpgradeHandoff: the state the daemon streams to
// its supervisor for a crash resume must carry every non-plumbing
// field the in-place upgrade handoff does, with the same values. A
// crash resume used to restore only topology, so mutt's arrow keys
// came back in normal cursor mode and scroll regions were gone.
func TestCrashStateMatchesUpgradeHandoff(t *testing.T) {
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	d := New(&cfg, filepath.Join(testutil.SockDir(t), "xerottyd.sock"))
	sess := d.session("default")
	tab, _, err := sess.NewTab(0, 80, 24, "", "", nil)
	if err != nil {
		t.Skipf("no PTY: %v", err)
	}
	// DECCKM, DECKPAM, alt screen, DECLRMM + margins, charsets, origin
	// mode, autowrap off; `read` parks the shell so nothing changes
	// between the two snapshots.
	_, _ = tab.Term.Write([]byte(`printf '\033[?1h\033=\033[?1049h\033[?69h\033[3;20r\033[5;70s\033)0\033*A\033n\033}\033[?6h\033[?7l'; printf 'EMU_%s' READY; read x` + "\r"))
	deadline := time.Now().Add(10 * time.Second)
	for !viewportHas(tab, "EMU_READY") {
		if time.Now().After(deadline) {
			t.Fatal("setup never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}

	crash := d.SnapshotState()
	up, keep, err := d.SerializeUpgrade()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	t.Cleanup(func() {
		for _, ts := range up.Tabs {
			_ = syscall.Kill(ts.ChildPID, syscall.SIGKILL)
		}
		for _, f := range keep {
			_ = f.Close()
		}
	})
	if len(crash.Tabs) != 1 || len(up.Tabs) != 1 {
		t.Fatalf("tabs: crash %d, upgrade %d", len(crash.Tabs), len(up.Tabs))
	}
	c, u := crash.Tabs[0], up.Tabs[0]

	// Not vacuous: the state really is there.
	if !c.AppCursor || c.Margins == nil || *c.Margins != (handoff.Margins{Top: 3, Bottom: 20, Left: 5, Right: 70}) ||
		c.Charsets == nil || *c.Charsets != (handoff.Charsets{G: "B0AB", GL: 2, GR: 2}) {
		t.Fatalf("crash state misses app state: app cursor %v margins %+v charsets %+v", c.AppCursor, c.Margins, c.Charsets)
	}
	for _, m := range []int{1, 6, 66, 69, 1049} {
		if !hasMode(c.DECModesSet, m) {
			t.Errorf("crash state: DEC mode %d not in set list %v", m, c.DECModesSet)
		}
	}
	if !hasMode(c.DECModesReset, 7) {
		t.Errorf("crash state: autowrap reset missing from %v", c.DECModesReset)
	}
	if len(c.Screen) == 0 {
		t.Error("crash state carries no screen")
	}

	cv, uv := reflect.ValueOf(c), reflect.ValueOf(u)
	for i := 0; i < cv.NumField(); i++ {
		name := cv.Type().Field(i).Name
		if handoffPlumbing[name] {
			continue
		}
		if !reflect.DeepEqual(cv.Field(i).Interface(), uv.Field(i).Interface()) {
			t.Errorf("%s: crash state %+v, upgrade handoff %+v", name, cv.Field(i).Interface(), uv.Field(i).Interface())
		}
	}
}

func viewportHas(tab *Tab, needle string) bool {
	for _, row := range tab.Term.SnapshotViewport() {
		var sb strings.Builder
		for i := range row {
			sb.WriteString(row[i].Content)
		}
		if strings.Contains(sb.String(), needle) {
			return true
		}
	}
	return false
}

func hasMode(modes []int, m int) bool {
	for _, x := range modes {
		if x == m {
			return true
		}
	}
	return false
}

// TestStatePushFollowsModesAndOutput: with only one topology push
// behind it (the tab's spawn), the supervisor still learns of a mode
// change promptly and of new screen contents after output, and a tab
// that never stops printing cannot push faster than minStateGap.
func TestStatePushFollowsModesAndOutput(t *testing.T) {
	parent, childEnd, err := supervise.Pair()
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	client, err := supervise.NewClient(childEnd)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	d := New(&cfg, filepath.Join(testutil.SockDir(t), "xerottyd.sock"))
	d.SetSupervisor(client)
	sess := d.session("default")
	tab, _, err := sess.NewTab(0, 80, 24, "", "", nil)
	if err != nil {
		t.Skipf("no PTY: %v", err)
	}
	defer tab.Term.Close()

	states := make(chan *handoff.State, 256)
	go func() {
		for {
			kind, payload, files, err := parent.Recv()
			for _, f := range files {
				_ = f.Close()
			}
			if err != nil {
				close(states)
				return
			}
			if kind != supervise.KindState {
				continue
			}
			var st handoff.State
			if _, err := st.UnmarshalMsg(payload); err == nil {
				states <- &st
			}
		}
	}()
	waitState := func(what string, ok func(handoff.TabState) bool) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case st := <-states:
				if len(st.Tabs) == 1 && ok(st.Tabs[0]) {
					return
				}
			case <-deadline:
				t.Fatalf("supervisor never got a state with %s", what)
			}
		}
	}

	_, _ = tab.Term.Write([]byte(`printf '\033[?1h\033[4;12r'; read x` + "\r"))
	waitState("app cursor keys and the scroll region", func(ts handoff.TabState) bool {
		return ts.AppCursor && ts.Margins != nil && ts.Margins.Top == 4 && ts.Margins.Bottom == 12
	})
	_, _ = tab.Term.Write([]byte("PUSHED_BY_OUTPUT"))
	waitState("the screen after output", func(ts handoff.TabState) bool {
		for _, row := range ts.Screen {
			var sb strings.Builder
			for _, c := range row {
				sb.WriteString(c.Content)
			}
			if strings.Contains(sb.String(), "PUSHED_BY_OUTPUT") {
				return true
			}
		}
		return false
	})

	// Endless output plus a mode flip per line (an app hiding its
	// cursor around every redraw): pushes stay at most one per gap.
	_, _ = tab.Term.Write([]byte("\r" + `while :; do printf '\033[?25l%s\033[?25h\n' line; done` + "\r"))
	time.Sleep(200 * time.Millisecond)
	for len(states) > 0 {
		<-states
	}
	const window = 3 * time.Second
	time.Sleep(window)
	if n, max := len(states), int(window/minStateGap)+1; n > max {
		t.Fatalf("%d state pushes in %s under constant output; the cap allows %d", n, window, max)
	} else if n == 0 {
		t.Fatal("no state pushes at all under constant output")
	}
}
