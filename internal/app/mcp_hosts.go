package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/guimcp"
	"github.com/LXXero/xerotty/internal/platform"
)

// The GUI MCP server's host tools. attach_host connects the GUI to a
// host's daemon the same way the Remote menu's Reattach does: the same
// dial, the host's existing tabs adopted into the active window, and
// later tabs on it appearing through the usual topology path. It
// shows no connect dialog: that dialog holds the keyboard of the
// window it is in, which an agent's attach must not take from the
// user, and its errors go back to the agent instead (and are not logged:
// this runs on the render thread, which must not block on stderr). list_hosts
// reports the hosts the GUI knows of.
//
// Dialing and adopting tabs belong to the render thread, while MCP
// requests arrive on the server's goroutines, so each request hands
// its work to the render thread through postMain and waits for the
// answer.

// attachHostTimeout bounds how long attach_host waits. It is longer
// than remoteHandshakeTimeout, so a dial that stalls normally fails
// with its own error first; past it the request is abandoned, and a
// dial nobody else is waiting for is cancelled.
var attachHostTimeout = 60 * time.Second

// postMain queues fn to run on the render thread at the start of the
// next frame and wakes the loop.
func (a *App) postMain(fn func()) {
	a.mainCallsMu.Lock()
	a.mainCalls = append(a.mainCalls, fn)
	a.mainCallsMu.Unlock()
	platform.PostWake()
}

// drainMainCalls runs the functions postMain queued. Render thread
// only.
func (a *App) drainMainCalls() {
	a.mainCallsMu.Lock()
	calls := a.mainCalls
	a.mainCalls = nil
	a.mainCallsMu.Unlock()
	for _, fn := range calls {
		fn()
	}
}

// hostAttached reports whether the GUI has a hub for host, under the
// same names list_tabs uses.
func (a *App) hostAttached(host string) bool {
	_, ok := a.hubsByName()[host]
	return ok
}

type attachResult struct {
	already bool
	err     error
}

// attachRequest is one attach_host call. The render thread owns
// every field; reply is sent on at most once.
type attachRequest struct {
	host      string
	reply     chan attachResult
	answered  bool
	abandoned bool
	dial      *remoteDial // the dial it waits on, nil when none
}

func (r *attachRequest) answer(already bool, err error) {
	if r.answered || r.abandoned {
		return
	}
	r.answered = true
	r.reply <- attachResult{already, err}
}

// AttachHost implements guimcp.Backend: connect the GUI to host's
// daemon unless it already is. host is a [[hosts]] name, a
// destination typed into the connect dialog this session, or an ssh
// destination that passes clientproto.ValidSSHDest. Runs on a guimcp
// goroutine.
func (a *App) AttachHost(ctx context.Context, host string) (bool, error) {
	if host == "" {
		return false, errors.New("host is required")
	}
	if a.hostAttached(host) {
		return true, nil
	}
	req := &attachRequest{host: host, reply: make(chan attachResult, 1)}
	a.postMain(func() { a.attachHostMain(req) })
	timer := time.NewTimer(attachHostTimeout)
	defer timer.Stop()
	var why error
	select {
	case r := <-req.reply:
		return r.already, r.err
	case <-ctx.Done():
		why = ctx.Err()
	case <-timer.C:
		why = fmt.Errorf("no answer from %s within %s; the attach was abandoned", host, attachHostTimeout)
	}
	// Abandon on the render thread, where the request's state lives.
	// A result that got there first wins, so a caller is never told
	// the attach failed when it went through. If the render thread
	// does not answer at all, the outcome is unknown, and the error
	// says so rather than claiming a failure.
	abandoned := make(chan struct{})
	a.postMain(func() {
		a.abandonAttach(req)
		close(abandoned)
	})
	stalled := time.NewTimer(attachAbandonWait)
	defer stalled.Stop()
	select {
	case r := <-req.reply:
		return r.already, r.err
	case <-abandoned:
		select {
		case r := <-req.reply:
			return r.already, r.err
		default:
		}
		return false, why
	case <-stalled.C:
		return false, fmt.Errorf("%v; the GUI did not answer within %s, so the attach may still complete (list_hosts shows whether it did)", why, attachAbandonWait)
	}
}

// attachAbandonWait bounds the wait for the render thread to abandon
// a request, so a GUI whose render thread has stopped cannot hold an
// MCP call forever.
var attachAbandonWait = 30 * time.Second

// attachHostMain is the render-thread half of AttachHost. It runs the
// dial the Remote menu's Reattach runs, without the connect dialog,
// and adopts the host's tabs when it completes.
func (a *App) attachHostMain(req *attachRequest) {
	host := req.host
	if a.hostAttached(host) {
		req.answer(true, nil)
		return
	}
	if err := a.checkAttachHost(host); err != nil {
		req.answer(false, err)
		return
	}
	w := a.attachWindow()
	if w == nil {
		req.answer(false, errors.New("the GUI has no open window"))
		return
	}
	a.remoteHubAsync(host, func(e *remoteHubEntry, err error) {
		if err != nil || req.abandoned {
			req.answer(false, err)
			return
		}
		// Adopt into the window that was active when the attach
		// started, as the menu adopts into the window it was used in;
		// if that window closed meanwhile, into the current one.
		target := w
		if !a.windowAlive(target) {
			target = a.attachWindow()
		}
		if target == nil {
			req.answer(false, nil)
			return
		}
		req.answer(false, target.adoptRemoteWindows(e, host, false))
	})
	if !req.answered {
		req.dial = a.remoteDials[host]
	}
}

// checkAttachHost refuses a host an MCP client may not dial. Saved
// hosts ([[hosts]] entries and connect-dialog destinations) dial with
// their own settings; anything else goes to ssh as the destination,
// so it must pass ValidSSHDest. "local" never dials: it is the local
// daemon's namespace, and hostAttached already answered for it.
func (a *App) checkAttachHost(host string) error {
	if host == "local" {
		_, name := a.getDaemonHub()
		return fmt.Errorf("no local daemon hub (this GUI's default hub is %q)", name)
	}
	for _, h := range a.cfg.Hosts {
		if h.Name == host {
			return nil
		}
	}
	if _, ok := a.adhocHosts[host]; ok {
		return nil
	}
	return clientproto.ValidSSHDest(host)
}

// abandonAttach gives up req on the render thread. Its dial is
// cancelled when nobody else waits for it: no manual request from the
// menu and no other live attach_host call.
func (a *App) abandonAttach(req *attachRequest) {
	if req.answered || req.abandoned {
		return
	}
	req.abandoned = true
	d := req.dial
	if d == nil || a.remoteDials[req.host] != d {
		return
	}
	d.mcpAbandoned++
	if d.mcpAbandoned >= len(d.waiters) {
		a.cancelRemoteDial(req.host)
	}
}

// attachWindow is the window an MCP attach adopts tabs into: the
// active window, else the first open one.
func (a *App) attachWindow() *Window {
	if a.active != nil && a.windowAlive(a.active) {
		return a.active
	}
	for _, w := range a.windows {
		if a.windowAlive(w) {
			return w
		}
	}
	return nil
}

// ListHosts implements guimcp.Backend: the attached hubs, the
// [[hosts]] entries, the destinations typed into the connect dialog
// this session, and the Host aliases in ~/.ssh/config.
func (a *App) ListHosts(ctx context.Context) ([]guimcp.HostRef, error) {
	type known struct{ config, connect []string }
	ch := make(chan known, 1)
	a.postMain(func() {
		var k known
		for _, h := range a.cfg.Hosts {
			k.config = append(k.config, h.Name)
		}
		for name := range a.adhocHosts {
			k.connect = append(k.connect, name)
		}
		ch <- k
	})
	var k known
	select {
	case k = <-ch:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, errors.New("the GUI's render thread did not answer within 10s")
	}

	byName := map[string]*guimcp.HostRef{}
	add := func(name, from string) *guimcp.HostRef {
		r := byName[name]
		if r == nil {
			r = &guimcp.HostRef{Name: name}
			byName[name] = r
		}
		if from != "" {
			r.KnownFrom = append(r.KnownFrom, from)
		}
		return r
	}
	for name := range a.hubsByName() {
		add(name, "").Attached = true
	}
	for _, n := range k.config {
		add(n, "hosts")
	}
	for _, n := range k.connect {
		add(n, "connect")
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, n := range sshConfigHosts(filepath.Join(home, ".ssh", "config")) {
			add(n, "ssh_config")
		}
	}
	out := make([]guimcp.HostRef, 0, len(byName))
	for _, r := range byName {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// sshConfigHosts returns the literal Host aliases in an ssh_config
// file, unquoted the way ssh reads them. Patterns (with *, ? or !)
// are skipped, since they name no single host; Include and Match are
// not followed.
func sshConfigHosts(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		kw, args := sshConfigLine(sc.Text())
		if !strings.EqualFold(kw, "host") {
			continue
		}
		for _, h := range args {
			if h == "" || strings.ContainsAny(h, "*?!") || seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// sshConfigLine splits one ssh_config line into its keyword and
// arguments: the keyword ends at whitespace or "=" ("Host=a" and
// "Host = a" are both allowed), arguments are split on whitespace
// with double or single quotes grouping, and an unquoted "#" starts
// a comment.
func sshConfigLine(line string) (string, []string) {
	line = strings.TrimLeft(line, " \t")
	end := strings.IndexAny(line, " \t=")
	if end < 0 {
		return line, nil
	}
	kw := line[:end]
	rest := strings.TrimLeft(line[end:], " \t")
	rest = strings.TrimLeft(strings.TrimPrefix(rest, "="), " \t")
	if strings.HasPrefix(kw, "#") {
		return "", nil
	}
	var args []string
	var cur strings.Builder
	var quote rune // the open quote character, 0 outside quotes
	have := false
	for _, r := range rest {
		inQuote := quote != 0
		switch {
		case r == quote:
			quote = 0
			have = true
		case !inQuote && (r == '"' || r == '\''):
			quote = r
			have = true
		case !inQuote && r == '#':
			if have {
				args = append(args, cur.String())
			}
			return kw, args
		case !inQuote && (r == ' ' || r == '\t'):
			if have {
				args = append(args, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if have {
		args = append(args, cur.String())
	}
	return kw, args
}
