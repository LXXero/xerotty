package terminal

import (
	"os"
	"strconv"
	"strings"
)

// processName returns the executable name for pid by reading
// /proc/<pid>/comm. The file holds the process's argv[0] basename
// (truncated to 15 bytes by the kernel — fine for terminal display).
// Returns "" if /proc/<pid>/comm doesn't exist or can't be read.
func processName(pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
