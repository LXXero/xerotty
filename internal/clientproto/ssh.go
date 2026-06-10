package clientproto

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LXXero/xerotty/internal/protocol"
)

// DialSSH spawns an SSH command that runs `xerotty serve --stdio`
// on the remote side and wraps the resulting stdin+stdout pipes
// as a Client. The remote daemon's stderr is forwarded to the
// parent's stderr so auth prompts + daemon log lines show up
// where the user can see them.
//
// xerotty is a single binary; `xerotty serve` is the daemon mode
// (no separate xerottyd executable). On the remote box you need
// the same `xerotty` binary on PATH (or pass a full path via
// daemonCmd).
//
// `sshDest` is whatever you'd pass to plain `ssh` — "user@host",
// "host", or a Host alias from ~/.ssh/config. Forwarding +
// auth + key selection are all delegated to ssh(1) — we don't
// reimplement any of that.
//
// `daemonCmd` is the remote command line that speaks the wire
// protocol over stdio. Default if empty: "xerotty serve --stdio".
// Use the full form when you need to point to a specific path,
// e.g. "/opt/xerotty/bin/xerotty serve --stdio".
//
// `extraSSHArgs` are inserted before `sshDest` for things like
// `-i identity` or `-p port`.
//
// Caller is responsible for calling Client.Close() to terminate.
// Closing the client closes the SSH stdin which signals the remote
// daemon to exit, which ends ssh(1), which fires our wait reaper.
func DialSSH(sshDest, daemonCmd string, extraSSHArgs []string) (*Client, error) {
	if daemonCmd == "" {
		daemonCmd = "xerotty serve --stdio"
	}
	args := append([]string{}, extraSSHArgs...)
	// Keepalive (Phase 10 layer 5): ask ssh to probe the server every
	// 10s and give up after 3 unanswered probes, so a dead SSH path
	// (peer powered off, NAT dropped the flow) is detected in ~30s and
	// ssh exits — which closes our stdio conn and triggers the Hub's
	// reconnect. This is a cheap EXTRA signal, not the correctness
	// mechanism: the app-level heartbeat (layer 4d) + bounded writers
	// (4a) are what actually guarantee detection (and they cover the
	// daemon-hung-but-SSH-alive case keepalive can't see). Placed AFTER
	// extraSSHArgs so a user-supplied -o ServerAlive* (which ssh honors
	// first-match) still wins.
	args = append(args, "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3")
	// -T disables pseudo-tty allocation for stdin/stdout, which is
	// critical: we're shipping binary msgpack frames and don't want
	// ssh's tty layer translating CR/LF on us.
	args = append(args, "-T", sshDest, daemonCmd)
	// GUI processes spawned by a compositor (labwc autostart, app
	// menus) often lack SSH_AUTH_SOCK, so their ssh child can't reach
	// the user's agent and dies with "Permission denied" — which used
	// to surface as an inscrutable handshake EOF. If the env doesn't
	// carry an agent but one lives at the standard systemd-user path,
	// hand it to ssh.
	var env []string
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		if rd := os.Getenv("XDG_RUNTIME_DIR"); rd != "" {
			if p := filepath.Join(rd, "ssh-agent.socket"); sockExists(p) {
				env = append(os.Environ(), "SSH_AUTH_SOCK="+p)
			}
		}
	}
	return dialCommandEnv(env, "ssh", args...)
}

func sockExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

// tailBuffer keeps the last few KB written to it — enough to carry
// ssh's parting words ("Permission denied (publickey)", host key
// complaints) into our error messages.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuffer) Tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// DialCommand spawns an arbitrary subprocess and treats its stdin +
// stdout as the protocol transport. The subprocess MUST speak the
// xerotty wire protocol over those streams — typical examples are
// `xerotty serve --stdio` (local subprocess daemon for testing) or
// `flatpak run io.xerotty.Daemon serve --stdio` (containerized).
//
// DialSSH is a thin convenience wrapper over this.
//
// stderr inherits from the parent so subprocess logs + interactive
// auth prompts (in the ssh case) reach the user. Closing the
// returned Client closes the subprocess's stdin, signaling EOF to
// the daemon, and reaps the process.
func DialCommand(name string, args ...string) (*Client, error) {
	return dialCommandEnv(nil, name, args...)
}

func dialCommandEnv(env []string, name string, args ...string) (*Client, error) {
	cmd := exec.Command(name, args...)
	if env != nil {
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%s stdin: %w", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("%s stdout: %w", name, err)
	}
	// Tee the transport's stderr: visible for CLI users, captured so
	// handshake failures can say WHY the transport died ("Permission
	// denied (publickey)") instead of a bare EOF.
	tail := &tailBuffer{}
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s start: %w", name, err)
	}

	conn := newSSHConn(stdout, stdin, cmd)
	c := wrap(conn)
	c.transportStderr = tail
	return c, nil
}

// sshConn is a net.Conn wrapper around an `ssh ... xerottyd --stdio`
// subprocess. Read = ssh stdout (daemon → client frames), Write =
// ssh stdin (client → daemon frames), Close = close stdin + reap.
//
// Mostly a thin pass-through on top of protocol.StdioConn; the extra
// state is the *exec.Cmd handle so Close can wait on the subprocess
// rather than leaving a zombie. We forward ssh's exit error to the
// client's ExitErr surface.
type sshConn struct {
	*protocol.StdioConn
	cmd *exec.Cmd

	once   sync.Once
	waitCh chan error
}

func newSSHConn(stdout io.ReadCloser, stdin io.WriteCloser, cmd *exec.Cmd) *sshConn {
	c := &sshConn{
		StdioConn: protocol.NewStdioConn(stdout, stdin),
		cmd:       cmd,
		waitCh:    make(chan error, 1),
	}
	// Reap in the background so Close can wait briefly without
	// hanging indefinitely on a wedged remote.
	go func() {
		c.waitCh <- cmd.Wait()
	}()
	return c
}

func (c *sshConn) Close() error {
	// Closing the write side flushes EOF to the remote daemon, which
	// will see it on its stdin reader and exit cleanly. Then ssh
	// exits when its stdio closes.
	err := c.StdioConn.Close()
	// Short wait — we don't want to hang forever if the remote is
	// stuck, but we do want a chance to reap normally.
	c.once.Do(func() {
		select {
		case <-c.waitCh:
		case <-time.After(2 * time.Second):
			// Force kill if the remote ignores EOF for too long.
			_ = c.cmd.Process.Kill()
			<-c.waitCh
		}
	})
	return err
}

// LocalAddr / RemoteAddr override stdio's so logs distinguish ssh
// from raw stdio.
func (c *sshConn) LocalAddr() net.Addr  { return sshAddr("local") }
func (c *sshConn) RemoteAddr() net.Addr { return sshAddr("remote") }

type sshAddr string

func (sshAddr) Network() string  { return "ssh" }
func (a sshAddr) String() string { return "ssh:" + string(a) }
