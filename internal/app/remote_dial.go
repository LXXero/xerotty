package app

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/platform"
	"github.com/LXXero/xerotty/internal/protocol"
)

// Dialing a remote host runs ssh, and ssh can sit for a long time
// before it answers: name resolution with the network down, a TCP
// connect to a host that drops the packets, an auth prompt. None of
// that may run on the render thread. A GUI that stops reading its
// display connection is not just frozen: the compositor drops a
// client whose event buffer fills up, SDL turns the dropped
// connection into a quit event, and the GUI exits without a word.
// That is what "xerotty crashed" looked like when the laptop lost
// Wi-Fi during a connect.
//
// So a dial runs in its own goroutine and its result is applied on
// the render thread by pollRemoteDials, once per frame. Code that
// needs a host's hub asks remoteHubAsync and gets a callback, on the
// render thread, when the hub exists or the dial failed. The connect
// dialog shows the dial in progress and offers Cancel; cancelling
// closes the half-made connection, which also kills the ssh child.

// errRemoteDialCancelled is what waiters hear when the user cancels
// a dial.
var errRemoteDialCancelled = errors.New("connection cancelled")

type remoteDialResult struct {
	cli      *clientproto.Client
	attached *protocol.Attached
	err      error
}

// remoteDial is one in-flight ssh dial to a host that has no hub
// yet. The render thread owns every field but done, which the dial
// goroutine sends on exactly once.
type remoteDial struct {
	name      string
	done      chan remoteDialResult
	cancel    func()
	cancelled bool
	waiters   []func(*remoteHubEntry, error)
}

// remoteDialFunc performs the dial off the render thread. cancel is
// closed when the user gives up; the dial must then fail promptly
// and leave no connection behind, or return one that the caller
// closes.
type remoteDialFunc func(name string, host config.RemoteHost, cancel <-chan struct{}) (*clientproto.Client, *protocol.Attached, error)

// lookupRemoteHub returns the registered hub for name, or nil.
func (a *App) lookupRemoteHub(name string) *remoteHubEntry {
	a.remoteHubsMu.Lock()
	defer a.remoteHubsMu.Unlock()
	return a.remoteHubs[name]
}

// remoteHubAsync delivers name's hub to then: at once when the hub
// already exists, otherwise from pollRemoteDials when the dial
// finishes or fails. then always runs on the render thread. One dial
// serves every caller that asks for the host while it is in flight.
func (a *App) remoteHubAsync(name string, then func(*remoteHubEntry, error)) {
	if e := a.lookupRemoteHub(name); e != nil {
		then(e, nil)
		return
	}
	if d, ok := a.remoteDials[name]; ok {
		d.waiters = append(d.waiters, then)
		return
	}
	host, ok := a.resolveRemote(name)
	if !ok {
		then(nil, fmt.Errorf("remote host %q: empty destination", name))
		return
	}
	cancelCh := make(chan struct{})
	d := &remoteDial{
		name:    name,
		done:    make(chan remoteDialResult, 1),
		cancel:  sync.OnceFunc(func() { close(cancelCh) }),
		waiters: []func(*remoteHubEntry, error){then},
	}
	if a.remoteDials == nil {
		a.remoteDials = make(map[string]*remoteDial)
	}
	a.remoteDials[name] = d
	dial := a.remoteDialer
	if dial == nil {
		dial = a.dialRemoteDaemon
	}
	go func() {
		cli, att, err := dial(name, host, cancelCh)
		d.done <- remoteDialResult{cli: cli, attached: att, err: err}
		// The render loop parks between events; wake it so the
		// result is applied now, not at the next keypress.
		platform.PostWake()
	}()
}

// cancelRemoteDial abandons the dial to name. Waiters hear
// errRemoteDialCancelled now. The dial goroutine still reports back
// later; pollRemoteDials drains that report and closes a connection
// that completed anyway.
func (a *App) cancelRemoteDial(name string) {
	d, ok := a.remoteDials[name]
	if !ok {
		return
	}
	delete(a.remoteDials, name)
	d.cancelled = true
	d.cancel()
	a.remoteDialsDraining = append(a.remoteDialsDraining, d)
	waiters := d.waiters
	d.waiters = nil
	for _, fn := range waiters {
		fn(nil, errRemoteDialCancelled)
	}
}

// pollRemoteDials applies the dials that finished since the last
// frame. Runs on the render thread once per frame and never blocks.
func (a *App) pollRemoteDials() {
	for name, d := range a.remoteDials {
		var res remoteDialResult
		select {
		case res = <-d.done:
		default:
			continue
		}
		delete(a.remoteDials, name)
		waiters := d.waiters
		d.waiters = nil
		if res.err != nil {
			for _, fn := range waiters {
				fn(nil, res.err)
			}
			continue
		}
		entry := a.registerRemoteHub(name, res.cli, res.attached)
		for _, fn := range waiters {
			fn(entry, nil)
		}
	}
	if len(a.remoteDialsDraining) == 0 {
		return
	}
	keep := a.remoteDialsDraining[:0]
	for _, d := range a.remoteDialsDraining {
		select {
		case res := <-d.done:
			if res.cli != nil {
				_ = res.cli.Close()
			}
		default:
			keep = append(keep, d)
		}
	}
	a.remoteDialsDraining = keep
}

// registerRemoteHub wraps a connected, attached client in a Hub and
// enters it in the registry under name. The Attached snapshot's
// windows become the entry's reattach queue, grouped by daemon-side
// window so a reattach restores the user's remote layout. If a hub
// for name was registered meanwhile, the new connection is closed
// and the existing entry returned.
func (a *App) registerRemoteHub(name string, cli *clientproto.Client, attached *protocol.Attached) *remoteHubEntry {
	if e := a.lookupRemoteHub(name); e != nil {
		_ = cli.Close()
		return e
	}
	host, _ := a.resolveRemote(name)
	hub := daemonsource.NewHub(cli)
	// Self-heal over SSH. Capture the resolved host (NOT a re-resolve
	// inside the closure) so the reconnect dialer never touches
	// a.adhocHosts off the main goroutine — SSH params don't change
	// mid-session anyway.
	hub.SetRedial(func() (*clientproto.Client, *protocol.Attached, error) {
		return a.dialRemoteDaemon(name, host, nil)
	})
	hub.SeedRevision(attached.Revision)
	hub.SeedInstance(attached.InstanceID)
	a.wireHubCallbacks(name, hub)

	entry := &remoteHubEntry{hub: hub}
	tabByID := make(map[uint32]daemonTabSnapshot, len(attached.Tabs))
	for _, ti := range attached.Tabs {
		tabByID[ti.ID] = daemonTabSnapshot{
			ID:    ti.ID,
			Title: ti.Title,
			Cols:  ti.Cols,
			Rows:  ti.Rows,
		}
	}
	for _, wi := range attached.Windows {
		snap := daemonWindowSnapshot{
			WindowID:     wi.ID,
			PosX:         wi.PosX,
			PosY:         wi.PosY,
			Width:        wi.Width,
			Height:       wi.Height,
			FocusedTabID: wi.FocusedTabID,
		}
		for _, tid := range wi.TabIDs {
			if t, ok := tabByID[tid]; ok {
				snap.Tabs = append(snap.Tabs, t)
				delete(tabByID, tid)
			}
		}
		if len(snap.Tabs) > 0 {
			entry.reattachQueue = append(entry.reattachQueue, snap)
		}
	}
	if len(tabByID) > 0 {
		// Sweep orphans (tabs not in any window) into a synthetic
		// snapshot so they're not silently dropped.
		var orphans []daemonTabSnapshot
		for _, t := range tabByID {
			orphans = append(orphans, t)
		}
		entry.reattachQueue = append(entry.reattachQueue, daemonWindowSnapshot{Tabs: orphans})
	}

	a.remoteHubsMu.Lock()
	if a.remoteHubs == nil {
		a.remoteHubs = make(map[string]*remoteHubEntry)
	}
	a.remoteHubs[name] = entry
	a.remoteHubsMu.Unlock()
	return entry
}

// windowAlive reports whether w is still one of the app's open
// windows. A dial's callback checks this: the user may have closed
// the window while ssh was still connecting.
func (a *App) windowAlive(w *Window) bool {
	for _, x := range a.windows {
		if x == w {
			return !w.pendingClose
		}
	}
	return false
}

// withRemoteHub runs use with hostName's hub once it exists. When the
// host is not connected yet, the connect dialog shows the dial in
// progress with a Cancel button, and use runs on the render thread
// when the dial completes, provided this window is still open. A
// failure to connect, or a failure from use, is shown in the dialog.
func (w *Window) withRemoteHub(hostName string, use func(*remoteHubEntry) error) {
	a := w.app
	if e := a.lookupRemoteHub(hostName); e != nil {
		if err := use(e); err != nil {
			w.remoteOpenFailed(hostName, err)
		}
		return
	}
	w.showConnecting(hostName)
	a.remoteHubAsync(hostName, func(e *remoteHubEntry, err error) {
		if !a.windowAlive(w) {
			return
		}
		if err != nil {
			w.connectFailed(hostName, err)
			return
		}
		w.connectDone(hostName)
		if err := use(e); err != nil {
			w.remoteOpenFailed(hostName, err)
		}
	})
}

// showConnecting puts the connect dialog in its in-progress state
// for host.
func (w *Window) showConnecting(host string) {
	w.connectingHost = true
	w.connectPending = true
	w.connectTarget = host
	w.connectBuffer = host
	w.connectError = ""
	w.app.active = w
}

// connectDone closes the dialog if it is still showing host's dial.
func (w *Window) connectDone(host string) {
	if w.connectPending && w.connectTarget == host {
		w.connectingHost = false
		w.connectPending = false
		w.connectError = ""
	}
}

// connectFailed reports a failed dial to host. A cancelled dial just
// closes the dialog; any other failure keeps it open with the error
// so the destination or ssh options can be corrected.
func (w *Window) connectFailed(host string, err error) {
	fmt.Fprintf(os.Stderr, "xerotty: connect %s: %v\n", host, err)
	if w.connectTarget != host {
		return // a newer dial replaced this one in the dialog
	}
	w.connectPending = false
	if errors.Is(err, errRemoteDialCancelled) {
		w.connectingHost = false
		w.connectError = ""
		return
	}
	w.connectingHost = true
	w.connectError = err.Error()
}

// remoteOpenFailed reports a failure that happened after the host
// was connected (a tab or window create the daemon refused, or a
// link that dropped meanwhile). Shown in the connect dialog so it is
// not just a line on stderr.
func (w *Window) remoteOpenFailed(host string, err error) {
	fmt.Fprintf(os.Stderr, "xerotty: %s: %v\n", host, err)
	w.connectingHost = true
	w.connectPending = false
	w.connectTarget = host
	w.connectBuffer = host
	w.connectError = err.Error()
	w.app.active = w
}
