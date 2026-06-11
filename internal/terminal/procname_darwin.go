package terminal

/*
#include <libproc.h>
*/
import "C"
import (
	"strings"
	"unsafe"
)

// processName returns the executable name for pid via libproc's
// proc_name — one syscall. The previous implementation forked
// `ps -o comm=` per call; the daemon's 750ms per-tab state tick and
// the GUI's title polling both land here, so subprocesses are off
// the table (same storm as the lsof cwd lookup, see cwd_darwin.go).
func processName(pid int) string {
	if pid <= 0 {
		return ""
	}
	buf := make([]byte, 256)
	n := C.proc_name(C.int(pid), unsafe.Pointer(&buf[0]), C.uint32_t(len(buf)))
	if n <= 0 {
		return ""
	}
	name := string(buf[:n])
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name
}
