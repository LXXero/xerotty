package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/LXXero/xerotty/internal/tabevents"
)

// Events implements `xerotty events`: print tab events (title and
// status-glyph changes, bell, OSC 9/777/99 notifications, tab
// created / closed / renamed, child exit) as one JSON object per
// line. With --follow it keeps printing as events happen until
// killed, so a background task (Claude Code's Monitor tool, a shell
// `while read`) wakes on each line instead of polling screens.
//
// It is a client of the wait_for_events MCP tool: the GUI aggregator
// (every host the GUI is attached to) when one is running, else the
// local daemon's own MCP socket. Discovery is the same as `xerotty
// mcp` (see mcpCandidates). With --follow the connection is redialed
// when it drops (GUI restart, daemon upgrade); the per-host cursor is
// kept across redials, so a GUI restart loses nothing and a daemon
// restart is reported as a `missed` line.
func Events(args []string) int {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	var o eventsOpts
	var socketPath string
	var daemonOnly bool
	var hosts, kinds, tabsFlag listFlag
	fs.BoolVar(&o.follow, "follow", false, "keep printing events as they happen until killed")
	fs.BoolVar(&o.follow, "f", false, "shorthand for --follow")
	fs.BoolVar(&o.fromNow, "new", false, "skip the events the daemons already hold; print only ones that happen from now on (use with --follow)")
	fs.Var(&hosts, "host", "only events from this host namespace, as in list_tabs (\"local\", \"kh\"); repeatable or comma-separated")
	fs.Var(&kinds, "kind", "only events of this kind ("+kindNames()+"); repeatable or comma-separated")
	fs.Var(&tabsFlag, "tab", "only events for this tab: a namespaced id like local:3 (a bare number means a local tab); repeatable or comma-separated")
	fs.StringVar(&socketPath, "socket", "", "MCP socket to read from (default: discover the GUI aggregator, then the local daemon)")
	fs.BoolVar(&daemonOnly, "daemon", false, "read the local daemon's MCP socket only, skipping the GUI aggregator")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), eventsHelp)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "xerotty events: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	o.hosts = hosts
	o.kinds = kinds
	if _, err := tabevents.ParseKinds(o.kinds); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty events: %v\n", err)
		return 2
	}
	for _, t := range tabsFlag {
		ns, err := normalizeTabID(t)
		if err != nil {
			fmt.Fprintf(os.Stderr, "xerotty events: %v\n", err)
			return 2
		}
		o.tabs = append(o.tabs, ns)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dial := func() (*eventsConn, error) {
		var lastErr error = errors.New("no xerotty MCP socket found (is the GUI or `xerotty serve` running?)")
		for _, c := range mcpCandidates(socketPath, daemonOnly) {
			ec, err := dialEvents(c.path)
			if err == nil {
				return ec, nil
			}
			lastErr = fmt.Errorf("%s: %w", c.path, err)
		}
		return nil, lastErr
	}
	err := runEvents(ctx, o, dial, os.Stdout, os.Stderr)
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "xerotty events: %v\n", err)
		return 1
	}
	return 0
}

const eventsHelp = `xerotty events — print tab events as JSON lines.

USAGE
  xerotty events [--follow] [--new] [--host H] [--kind K] [--tab ID]

Prints one JSON object per line. Without --follow it prints the
events the daemons still hold and exits. With --follow it then keeps
printing new ones until killed (Ctrl-C, SIGTERM, or the reader
closing the pipe); --new skips the held ones.

Each event line has host, tab_id (namespaced, e.g. "local:3"), seq
(per host), time, kind and the kind's fields, the same as the
wait_for_events MCP tool (see docs/MCP.md). Two more kinds of line
can appear:
  {"kind":"events_missed","host":H,"reason":"restarted"|"overflow"}
      events from host H were lost: its daemon restarted (sequence
      reset) or more happened than its ring holds since the last read.
  {"kind":"host_error","host":H,"error":"..."}
      host H could not be read (printed again only when it changes).

Reads the GUI aggregator (all hosts) when the GUI is running, else
the local daemon. --host other than "local" needs the GUI.

EXAMPLES
  Wake a Claude Code session when an agent in any tab goes idle or
  rings the bell (Monitor tool / background Bash):
    xerotty events --follow --new --kind title_changed,bell,notification

  What happened in the last while:
    xerotty events | jq -c 'select(.kind=="child_exited")'

FLAGS
`

// eventsOpts is what `xerotty events` was asked for.
type eventsOpts struct {
	follow  bool
	fromNow bool
	hosts   []string
	kinds   []string
	tabs    []string // namespaced
}

// followWait is how long each wait_for_events call blocks in
// --follow mode. Any value under the tool's 5-minute cap works; a
// shorter one only bounds how long a dead connection goes unnoticed
// when nothing else would fail it.
const followWait = 60 * time.Second

// redialBackoff bounds the wait between redial attempts in --follow
// mode.
var redialBackoff = []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 5 * time.Second}

// cliEvent is one printed line: the same shape wait_for_events uses
// on the GUI aggregator (host + namespaced tab_id + the daemon's
// event), so a line means the same thing whichever socket it came
// from.
type cliEvent struct {
	Host  string `json:"host"`
	TabID string `json:"tab_id"`
	tabevents.Event
}

// noticeLine is an events_missed or host_error line.
type noticeLine struct {
	Kind   string `json:"kind"`
	Host   string `json:"host"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// eventsCursor is the per-host read position, the aggregator's cursor
// shape. A daemon socket's position is kept under localHost so it
// survives switching between the daemon and the GUI on redial.
type eventsCursor map[string]hostCursor

type hostCursor struct {
	Epoch string `json:"epoch"`
	Seq   uint64 `json:"seq"`
}

const localHost = "local"

// eventsBatch is one wait_for_events reply, normalized across the two
// server flavors.
type eventsBatch struct {
	Events []cliEvent
	Cursor eventsCursor
	Missed map[string]string
	Errors map[string]string
}

// runEvents is the body of `xerotty events`, with the socket dial
// and the outputs injected for tests.
func runEvents(ctx context.Context, o eventsOpts, dial func() (*eventsConn, error), stdout, stderr io.Writer) error {
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	cursor := eventsCursor{}
	lastErrs := map[string]string{}
	started := false // the first read (which --new discards) is done

	var conn *eventsConn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		if conn == nil {
			c, err := dial()
			if err != nil {
				if !o.follow {
					return err
				}
				if attempt == 0 {
					fmt.Fprintf(stderr, "xerotty events: %v; retrying\n", err)
				}
				if !sleepCtx(ctx, redialBackoff[min(attempt, len(redialBackoff)-1)]) {
					return nil
				}
				attempt++
				continue
			}
			if attempt > 0 {
				fmt.Fprintf(stderr, "xerotty events: connected to %s\n", c.path)
			}
			conn, attempt = c, 0
			// Being killed closes the socket, which ends a blocked read.
			raw := conn.conn
			conn.stopOnCancel = context.AfterFunc(ctx, func() { _ = raw.Close() })
		}

		timeout := followWait
		if !o.follow || !started {
			timeout = 0
		}
		b, err := conn.wait(ctx, o, cursor, timeout)
		if err != nil {
			var bad errBadRequest
			if errors.As(err, &bad) || !o.follow {
				return err
			}
			if ctx.Err() == nil {
				fmt.Fprintf(stderr, "xerotty events: connection lost (%v); reconnecting\n", err)
			}
			conn.Close()
			conn = nil
			continue
		}
		cursor = b.Cursor

		if started || !o.fromNow {
			for _, h := range sortedKeys(b.Missed) {
				if err := enc.Encode(noticeLine{Kind: "events_missed", Host: h, Reason: b.Missed[h]}); err != nil {
					return nil // reader went away
				}
			}
			for _, e := range b.Events {
				if err := enc.Encode(e); err != nil {
					return nil
				}
			}
		}
		for _, h := range sortedKeys(b.Errors) {
			if lastErrs[h] != b.Errors[h] {
				if err := enc.Encode(noticeLine{Kind: "host_error", Host: h, Error: b.Errors[h]}); err != nil {
					return nil
				}
			}
		}
		lastErrs = b.Errors
		started = true
		if !o.follow {
			return nil
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// errBadRequest is a JSON-RPC error reply: the request itself is
// wrong (unknown kind, bad tab id), so retrying won't help.
type errBadRequest struct{ msg string }

func (e errBadRequest) Error() string { return e.msg }

// eventsConn is one connection to an MCP socket.
type eventsConn struct {
	path string
	conn net.Conn
	sc   *bufio.Scanner
	gui  bool // the GUI aggregator (namespaced ids, all hosts)
	id   int

	stopOnCancel func() bool
}

// dialEvents connects to an MCP socket and runs the initialize
// handshake, whose serverInfo name tells the GUI aggregator
// ("xerotty-gui") from a single daemon.
func dialEvents(path string) (*eventsConn, error) {
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	c := &eventsConn{path: path, conn: conn, sc: sc}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	res, err := c.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "xerotty-events"},
	})
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return nil, err
	}
	var info struct {
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	_ = json.Unmarshal(res, &info)
	c.gui = info.ServerInfo.Name == "xerotty-gui"
	return c, nil
}

func (c *eventsConn) Close() {
	if c.stopOnCancel != nil {
		c.stopOnCancel()
	}
	_ = c.conn.Close()
}

// call sends one JSON-RPC request and returns its result.
func (c *eventsConn) call(method string, params any) (json.RawMessage, error) {
	c.id++
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	want := strconv.Itoa(c.id)
	for c.sc.Scan() {
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(c.sc.Bytes(), &resp) != nil || string(resp.ID) != want {
			continue // a notification, or a reply to something else
		}
		if resp.Error != nil {
			return nil, errBadRequest{fmt.Sprintf("%s: %s", method, resp.Error.Message)}
		}
		return resp.Result, nil
	}
	if err := c.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// wait runs one wait_for_events call from cursor, through the
// aggregator's or the daemon's native method.
func (c *eventsConn) wait(ctx context.Context, o eventsOpts, cursor eventsCursor, timeout time.Duration) (eventsBatch, error) {
	ms := timeout.Milliseconds()
	if c.gui {
		params := map[string]any{"timeout_ms": ms}
		if len(cursor) > 0 {
			params["cursor"] = cursor
		}
		if len(o.hosts) > 0 {
			params["hosts"] = o.hosts
		}
		if len(o.kinds) > 0 {
			params["kinds"] = o.kinds
		}
		if len(o.tabs) > 0 {
			params["tab_ids"] = o.tabs
		}
		res, err := c.call("wait_for_events", params)
		if err != nil {
			return eventsBatch{}, err
		}
		var r struct {
			Events []cliEvent        `json:"events"`
			Cursor eventsCursor      `json:"cursor"`
			Missed map[string]string `json:"missed"`
			Errors map[string]string `json:"errors"`
		}
		if err := json.Unmarshal(res, &r); err != nil {
			return eventsBatch{}, fmt.Errorf("wait_for_events: bad reply: %w", err)
		}
		if r.Cursor == nil {
			r.Cursor = eventsCursor{}
		}
		return eventsBatch{Events: r.Events, Cursor: r.Cursor, Missed: r.Missed, Errors: r.Errors}, nil
	}

	// A single daemon: only its own tabs, reported as host "local".
	// Hosts other than local are reported, not waited on.
	b := eventsBatch{Cursor: eventsCursor{}}
	for h, hc := range cursor {
		b.Cursor[h] = hc
	}
	wantLocal := len(o.hosts) == 0
	for _, h := range o.hosts {
		if h == localHost {
			wantLocal = true
		} else {
			if b.Errors == nil {
				b.Errors = map[string]string{}
			}
			b.Errors[h] = "not reachable from the local daemon's socket; remote hosts need the GUI running"
		}
	}
	var tabIDs []uint32
	for _, ns := range o.tabs {
		host, id, _ := splitTabID(ns)
		if host == localHost {
			tabIDs = append(tabIDs, id)
		}
	}
	if len(o.tabs) > 0 && len(tabIDs) == 0 {
		wantLocal = false // every --tab is on another host
	}
	if !wantLocal {
		// Nothing to wait on; don't let --follow spin.
		sleepCtx(ctx, timeout)
		return b, nil
	}
	lc := cursor[localHost]
	params := map[string]any{"after_seq": lc.Seq, "epoch": lc.Epoch, "timeout_ms": ms}
	if len(o.kinds) > 0 {
		params["kinds"] = o.kinds
	}
	if len(tabIDs) > 0 {
		params["tab_ids"] = tabIDs
	}
	res, err := c.call("events/wait", params)
	if err != nil {
		return eventsBatch{}, err
	}
	var r struct {
		Events       []tabevents.Event `json:"events"`
		LastSeq      uint64            `json:"last_seq"`
		Epoch        string            `json:"epoch"`
		MissedReason string            `json:"missed_reason"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return eventsBatch{}, fmt.Errorf("events/wait: bad reply: %w", err)
	}
	for _, e := range r.Events {
		b.Events = append(b.Events, cliEvent{Host: localHost, TabID: localHost + ":" + strconv.FormatUint(uint64(e.Tab), 10), Event: e})
	}
	b.Cursor[localHost] = hostCursor{r.Epoch, r.LastSeq}
	if r.MissedReason != "" {
		b.Missed = map[string]string{localHost: r.MissedReason}
	}
	return b, nil
}

// normalizeTabID turns a --tab value into a namespaced id: "kh:3"
// stays, a bare "3" means local tab 3.
func normalizeTabID(s string) (string, error) {
	if _, err := strconv.ParseUint(s, 10, 32); err == nil {
		return localHost + ":" + s, nil
	}
	if _, _, ok := splitTabID(s); ok {
		return s, nil
	}
	return "", fmt.Errorf("--tab %q is not a tab id like local:3 or 3", s)
}

// splitTabID parses "<host>:<tabid>", splitting on the last colon (a
// host key may itself contain one, e.g. "user@host:2222").
func splitTabID(ns string) (string, uint32, bool) {
	i := strings.LastIndex(ns, ":")
	if i <= 0 {
		return "", 0, false
	}
	id, err := strconv.ParseUint(ns[i+1:], 10, 32)
	if err != nil {
		return "", 0, false
	}
	return ns[:i], uint32(id), true
}

func kindNames() string {
	names := make([]string, len(tabevents.AllKinds))
	for i, k := range tabevents.AllKinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// listFlag is a repeatable, comma-separated string flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}
