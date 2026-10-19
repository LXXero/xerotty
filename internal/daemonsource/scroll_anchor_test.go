package daemonsource

import (
	"fmt"
	"testing"

	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/scrollback"
)

// appendRows builds a ScrollbackAppend whose rows carry their absolute
// index as content, so a snapshot's rows are self-identifying.
func appendRows(base, n, total int) *protocol.ScrollbackAppend {
	rows := make([][]protocol.Cell, n)
	for i := range rows {
		rows[i] = []protocol.Cell{{Content: fmt.Sprintf("%d", base+i), Width: 1}}
	}
	return &protocol.ScrollbackAppend{
		BaseIdx: uint32(base),
		Rows:    rows,
		Total:   uint32(total),
	}
}

// TestSnapshotWindowAnchorCompensation reproduces the "scrolled view
// keeps bumping to the bottom while output streams" bug on remote
// daemon tabs: the frame's scroll-anchor pass patches Offset against
// the ScrollbackLen it sees at frame top, but the hub reader applies
// ScrollbackAppends concurrently (in bursts of up to 256 rows over a
// remote link). An append landing between the anchor pass and the
// snapshot used to shift the rendered window toward the live tail by
// exactly the growth. Passing asOfSbLen lets SnapshotWindow resolve
// the growth under its own lock, pinning the viewport to content.
func TestSnapshotWindowAnchorCompensation(t *testing.T) {
	s := &Source{windowed: true}
	s.applyScrollbackAppend(appendRows(0, 100, 100))

	// The frame's anchor pass: offset computed against sbLen=100.
	asOf := s.ScrollbackLen()
	if asOf != 100 {
		t.Fatalf("seed: ScrollbackLen = %d, want 100", asOf)
	}
	const offset, rows, cols = 50, 20, 1

	before, baseBefore, _ := s.SnapshotWindow(offset, asOf, rows, cols)

	// A 30-row burst lands mid-frame (reader goroutine), contiguous
	// with the live tail — total grows to 130.
	s.applyScrollbackAppend(appendRows(100, 30, 130))

	after, baseAfter, _ := s.SnapshotWindow(offset, asOf, rows, cols)
	if baseAfter != baseBefore {
		t.Fatalf("anchored base drifted: before=%d after=%d", baseBefore, baseAfter)
	}
	for r := range before {
		if before[r][0].Content != after[r][0].Content {
			t.Fatalf("row %d changed under anchor: %q -> %q",
				r, before[r][0].Content, after[r][0].Content)
		}
	}

	// Sanity: the uncompensated call (asOfSbLen=0, the old semantics)
	// exhibits the bug — the window rides the growth toward live.
	drifted, baseDrifted, _ := s.SnapshotWindow(offset, 0, rows, cols)
	if baseDrifted == baseBefore {
		t.Fatalf("expected uncompensated snapshot to drift; base stayed %d", baseBefore)
	}
	if drifted[0][0].Content == before[0][0].Content {
		t.Fatalf("expected uncompensated top row to differ from anchored %q",
			before[0][0].Content)
	}
}

// TestFullMirrorKeepsScrolledView reproduces "a scrolled-up remote tab
// keeps scrolling with new output" once the tab has more history than
// the local mirror holds (scrollback mode "memory", 10000 rows by
// default). At the cap every append drops as many old rows as it adds,
// so ScrollbackLen stays the same and an anchor measured by length
// never moves the offset: the view stays the same distance from the
// bottom while the lines under it shift, which looks exactly like
// following the output. The anchor has to measure rows pushed.
func TestFullMirrorKeepsScrolledView(t *testing.T) {
	const capRows, offset, rows, cols = 100, 50, 20, 1
	s := &Source{scrollbackCap: capRows}
	s.applyScrollbackAppend(appendRows(0, capRows, capRows))

	st := scrollback.New()
	st.OnOutput(s.ScrollbackPushed(), s.ScrollbackLen(), false)
	st.Offset = offset // the user scrolls up
	before, _, _ := s.SnapshotWindow(st.Offset, st.PrevPushed, rows, cols)

	// 30 rows arrive: the full mirror drops its 30 oldest rows.
	s.applyScrollbackAppend(appendRows(capRows, 30, capRows+30))
	if got := s.ScrollbackLen(); got != capRows {
		t.Fatalf("ScrollbackLen = %d, want the cap %d", got, capRows)
	}

	// Mid-frame, before the anchor pass sees the burst.
	mid, _, _ := s.SnapshotWindow(st.Offset, st.PrevPushed, rows, cols)
	// The next frame's anchor pass.
	st.OnOutput(s.ScrollbackPushed(), s.ScrollbackLen(), false)
	after, _, _ := s.SnapshotWindow(st.Offset, st.PrevPushed, rows, cols)

	for r := range before {
		if mid[r][0].Content != before[r][0].Content {
			t.Fatalf("mid-frame row %d moved: %q -> %q", r, before[r][0].Content, mid[r][0].Content)
		}
		if after[r][0].Content != before[r][0].Content {
			t.Fatalf("row %d moved after the anchor pass: %q -> %q (offset %d)",
				r, before[r][0].Content, after[r][0].Content, st.Offset)
		}
	}

	// Once the lines being read drop out of the mirror, the view stops
	// at the oldest row instead of running off the top.
	s.applyScrollbackAppend(appendRows(capRows+30, capRows, 2*capRows+30))
	st.OnOutput(s.ScrollbackPushed(), s.ScrollbackLen(), false)
	if st.Offset != capRows {
		t.Fatalf("offset = %d after the read lines dropped out, want the cap %d", st.Offset, capRows)
	}
}
