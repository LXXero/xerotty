package terminal

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
)

// TestEmulatorPanicIsContained: a panic while feeding the emulator
// must not unwind the process. The reader recovers, resets the
// emulator, and keeps ingesting; the shell is untouched. Regression
// for the 2026-10-06 daemon death, where one tab's stale scroll
// margins took every session with it.
func TestEmulatorPanicIsContained(t *testing.T) {
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	term, err := New(&cfg, 80, 24, "")
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}
	defer term.Close()

	var fired atomic.Int32
	term.publishMu.Lock()
	term.ingestHook = func([]byte) {
		if fired.CompareAndSwap(0, 1) {
			panic("injected emulator panic")
		}
	}
	term.publishMu.Unlock()

	// The echo of this line is the chunk that panics (and is lost).
	if _, err := term.Write([]byte("true\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Fatal("hook never ran")
	}

	// Everything after the panic must flow again, through the same
	// emulator, with the same shell.
	term.Write([]byte("printf 'AFTER_%s\\n' PANIC\r"))
	waitFor(t, term, "AFTER_PANIC")
	term.mu.Lock()
	exited, panics := term.childExited, term.emuPanics
	term.mu.Unlock()
	if exited {
		t.Fatal("shell died with the contained panic")
	}
	if panics != 1 {
		t.Fatalf("emuPanics = %d, want 1", panics)
	}
}

// TestEmulatorPanicBudgetClosesTab: a stream that keeps tripping the
// emulator closes its own tab instead of looping — and only its own.
func TestEmulatorPanicBudgetClosesTab(t *testing.T) {
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	term, err := New(&cfg, 80, 24, "")
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}
	defer term.Close()

	term.publishMu.Lock()
	term.ingestHook = func([]byte) { panic("poison stream") }
	term.publishMu.Unlock()

	// Each line's echo is one more panic; the budget closes the tab.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, _ = term.Write([]byte("true\r"))
		term.mu.Lock()
		exited := term.childExited
		term.mu.Unlock()
		if exited {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	term.mu.Lock()
	exited, panics := term.childExited, term.emuPanics
	term.mu.Unlock()
	if !exited {
		t.Fatalf("tab not closed after %d panics", panics)
	}
	if panics < maxEmulatorPanics {
		t.Fatalf("closed after %d panics, budget is %d", panics, maxEmulatorPanics)
	}
}
