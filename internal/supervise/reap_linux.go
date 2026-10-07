package supervise

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// platformInit makes the supervisor a child subreaper: when the
// daemon dies, its shells re-parent to us instead of init, so reapAll
// still collects their exit status and nothing turns into a zombie.
func platformInit() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("supervise: PR_SET_CHILD_SUBREAPER: %w (shell exits after a daemon death will not be reported)", err)
	}
	return nil
}

// watchPID is a no-op on Linux: re-parented shells reach wait4.
func watchPID(int) {}

// setExitSink is a no-op on Linux: reapAll feeds shell exits directly.
func setExitSink(func(ExitMsg)) {}
