package supervise

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
)

// Client is the daemon child's end of the control channel.
type Client struct {
	mu    sync.Mutex // serializes Send
	conn  *Conn
	exits chan ExitMsg
	done  chan struct{}

	upgrades    chan UpgradeMsg
	supUpgrades atomic.Bool
}

// NewClient wraps the inherited control fd and starts reading exit
// notices from the supervisor.
func NewClient(f *os.File) (*Client, error) {
	conn, err := Wrap(f)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, exits: make(chan ExitMsg, 64), done: make(chan struct{}), upgrades: make(chan UpgradeMsg, 1)}
	go c.readLoop()
	return c, nil
}

// Exits delivers shell exits the supervisor observed. Only tabs the
// daemon did not spawn itself need these; the channel closes when
// the supervisor goes away.
func (c *Client) Exits() <-chan ExitMsg { return c.exits }

func (c *Client) readLoop() {
	defer close(c.exits)
	for {
		kind, payload, files, err := c.conn.Recv()
		closeAll(files)
		if err != nil {
			return
		}
		switch kind {
		case KindExit:
			var m ExitMsg
			if json.Unmarshal(payload, &m) == nil {
				select {
				case c.exits <- m:
				default: // a wedged consumer must not stall the control channel
				}
			}
		case KindCaps:
			var m CapsMsg
			if json.Unmarshal(payload, &m) == nil {
				c.supUpgrades.Store(m.Upgrade)
			}
		case KindUpgrade:
			var m UpgradeMsg
			if json.Unmarshal(payload, &m) == nil && m.Handoff != "" {
				select {
				case c.upgrades <- m:
				default: // one is already pending; it does the same work
				}
			}
		}
	}
}

// UpgradeRequests delivers the supervisor's requests to hand off for
// an upgrade.
func (c *Client) UpgradeRequests() <-chan UpgradeMsg { return c.upgrades }

// SupervisorUpgrades reports whether the supervisor said it handles
// SIGUSR2 upgrades itself (CapsMsg).
func (c *Client) SupervisorUpgrades() bool { return c.supUpgrades.Load() }

// SendTab hands the supervisor a tab's plumbing. fds[0] must be the
// PTY master; fds[1], if present, the disk scrollback file. The
// supervisor receives its own copies.
func (c *Client) SendTab(id uint32, pid int, fds ...int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.SendJSON(KindTab, TabMsg{ID: id, PID: pid}, fds...)
}

// SendTabGone tells the supervisor to drop a tab's copies.
func (c *Client) SendTabGone(id uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.SendJSON(KindTabGone, TabGoneMsg{ID: id})
}

// SendState ships the latest handoff.State (msgpack, no fds or
// screens) — what the supervisor will resume from.
func (c *Client) SendState(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Send(KindState, b)
}

// Close ends the channel.
func (c *Client) Close() error {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return c.conn.Close()
}

var _ io.Closer = (*Client)(nil)

// ExecFD returns a duplicate of the control socket with FD_CLOEXEC
// cleared, for an exec-in-place upgrade to pass as --control-fd. The
// caller owns the number from here on.
func (c *Client) ExecFD() (int, error) {
	f, err := c.conn.uc.File()
	if err != nil {
		return -1, err
	}
	fd, err := syscall.Dup(int(f.Fd()))
	_ = f.Close()
	if err != nil {
		return -1, err
	}
	return fd, nil
}
