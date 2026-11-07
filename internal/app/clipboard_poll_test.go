package app

import (
	"errors"
	"testing"
	"time"
)

func TestClipboardMirrorCapsFailedReadsPerGeneration(t *testing.T) {
	gen := uint64(7)
	attempts := 0
	readErr := errors.New("clipboard owner did not deliver")
	a := &App{
		remoteHubs: map[string]*remoteHubEntry{"remote": {}},
	}
	generation := func() (uint64, bool) { return gen, true }
	readQuick := func() (string, error) {
		attempts++
		return "", readErr
	}
	poll := func() {
		t.Helper()
		a.lastClipboardPoll = time.Time{} // bypass the one-second production gate
		a.pollClipboardForDaemonsWith(generation, readQuick)
	}

	for attempt := 1; attempt <= clipboardReadFailureLimit; attempt++ {
		poll()
		if attempts != attempt {
			t.Fatalf("attempts after failure %d = %d", attempt, attempts)
		}
	}
	if a.lastClipboardGen != gen {
		t.Fatalf("failed generation = %d, want exhausted generation %d", a.lastClipboardGen, gen)
	}
	poll()
	if attempts != clipboardReadFailureLimit {
		t.Fatalf("read retried exhausted generation: attempts = %d", attempts)
	}

	gen++ // a new owner gets a fresh retry budget
	poll()
	if attempts != clipboardReadFailureLimit+1 {
		t.Fatalf("new generation attempts = %d, want %d", attempts, clipboardReadFailureLimit+1)
	}
	readErr = nil
	poll()
	if attempts != clipboardReadFailureLimit+2 {
		t.Fatalf("transient retry attempts = %d, want %d", attempts, clipboardReadFailureLimit+2)
	}
	if a.lastClipboardGen != gen {
		t.Fatalf("successful generation = %d, want %d", a.lastClipboardGen, gen)
	}
}
