package daemonsource

import "testing"

// Daemon-side search returns the daemon's ABSOLUTE row indices, which
// only line up with the client's rows when ScrollbackLen is the
// daemon's true depth — i.e. a windowed Source. A non-windowed Source
// mirrors just the rows it received, so it must be searched locally.
func TestSearchesOnDaemonOnlyWhenWindowed(t *testing.T) {
	if !newWindowedSource().SearchesOnDaemon() {
		t.Fatal("windowed Source must search on the daemon")
	}
	capped := &Source{dataCh: make(chan struct{}, 1), scrollbackCap: 100}
	if capped.SearchesOnDaemon() {
		t.Fatal("non-windowed Source must not search on the daemon: " +
			"its ScrollbackLen is the client mirror's length, not the daemon's depth")
	}
}
