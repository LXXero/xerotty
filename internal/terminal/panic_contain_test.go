package terminal

import (
	"sync"
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

// TestResizePanicIsContained: a panic inside the emulator's resize
// must not unwind the caller. The first one resets the emulator and
// the tab keeps working with neither mu nor publishMu left held; past
// the budget the tab is closed and OnChildExit fires, which is what
// the daemon relays to clients as MsgChildExit.
func TestResizePanicIsContained(t *testing.T) {
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	term, err := New(&cfg, 80, 24, "")
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}
	defer term.Close()

	exited := make(chan int, 1)
	term.SetOnChildExit(func(code int) { exited <- code })

	var fired atomic.Int32
	term.mu.Lock()
	term.resizeHook = func(int, int) {
		if fired.Add(1) == 1 {
			panic("injected resize panic")
		}
	}
	term.mu.Unlock()

	term.Resize(100, 30)
	// The reader takes both locks briefly, so "not left held" means
	// lockable soon, not lockable right now.
	lockSoon(t, &term.mu, "mu")
	panics := term.emuPanics
	term.mu.Unlock()
	lockSoon(t, &term.publishMu, "publishMu")
	term.publishMu.Unlock()
	if panics != 1 {
		t.Fatalf("emuPanics = %d, want 1", panics)
	}
	if w, h := term.Emu.Width(), term.Emu.Height(); w != 100 || h != 30 {
		t.Fatalf("emulator is %dx%d after the retried resize, want 100x30", w, h)
	}
	term.Write([]byte("printf 'AFTER_%s\\n' RESIZE\r"))
	waitFor(t, term, "AFTER_RESIZE")

	// A resize that always panics uses up the budget and closes the tab.
	term.mu.Lock()
	term.resizeHook = func(int, int) { panic("poison resize") }
	term.mu.Unlock()
	term.Resize(90, 20)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("tab not reported gone after the resize panic budget")
	}
	term.mu.Lock()
	panics = term.emuPanics
	term.mu.Unlock()
	if panics != maxEmulatorPanics {
		t.Fatalf("emuPanics = %d, want %d", panics, maxEmulatorPanics)
	}
}

// lockSoon locks mu, failing the test if that takes longer than a
// contended lock ever should.
func lockSoon(t *testing.T, mu *sync.Mutex, name string) {
	t.Helper()
	locked := make(chan struct{})
	go func() { mu.Lock(); close(locked) }()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s left held after a contained resize panic", name)
	}
}
