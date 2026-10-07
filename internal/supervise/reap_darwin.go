package supervise

import (
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// macOS has no child subreaper: shells orphaned by a daemon death
// re-parent to launchd, which reaps them. Exits are observed with
// kqueue EVFILT_PROC instead, which works for any pid.

var (
	kqOnce sync.Once
	kq     int
	kqErr  error
	kqMu   sync.Mutex
	kqSink func(ExitMsg)
)

func platformInit() error {
	return nil
}

// watchPID registers pid for exit notification. The first call
// starts the kqueue loop. Registration does not depend on a sink
// being set: the notice is delivered to whatever sink is installed
// when the process exits.
func watchPID(pid int) {
	kqOnce.Do(func() {
		kq, kqErr = unix.Kqueue()
		if kqErr == nil {
			go kqLoop()
		}
	})
	if kqErr != nil {
		return
	}
	ev := unix.Kevent_t{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT | unix.NOTE_EXITSTATUS,
	}
	_, _ = unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil)
}

func kqLoop() {
	events := make([]unix.Kevent_t, 16)
	for {
		n, err := unix.Kevent(kq, nil, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return
		}
		for _, ev := range events[:n] {
			if ev.Filter != unix.EVFILT_PROC || ev.Fflags&unix.NOTE_EXIT == 0 {
				continue
			}
			ws := syscall.WaitStatus(ev.Data)
			kqMu.Lock()
			sink := kqSink
			kqMu.Unlock()
			if sink != nil {
				sink(ExitMsg{PID: int(ev.Ident), Code: exitCode(ws)})
			}
		}
	}
}

// setExitSink receives kqueue exit notices.
func setExitSink(fn func(ExitMsg)) {
	kqMu.Lock()
	kqSink = fn
	kqMu.Unlock()
}
