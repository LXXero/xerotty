package terminal

/*
#include <libproc.h>
#include <string.h>
#include <sys/sysctl.h>

// xt_proc_argv0 fetches argv[0] for pid via KERN_PROCARGS2.
// Layout of the sysctl buffer: int argc, exec_path\0, NUL padding,
// argv[0]\0, argv[1]\0, ... Returns the argv[0] length, 0 on any
// failure. Needs the same or lower privilege as proc_name for
// processes we own (which PTY children are).
static int xt_proc_argv0(int pid, char* out, unsigned int outlen) {
    int mib[3] = {CTL_KERN, KERN_PROCARGS2, pid};
    char buf[4096];
    size_t size = sizeof(buf);
    if (sysctl(mib, 3, buf, &size, NULL, 0) != 0) return 0;
    if (size <= sizeof(int)) return 0;
    char* p = buf + sizeof(int);
    char* end = buf + size;
    while (p < end && *p != '\0') p++; // skip exec_path
    while (p < end && *p == '\0') p++; // skip alignment padding
    char* a0 = p;
    while (p < end && *p != '\0') p++;
    unsigned int n = (unsigned int)(p - a0);
    if (n == 0 || n >= outlen) return 0;
    memcpy(out, a0, n);
    out[n] = '\0';
    return (int)n;
}
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
//
// proc_name reports the kernel's p_comm: the basename of the file
// that was exec'd — NOT argv[0]. Version-managed tools exec
// version-named binaries (Claude Code's real executable is literally
// ~/.local/share/claude/versions/2.1.172), so tab titles fell back
// to bare version numbers after the ps -> libproc switch (`ps -o
// comm=` had been showing argv[0] all along). When the kernel name
// looks like a version string, resolve argv[0] instead — that's the
// symlink the user actually invoked ("claude").
func processName(pid int) string {
	if pid <= 0 {
		return ""
	}
	buf := make([]byte, 256)
	n := C.proc_name(C.int(pid), unsafe.Pointer(&buf[0]), C.uint32_t(len(buf)))
	if n <= 0 {
		return ""
	}
	name := basename(string(buf[:n]))
	if looksLikeVersion(name) {
		abuf := make([]byte, 1024)
		if an := C.xt_proc_argv0(C.int(pid), (*C.char)(unsafe.Pointer(&abuf[0])),
			C.uint(len(abuf))); an > 0 {
			if a0 := basename(string(abuf[:an])); a0 != "" && !looksLikeVersion(a0) {
				return a0
			}
		}
	}
	return name
}

func basename(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// looksLikeVersion reports whether s is composed solely of digits
// and dots (e.g. "2.1.172") — a kernel process name that's useless
// as a tab title.
func looksLikeVersion(s string) bool {
	if s == "" {
		return false
	}
	hasDigit := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '.':
		default:
			return false
		}
	}
	return hasDigit
}
