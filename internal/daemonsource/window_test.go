package daemonsource

import (
	"runtime"
	"testing"

	"github.com/LXXero/xerotty/internal/protocol"
)

func wRow(n int) []protocol.Cell {
	row := make([]protocol.Cell, 80)
	for i := range row {
		row[i] = protocol.Cell{Content: "x", Width: 1}
	}
	return row
}

// newWindowedSource builds a windowed Source with no hub (so
// EnsureScrollbackWindow records the requested range but sends
// nothing — see the nil-hub guard).
func newWindowedSource() *Source {
	return &Source{dataCh: make(chan struct{}, 1), windowed: true, scrollbackCap: 10000}
}

// streamAppends feeds `total` rows in batches of 256, like the daemon
// publish loop, always contiguous at the live end.
func streamAppends(s *Source, total int) {
	base := 0
	for base < total {
		n := 256
		if base+n > total {
			n = total - base
		}
		rows := make([][]protocol.Cell, n)
		for i := range rows {
			rows[i] = wRow(base + i)
		}
		s.applyScrollbackAppend(&protocol.ScrollbackAppend{
			BaseIdx: uint32(base), Rows: rows, Total: uint32(base + n),
		})
		select {
		case <-s.dataCh:
		default:
		}
		base += n
	}
}

// Windowed mode must report the daemon's TRUE total via ScrollbackLen
// while holding only a bounded window in memory.
func TestWindowedReportsTotalBoundsMemory(t *testing.T) {
	s := newWindowedSource()
	const total = 500_000 // a full mirror would be ~9 GB

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	streamAppends(s, total)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	if got := s.ScrollbackLen(); got != total {
		t.Fatalf("ScrollbackLen = %d, want %d (true daemon total)", got, total)
	}
	if rows := len(s.scrollback); rows > scrollbackWindowCap {
		t.Fatalf("window holds %d rows, want <= cap %d", rows, scrollbackWindowCap)
	}
	// The whole point: 500k rows streamed, but heap growth is the
	// window (~8000 rows), not the history.
	growthMB := (int64(after.HeapInuse) - int64(base.HeapInuse)) / 1024 / 1024
	if growthMB > 80 {
		t.Fatalf("heap grew %d MB streaming 500k rows — window not bounding memory?", growthMB)
	}

	// Live-end rows are cached; cold history reads nil until fetched.
	if c := s.ScrollbackCellAt(0, total-1); c == nil {
		t.Fatal("newest row should be in the live-anchored window")
	}
	if c := s.ScrollbackCellAt(0, 10); c != nil {
		t.Fatal("cold row far below the window should read nil (needs fetch)")
	}
}

// Scrolling into cold history must request a window around it; the
// range reply must install and become readable.
func TestWindowedFetchOnScroll(t *testing.T) {
	s := newWindowedSource()
	const total = 500_000
	streamAppends(s, total)

	// Viewport wants rows around absolute 100_000 (deep cold history).
	s.EnsureScrollbackWindow(100_000, 100_050)
	if s.reqTo <= s.reqFrom {
		t.Fatal("EnsureScrollbackWindow did not record a fetch range")
	}
	if !(s.reqFrom <= 100_000 && s.reqTo >= 100_050) {
		t.Fatalf("requested range [%d,%d) does not cover the visible [100000,100050)", s.reqFrom, s.reqTo)
	}
	if s.reqTo-s.reqFrom > scrollbackWindowCap {
		t.Fatalf("requested span %d exceeds window cap %d", s.reqTo-s.reqFrom, scrollbackWindowCap)
	}

	// A second identical Ensure while the request is in flight must
	// NOT issue another fetch (debounce).
	from0, to0 := s.reqFrom, s.reqTo
	s.EnsureScrollbackWindow(100_000, 100_050)
	if s.reqFrom != from0 || s.reqTo != to0 {
		t.Fatal("duplicate Ensure re-requested an in-flight range")
	}

	// Daemon answers; the rows install and become readable.
	from := s.reqFrom
	n := s.reqTo - from
	rows := make([][]protocol.Cell, n)
	for i := range rows {
		rows[i] = wRow(from + i)
	}
	s.applyScrollbackRange(&protocol.ScrollbackRange{From: uint32(from), Rows: rows})

	if c := s.ScrollbackCellAt(0, 100_000); c == nil {
		t.Fatal("fetched cold row should now be readable")
	}
	if got := s.ScrollbackLen(); got != total {
		t.Fatalf("ScrollbackLen = %d, want %d after fetch", got, total)
	}
}

// Prefetch must fire BEFORE the viewport leaves the cached window, and
// the fetched chunk must MERGE (extend) the window rather than replace
// it — so scrolling across the old edge stays continuous (no blank) —
// while the window is still trimmed back to the cap (no growth/leak).
func TestWindowedPrefetchMergesBounded(t *testing.T) {
	defer func(c, p, f int) { scrollbackWindowCap, scrollbackPrefetch, scrollbackFetchSpan = c, p, f }(scrollbackWindowCap, scrollbackPrefetch, scrollbackFetchSpan)
	scrollbackWindowCap, scrollbackPrefetch, scrollbackFetchSpan = 100, 20, 50

	s := newWindowedSource()
	streamAppends(s, 500) // live-anchored window = last 100 rows: [400,500)

	if s.ScrollbackCellAt(0, 405) == nil {
		t.Fatal("expected last 100 rows cached after streaming")
	}
	// Viewport sits near the OLD (bottom) edge of the window — within
	// the prefetch margin — but still fully cached.
	s.EnsureScrollbackWindow(405, 415)
	if !(s.reqFrom == 350 && s.reqTo == 400) {
		t.Fatalf("prefetch should request the adjacent [350,400); got [%d,%d)", s.reqFrom, s.reqTo)
	}

	// Daemon answers the prefetch; rows must merge in.
	from, n := s.reqFrom, s.reqTo-s.reqFrom
	rows := make([][]protocol.Cell, n)
	for i := range rows {
		rows[i] = wRow(from + i)
	}
	s.applyScrollbackRange(&protocol.ScrollbackRange{From: uint32(from), Rows: rows})

	// Continuity: the row that was on screen is STILL readable...
	if s.ScrollbackCellAt(0, 405) == nil {
		t.Fatal("previously-visible row blanked after a prefetch merge")
	}
	// ...AND newly-fetched older rows are now available (runway).
	if s.ScrollbackCellAt(0, 365) == nil {
		t.Fatal("prefetched older row not available after merge")
	}
	// Hard cap holds — merge did not grow memory.
	if rows := len(s.scrollback); rows > scrollbackWindowCap {
		t.Fatalf("window grew to %d rows after merge, cap is %d", rows, scrollbackWindowCap)
	}
}
