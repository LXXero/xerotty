package runner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/protocol"
)

// connectTab is one row of `xerotty connect --list`: the topology's
// TabInfo merged with the per-tab TabState the daemon pushes right
// after attach (cwd, foreground process, live title/name).
type connectTab struct {
	ID    uint32
	Name  string
	Title string
	CWD   string
	Fg    string
}

// label is what the tab bar shows: the assigned name, else the OSC
// title, else the foreground process.
func (t connectTab) label() string {
	switch {
	case t.Name != "":
		return t.Name
	case t.Title != "":
		return t.Title
	default:
		return t.Fg
	}
}

func tabsFromAttached(a *protocol.Attached) []connectTab {
	out := make([]connectTab, 0, len(a.Tabs))
	for _, ti := range a.Tabs {
		out = append(out, connectTab{ID: ti.ID, Name: ti.Name, Title: ti.Title})
	}
	return out
}

// mergeTabState folds a TabState push into the matching row. Name
// and Title only overwrite when set, so an older daemon that omits
// them keeps the topology's values.
func mergeTabState(tabs []connectTab, st *protocol.TabState) bool {
	for i := range tabs {
		if tabs[i].ID != st.ID {
			continue
		}
		tabs[i].CWD = st.CWD
		tabs[i].Fg = st.ForegroundProcessName
		if st.Name != "" {
			tabs[i].Name = st.Name
		}
		if st.Title != "" {
			tabs[i].Title = st.Title
		}
		return true
	}
	return false
}

// formatTabList renders one line per tab: a "*" marker on the
// focused tab (the one a plain `xerotty connect` lands on), then id,
// label, cwd and foreground process, tab-separated and aligned.
// Empty fields print as "-" so every line has the same columns.
func formatTabList(tabs []connectTab, focusedID uint32) string {
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	for _, t := range tabs {
		mark := " "
		if t.ID == focusedID {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s %d\t%s\t%s\t%s\n", mark, t.ID, orDash(t.label()), orDash(t.CWD), orDash(t.Fg))
	}
	_ = tw.Flush()
	return sb.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// matchTab resolves a --tab argument against the tabs as --list
// shows them: a numeric tab id first, then an exact name, title or
// shown label, then a case-insensitive substring of the name, title
// or foreground process. Ambiguous and missing matches return an
// error that lists the candidates.
func matchTab(tabs []connectTab, query string) (connectTab, error) {
	if id, err := strconv.ParseUint(query, 10, 32); err == nil {
		for _, t := range tabs {
			if t.ID == uint32(id) {
				return t, nil
			}
		}
	}
	pick := func(keep func(connectTab) bool) []connectTab {
		var hits []connectTab
		for _, t := range tabs {
			if keep(t) {
				hits = append(hits, t)
			}
		}
		return hits
	}
	exact := pick(func(t connectTab) bool {
		return t.Name == query || t.Title == query || t.label() == query
	})
	if len(exact) == 1 {
		return exact[0], nil
	}
	if len(exact) > 1 {
		return connectTab{}, fmt.Errorf("tab %q is ambiguous, it matches:\n%s", query, candidateList(exact))
	}
	q := strings.ToLower(query)
	sub := pick(func(t connectTab) bool {
		for _, f := range []string{t.Name, t.Title, t.Fg} {
			if strings.Contains(strings.ToLower(f), q) {
				return true
			}
		}
		return false
	})
	switch len(sub) {
	case 1:
		return sub[0], nil
	case 0:
		return connectTab{}, fmt.Errorf("no tab matches %q; tabs:\n%s", query, candidateList(tabs))
	default:
		return connectTab{}, fmt.Errorf("tab %q is ambiguous, it matches:\n%s", query, candidateList(sub))
	}
}

func candidateList(tabs []connectTab) string {
	sorted := append([]connectTab(nil), tabs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var sb strings.Builder
	for _, t := range sorted {
		fmt.Fprintf(&sb, "  %d\t%s\n", t.ID, orDash(t.label()))
	}
	return sb.String()
}

// waitAttached waits for the Attached frame, failing on a daemon
// error, a closed connection or a timeout instead of hanging.
func waitAttached(c *clientproto.Client, timeout time.Duration) (*protocol.Attached, error) {
	select {
	case a := <-c.Attached():
		return a, nil
	case e := <-c.Errors():
		return nil, fmt.Errorf("daemon error %d: %s", e.Code, e.Message)
	case <-c.Closed():
		if err := c.ExitErr(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("connection closed before attach")
	case <-time.After(timeout):
		return nil, fmt.Errorf("no attach reply within %s", timeout)
	}
}

// collectTabStates gathers the TabState each subscribed tab gets
// right after attach, so --list and --tab see cwd, foreground process
// and live titles. It keeps draining the frame channels Client.Run
// fills with blocking sends, so the read loop never wedges, and
// returns the screen frames it took off them (CellFull, CellDiff,
// Cursor, ChildExit, in arrival order) for the viewer to replay. It
// stops once every tab has reported or the timeout passes (an older
// daemon may not send them).
func collectTabStates(c *clientproto.Client, tabs []connectTab, timeout time.Duration) []any {
	var frames []any
	seen := make(map[uint32]bool, len(tabs))
	deadline := time.After(timeout)
	for len(seen) < len(tabs) {
		select {
		case st := <-c.TabState():
			if mergeTabState(tabs, st) {
				seen[st.ID] = true
			}
		case f := <-c.CellFull():
			frames = append(frames, f)
		case d := <-c.CellDiff():
			frames = append(frames, d)
		case cur := <-c.Cursor():
			frames = append(frames, cur)
		case ce := <-c.ChildExit():
			frames = append(frames, ce)
		case <-c.Topology():
		case <-c.Title():
		case <-c.Bell():
		case <-c.ScrollbackAppend():
		case <-c.ScrollbackCleared():
		case <-c.Errors():
		case <-c.Closed():
			return frames
		case <-deadline:
			return frames
		}
	}
	return frames
}
