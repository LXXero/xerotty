package terminal

import (
	"testing"
	"time"
)

// The reader fires the state-change hook while holding the emulator
// lock, and resize holds mu while it waits for the emulator lock. If
// the hook needed mu, a mode change during a resize deadlocked the tab
// (and then the whole daemon queued behind it). The hook must fire
// while mu is held elsewhere.
func TestStateChangedDoesNotTakeMu(t *testing.T) {
	term := &Terminal{}
	fired := make(chan struct{}, 1)
	term.SetOnStateChange(func() { fired <- struct{}{} })

	term.mu.Lock()
	defer term.mu.Unlock()

	done := make(chan struct{})
	go func() {
		term.stateChanged()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stateChanged blocked on mu")
	}
	select {
	case <-fired:
	default:
		t.Fatal("hook did not fire")
	}
}

func TestSetOnStateChangeNilClearsHook(t *testing.T) {
	term := &Terminal{}
	term.SetOnStateChange(func() { t.Fatal("cleared hook fired") })
	term.SetOnStateChange(nil)
	term.stateChanged()
}
