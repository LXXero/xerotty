package terminal

import (
	"os/exec"
	"strconv"
	"strings"
)

// processCWD looks up the working directory of pid via lsof, which ships
// with macOS by default. The -F n format prints one record per line:
// `p<pid>` then `n<path>`. We pull the first n-line.
func processCWD(pid int) string {
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-F", "n").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}
