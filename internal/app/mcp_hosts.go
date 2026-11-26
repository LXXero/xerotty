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

	"github.com/LXXero/xerotty/internal/guimcp"
	"github.com/LXXero/xerotty/internal/platform"
)

// The GUI MCP server's host tools. attach_host connects the GUI to a
// host's daemon the same way the Remote menu's Reattach does: the
// connect dialog shows the dial, the host's existing tabs are adopted
// into the active window, and later tabs on it appear through the
// usual topology path. list_hosts reports the hosts the GUI knows of.
//
// Dialing and adopting tabs belong to the render thread, while MCP
// requests arrive on the server's goroutines, so each request hands
// its work to the render thread through postMain and waits for the
// answer.

// attachHostTimeout bounds how long attach_host waits for ssh. The
// dial carries on past it; the connect dialog still shows it and a
// later attach_host call waits on the same dial.
const attachHostTimeout = 60 * time.Second

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

// hostAttached reports whether the GUI has a hub for host. "" and
// "local" name the default hub.
func (a *App) hostAttached(host string) bool {
	if host == "" || host == "local" {
		return true
	}
	_, ok := a.hubsByName()[host]
	return ok
}

// AttachHost implements guimcp.Backend: connect the GUI to host's
// daemon unless it already is. host is anything the connect dialog
// accepts: a [[hosts]] name, an ~/.ssh/config alias, or an ssh
// destination. Runs on a guimcp goroutine.
func (a *App) AttachHost(ctx context.Context, host string) (bool, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return false, errors.New("host is required")
	}
	if a.hostAttached(host) {
		return true, nil
	}
	type result struct {
		already bool
		err     error
	}
	res := make(chan result, 1)
	a.postMain(func() {
		a.attachHostMain(host, func(already bool, err error) {
			res <- result{already, err}
		})
	})
	timer := time.NewTimer(attachHostTimeout)
	defer timer.Stop()
	select {
	case r := <-res:
		return r.already, r.err
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, fmt.Errorf("still connecting to %s after %s; the GUI's connect dialog shows the dial, and calling attach_host again waits for it", host, attachHostTimeout)
	}
}

// attachHostMain is the render-thread half of AttachHost. done runs
// once, on the render thread.
func (a *App) attachHostMain(host string, done func(already bool, err error)) {
	if a.hostAttached(host) {
		done(true, nil)
		return
	}
	w := a.attachWindow()
	if w == nil {
		done(false, errors.New("the GUI has no open window"))
		return
	}
	w.withRemoteHubThen(host, func(e *remoteHubEntry) error {
		return w.adoptRemoteWindows(e, host, false)
	}, func(err error) {
		done(false, err)
	})
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
// file. Patterns (with *, ? or !) are skipped, since they name no
// single host; Include and Match are not followed.
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
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		// ssh_config allows "Host=alias" as well as "Host alias".
		line = strings.Replace(strings.TrimSpace(line), "=", " ", 1)
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "host") {
			continue
		}
		for _, h := range fields[1:] {
			if strings.ContainsAny(h, "*?!") || seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}
