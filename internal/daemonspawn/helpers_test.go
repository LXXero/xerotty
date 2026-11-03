package daemonspawn

import (
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// dialer is the Options.Dial probe for a unix socket.
func dialer(sock string) func() bool {
	return func() bool {
		c, err := net.Dial("unix", sock)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// sessionID asks ps for pid's session id: portable across Linux and
// macOS, which is more than package syscall offers for getsid.
func sessionID(t *testing.T, pid int) int {
	t.Helper()
	out, err := exec.Command("ps", "-o", "sid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	sid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("ps sid %q: %v", out, err)
	}
	return sid
}
