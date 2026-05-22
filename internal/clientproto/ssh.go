package clientproto

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
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
	// -T disables pseudo-tty allocation for stdin/stdout, which is
	// critical: we're shipping binary msgpack frames and don't want
	// ssh's tty layer translating CR/LF on us.
	args = append(args, "-T", sshDest, daemonCmd)
	return DialCommand("ssh", args...)
}

// DialCommand spawns an arbitrary subprocess and treats its stdin +
// stdout as the protocol transport. The subprocess MUST speak the
// xerotty wire protocol over those streams — typical examples are
// `xerottyd --stdio` (local subprocess daemon for testing) or
// `flatpak run io.xerotty.Daemon --stdio` (containerized variant).
//
// DialSSH is a thin convenience wrapper over this.
//
// stderr inherits from the parent so subprocess logs + interactive
// auth prompts (in the ssh case) reach the user. Closing the
// returned Client closes the subprocess's stdin, signaling EOF to
// the daemon, and reaps the process.
func DialCommand(name string, args ...string) (*Client, error) {
	cmd := exec.Command(name, args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%s stdin: %w", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("%s stdout: %w", name, err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s start: %w", name, err)
	}

	conn := newSSHConn(stdout, stdin, cmd)
	return wrap(conn), nil
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

