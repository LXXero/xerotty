package terminal

import (
	"os"
	"strconv"
)

func processCWD(pid int) string {
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil {
		return ""
	}
	return cwd
}
