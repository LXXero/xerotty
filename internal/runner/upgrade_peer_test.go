package runner

import (
	"errors"
	"os"
	"testing"
)

func fakeProcs(m map[int]procInfo) func(int) (procInfo, error) {
	return func(pid int) (procInfo, error) {
		pi, ok := m[pid]
		if !ok {
			return procInfo{}, errors.New("no such process")
		}
		return pi, nil
	}
}

func TestSocketOwner(t *testing.T) {
	procs := fakeProcs(map[int]procInfo{
		// Supervisor: what SO_PEERCRED names on Linux.
		1304: {ppid: 900, argv: []string{"/Applications/xerotty.app/Contents/MacOS/xerotty", "serve"}},
		// Its daemon child: what LOCAL_PEERPID names on macOS.
		6675: {ppid: 1304, argv: []string{"/Applications/xerotty.app/Contents/MacOS/xerotty", "serve", "--child", "--control-fd", "3"}},
		// A daemon child after an in-place upgrade.
		6676: {ppid: 1304, argv: []string{"xerotty", "serve", "--resume", "/tmp/x/xerottyd.handoff", "--socket", "/tmp/x/sock", "--child", "--control-fd", "3"}},
		// An unsupervised daemon.
		700: {ppid: 1, argv: []string{"xerotty", "serve", "--no-supervisor"}},
		// A daemon child whose supervisor died.
		701: {ppid: 1, argv: []string{"xerotty", "serve", "--child", "--control-fd", "3"}},
		// --child given to some other subcommand.
		702: {ppid: 1304, argv: []string{"xerotty", "connect", "--child"}},
	})
	for _, tc := range []struct {
		name               string
		peer               int
		wantOwner, wantKid int
	}{
		{"supervisor peer (Linux)", 1304, 1304, 0},
		{"daemon child peer (macOS)", 6675, 1304, 6675},
		{"daemon child after in-place exec", 6676, 1304, 6676},
		{"unsupervised daemon", 700, 700, 0},
		{"orphaned daemon child", 701, 701, 0},
		{"not a serve child", 702, 702, 0},
		{"peer info unavailable", 999, 999, 0},
	} {
		owner, child := socketOwner(tc.peer, procs)
		if owner != tc.wantOwner || child != tc.wantKid {
			t.Errorf("%s: socketOwner(%d) = (%d, %d), want (%d, %d)", tc.name, tc.peer, owner, child, tc.wantOwner, tc.wantKid)
		}
	}
}

func TestProcInfoOfSelf(t *testing.T) {
	pi, err := procInfoOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if pi.ppid != os.Getppid() {
		t.Errorf("ppid = %d, want %d", pi.ppid, os.Getppid())
	}
	if len(pi.argv) == 0 {
		t.Errorf("empty argv")
	}
}

func TestLsofTxt(t *testing.T) {
	const exe = "/Applications/xerotty.app/Contents/MacOS/xerotty"
	// `lsof -w -a -p 6675 -d txt -F Din` after an install replaced the
	// binary and the daemon re-executed it.
	out := []byte("p6675\nftxt\nD0x1000010\ni4242\nn" + exe + "\nftxt\nD0x1000010\ni77\nn/usr/lib/dyld\n")
	for _, tc := range []struct {
		name string
		id   fileID
		want bool
	}{
		{"new inode, same device", fileID{dev: 0x1000010, ino: 4242, path: exe}, true},
		{"new inode, other device, same path", fileID{dev: 5, ino: 4242, path: exe}, true},
		{"old inode", fileID{dev: 0x1000010, ino: 4241, path: exe}, false},
		{"same inode number on another device and path", fileID{dev: 5, ino: 4242, path: "/usr/local/bin/xerotty"}, false},
	} {
		ok, mapped := lsofTxt(out, tc.id)
		if ok != tc.want {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.want)
		}
		if mapped != exe {
			t.Errorf("%s: mapped = %q, want %q", tc.name, mapped, exe)
		}
	}
	if ok, mapped := lsofTxt(nil, fileID{ino: 1}); ok || mapped != "" {
		t.Errorf("empty output: (%v, %q)", ok, mapped)
	}
}
