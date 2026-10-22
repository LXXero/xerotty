package tabs

import (
	"testing"

	"github.com/LXXero/xerotty/internal/terminal"
)

// closeOnlySource satisfies terminal.Source for Manager tests; only
// Close is called.
type closeOnlySource struct {
	terminal.Source
	closed bool
}

func (s *closeOnlySource) Close() { s.closed = true }

func managerWith(n, active int) (*Manager, []*Tab) {
	m := &Manager{}
	for i := 0; i < n; i++ {
		m.Tabs = append(m.Tabs, &Tab{ID: i + 1, Terminal: &closeOnlySource{}})
	}
	m.ActiveIdx = active
	return m, append([]*Tab(nil), m.Tabs...)
}

func TestCloseTabKeepsActiveTab(t *testing.T) {
	cases := []struct {
		desc            string
		n, active, idx  int
		wantActiveTabID int
	}{
		{"close before active: same tab stays active", 5, 3, 1, 4},
		{"close after active: same tab stays active", 5, 1, 3, 2},
		{"close active: next tab becomes active", 5, 2, 2, 4},
		{"close active last tab: previous becomes active", 5, 4, 4, 4},
	}
	for _, c := range cases {
		m, orig := managerWith(c.n, c.active)
		m.CloseTab(c.idx)
		if !orig[c.idx].Terminal.(*closeOnlySource).closed {
			t.Errorf("%s: closed tab's terminal was not closed", c.desc)
		}
		if got := m.Active(); got == nil || got.ID != c.wantActiveTabID {
			t.Errorf("%s: active tab = %v, want ID %d", c.desc, got, c.wantActiveTabID)
		}
	}
}
