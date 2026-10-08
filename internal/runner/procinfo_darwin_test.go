//go:build darwin

package runner

import "testing"

func TestDaemonChildFromPS(t *testing.T) {
	// Real `ps -A -ww -o pid= -o ppid= -o args=` lines, including a
	// child that exec'd in place (--child after --resume) and the
	// supervisor itself, which must not match its own pid.
	const out = `    1     0 /sbin/launchd
 1304     1 xerotty serve --resume /tmp/xerotty-501/xerottyd.handoff --socket /tmp/xerotty-501/xerottyd.sock --mcp-socket /tmp/xerotty-501/xerottyd.mcp.sock
 7000  1304 /bin/zsh -l
 9033  1304 /Applications/xerotty.app/Contents/MacOS/xerotty serve --resume /tmp/xerotty-501/xerottyd.handoff --socket /tmp/xerotty-501/xerottyd.sock --mcp-socket /tmp/xerotty-501/xerottyd.mcp.sock --child --control-fd 37
 9100  4242 /usr/local/bin/xerotty serve --child --control-fd 3
`
	if got := daemonChildFromPS(out, 1304); got != 9033 {
		t.Fatalf("daemonChildFromPS(1304) = %d, want 9033", got)
	}
	if got := daemonChildFromPS(out, 4242); got != 9100 {
		t.Fatalf("daemonChildFromPS(4242) = %d, want 9100", got)
	}
	if got := daemonChildFromPS(out, 1); got != 0 {
		t.Fatalf("daemonChildFromPS(1) = %d, want 0 (the supervisor is not a --child)", got)
	}
	if got := daemonChildFromPS("", 1304); got != 0 {
		t.Fatalf("empty ps output = %d, want 0", got)
	}
}
