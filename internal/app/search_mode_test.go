package app

import (
	"testing"

	"github.com/LXXero/xerotty/internal/protocol"
)

type stubSearcher struct{ onDaemon bool }

func (stubSearcher) RequestScrollbackSearch(string, bool, bool, bool) uint64 { return 1 }
func (stubSearcher) TakeSearchResults() (uint64, []protocol.SearchMatch, bool, bool) {
	return 0, nil, false, false
}
func (s stubSearcher) SearchesOnDaemon() bool { return s.onDaemon }

// Regression: every daemon Source implements the daemon-search
// methods, so the old type assertion sent NON-windowed remote tabs to
// the daemon too. Their ScrollbackLen is the client mirror's length,
// not the daemon's depth, so the daemon's absolute match rows were
// converted against the wrong total and highlights/jumps landed on the
// wrong lines. Only windowed tabs may use the daemon searcher.
func TestDaemonSearcherForOnlyWindowed(t *testing.T) {
	if _, ok := daemonSearcherFor(stubSearcher{onDaemon: true}); !ok {
		t.Fatal("windowed daemon tab must search on the daemon")
	}
	if _, ok := daemonSearcherFor(stubSearcher{onDaemon: false}); ok {
		t.Fatal("non-windowed daemon tab must be searched locally")
	}
	if _, ok := daemonSearcherFor(struct{}{}); ok {
		t.Fatal("in-process terminal must be searched locally")
	}
}
