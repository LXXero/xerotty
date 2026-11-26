package clientproto

import (
	"slices"
	"testing"
)

// The destination always follows "--", so ssh never reads it as an
// option, and the user's own options stay in front.
func TestSSHArgsEndOptionsBeforeDest(t *testing.T) {
	args := sshArgs("-oProxyCommand=touch x", "", []string{"-p", "2222"})
	i := slices.Index(args, "--")
	if i < 0 || i+2 != len(args)-1 || args[i+1] != "-oProxyCommand=touch x" || args[i+2] != "xerotty serve --stdio" {
		t.Fatalf("sshArgs = %q; want ... -- <dest> <cmd>", args)
	}
	if args[0] != "-p" || args[1] != "2222" {
		t.Fatalf("extra ssh args not first: %q", args)
	}
}

func TestValidSSHDest(t *testing.T) {
	for _, ok := range []string{"xhtpc", "xero@xhtpc", "xhtpc:2222", "kh.zaxxon.cc", "192.168.1.1", "[::1]:22", "a-b_c"} {
		if err := ValidSSHDest(ok); err != nil {
			t.Errorf("ValidSSHDest(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"", "-oProxyCommand=x", "-p", "a b", "a\tb", "x=y", "h;id", "h$(id)", "h`id`",
		"h|id", "h&", "h>f", "'h'", "a@b@c", "user@", "user@-x", "ssh://h", "h/x", "h%d", "h\n"} {
		if err := ValidSSHDest(bad); err == nil {
			t.Errorf("ValidSSHDest(%q) accepted", bad)
		}
	}
}
