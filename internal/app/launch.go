package app

import (
	"github.com/LXXero/xerotty/internal/launchipc"
	"github.com/LXXero/xerotty/internal/platform"
	"github.com/LXXero/xerotty/internal/terminal"
)

// This file is the GUI side of single-instance launching: a later
// plain `xerotty` run in the same session forwards "open a window/tab
// in your process" over the launch socket (internal/launchipc) instead
// of spawning a second GUI attached to the same daemon session.

// enqueueLaunch is the launchipc.Listen callback. It runs on the
// socket's accept goroutine, so it must not touch window/tab/ImGui
// state — it only queues the request for the frame loop.
func (a *App) enqueueLaunch(req launchipc.Request) {
	a.launchMu.Lock()
	a.launchQueue = append(a.launchQueue, req)
	a.launchMu.Unlock()
	// The main loop may be parked in WaitEventTimeout; nudge it so the
	// forwarded window/tab appears immediately, not at the next input.
	platform.PostWake()
}

// drainLaunchRequests applies queued forwarded launches. Called once
// per frame from the main loop — window/tab creation touches ImGui and
// SDL state that only the frame loop may own.
func (a *App) drainLaunchRequests() {
	a.launchMu.Lock()
	reqs := a.launchQueue
	a.launchQueue = nil
	a.launchMu.Unlock()
	if len(reqs) == 0 {
		return
	}
	// Too early: the first Window isn't renderable yet, so a spawn
	// would silently no-op and the request would be lost. Put the
	// batch back (ahead of anything that arrived meanwhile) and retry
	// on a later frame.
	if len(a.windows) == 0 || a.windows[0].renderer == nil {
		a.launchMu.Lock()
		a.launchQueue = append(reqs, a.launchQueue...)
		a.launchMu.Unlock()
		return
	}
	for _, req := range reqs {
		switch req.Action {
		case "tab":
			w := a.active
			if w == nil {
				w = a.windows[0]
			}
			w.openTabWithCWD(req.CWD)
		default: // "window" (Listen already filtered unknown actions)
			a.spawnCWD = req.CWD
			if len(req.Argv) > 0 {
				a.spawnCmd = &terminal.LaunchCmd{Argv: req.Argv, Shell: req.Shell}
			}
			a.spawnWindow()
			a.spawnCWD = ""
			a.spawnCmd = nil
		}
	}
}

// openTabWithCWD opens a new tab in this Window whose shell starts in
// cwd ("" = xerotty's own CWD). The "new_tab" UI action computes its
// cwd from InheritCWD instead; forwarded launches carry the INVOKING
// shell's directory, which is the directory the user meant.
func (w *Window) openTabWithCWD(cwd string) {
	cols, rows := w.gridSize()
	if tab, err := w.tabs.NewTab(cols, rows, cwd); err == nil && tab != nil {
		// Same 1→2 tab-bar transition quirk as the "new_tab" action:
		// AutoSelectNewTabs can't catch the new tab when the bar first
		// appears, so request the switch explicitly.
		w.tabSwitchReq = tab.ID
	}
}
