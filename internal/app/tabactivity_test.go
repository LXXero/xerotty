package app

import (
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/tabs"
)

// TestFirstTabOfNewWindowNotMarkedUnseen covers a brand-new window: its
// only tab prints a shell prompt while no tab bar is drawn, then a
// second tab opens. The first tab must not show unseen output, because
// it was on screen when the prompt arrived. The active-tab stamp used
// to run only inside renderTabBar, which returns early for one tab, so
// tabViewed stayed zero and the prompt lit the activity bar.
func TestFirstTabOfNewWindowNotMarkedUnseen(t *testing.T) {
	m := &tabs.Manager{Tabs: []*tabs.Tab{{ID: 1}}}
	w := &Window{tabs: m, tabViewed: make(map[int]time.Time)}

	t0 := time.Now()
	prompt := t0.Add(10 * time.Millisecond)
	// Frames while the window has one tab (no tab bar drawn).
	w.markActiveTabViewed(t0)
	w.markActiveTabViewed(prompt.Add(16 * time.Millisecond))

	// Second tab opens and becomes active.
	m.Tabs = append(m.Tabs, &tabs.Tab{ID: 2})
	m.ActiveIdx = 1
	w.markActiveTabViewed(prompt.Add(32 * time.Millisecond))

	if w.hasUnseenOutput(1, false, prompt) {
		t.Fatal("first tab shows unseen output for a prompt it displayed while active")
	}

	// Real output after the switch still lights it.
	later := prompt.Add(time.Second)
	if !w.hasUnseenOutput(1, false, later) {
		t.Fatal("first tab does not show output that arrived while it was in the background")
	}
	// The active tab never shows it.
	if w.hasUnseenOutput(2, true, later) {
		t.Fatal("active tab shows unseen output")
	}
}
