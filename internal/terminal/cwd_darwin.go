package terminal

/*
#include <libproc.h>
#include <string.h>

// xt_proc_cwd fills buf with pid's current working directory via
// PROC_PIDVNODEPATHINFO — the same kernel source `lsof -d cwd` reads,
// minus the part where lsof forks, initializes its device cache, and
// crawls the system-wide fd table on every invocation. The daemon
// asks for every tab's cwd on a 750ms state tick, so this MUST be a
// syscall, not a subprocess: the lsof version showed up in the wild
// as "xerotty serve fires lsof every second, ~50k fd events each".
static int xt_proc_cwd(int pid, char *buf, int buflen) {
    struct proc_vnodepathinfo vpi;
    int n = proc_pidinfo(pid, PROC_PIDVNODEPATHINFO, 0, &vpi, sizeof(vpi));
    if (n <= 0) return 0;
    strncpy(buf, vpi.pvi_cdir.vip_path, buflen - 1);
    buf[buflen - 1] = 0;
    return 1;
}
*/
import "C"
import "unsafe"

// processCWD looks up the working directory of pid via libproc —
// one proc_pidinfo syscall. See xt_proc_cwd.
func processCWD(pid int) string {
	if pid <= 0 {
		return ""
	}
	buf := make([]byte, 1024)
	if C.xt_proc_cwd(C.int(pid), (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf))) == 0 {
		return ""
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n])
}
