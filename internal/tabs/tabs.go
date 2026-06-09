// Package tabs manages terminal tabs.
package tabs

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/terminal"
)

// atomicBool aliases sync/atomic.Bool so the Tab struct comment
// can refer to it without importing noise elsewhere.
type atomicBool = atomic.Bool

// Tab represents a single terminal tab.
//
// Title is set by the OSC 0/2 escape sequence (most shells set it
// from their prompt, claude/vim/etc set it as their UI title). When
// no OSC has fired yet, DisplayTitle() falls back to the PTY's
// foreground process name (vim, top, ssh) so the title bar / tab bar
// shows what's actually running — matching iTerm2/Terminal.app
// behaviour for apps that don't emit OSC themselves.
type Tab struct {
	ID int
	// Host is the named remote host this tab's PTY lives on (from
	// cfg.Hosts). Empty for local tabs (in-process PTY or local
	// daemon). The GUI uses this to render a host badge in the
	// tab title so users know which machine each tab is on.
	Host string
	// Terminal is the abstraction over the tab's PTY+grid. Default
	// impl is *terminal.Terminal (in-process PTY); when the daemon
	// arc is wired up this is a daemon-backed source instead. The
	// GUI only ever talks through this interface.
	Terminal terminal.Source
	Dirty    bool
	Closed   bool

	// title is the OSC-set title, set from the PTY/daemon callback
	// goroutine and read from the render thread — guarded by
	// titleMu. Accessed via Title() / SetTitle().
	titleMu sync.RWMutex
	title   string

	// bellPending is set (off the render thread) when the tab's
	// terminal rang the bell while not focused; the tab bar
	// renders an urgency marker (●) and clears it on focus.
	// atomic so the cross-goroutine set/read is safe.
	bellPending atomicBool

	// foregroundCache + foregroundAt throttle the per-tab PTY-pgid +
	// processName lookup so we don't fork `ps` on macOS every frame.
	// The cache is invalidated after foregroundCacheTTL.
	foregroundCache string
	foregroundAt    time.Time
}

const foregroundCacheTTL = 500 * time.Millisecond

// DisplayTitle returns the user-facing title for this tab — OSC-set
// title if the running app emitted one (priority), otherwise the
// foreground process name from the PTY (e.g. "vim", "top"), with a
// "shell" fallback when neither is available. Cached to throttle the
// foreground lookup (forks `ps` on macOS, cheap on Linux).
//
// Remote tabs (t.Host != "") get a "host: " prefix so the user can
// tell at a glance which machine the tab lives on.
func (t *Tab) DisplayTitle() string {
	base := t.titleBase()
	if t.Host != "" {
		return t.Host + ": " + base
	}
	return base
}

// Title returns the OSC-set title (thread-safe).
func (t *Tab) Title() string {
	t.titleMu.RLock()
	defer t.titleMu.RUnlock()
	return t.title
}

// SetTitle sets the OSC-set title (thread-safe; called from the
// PTY/daemon title callback on a non-render goroutine).
func (t *Tab) SetTitle(s string) {
	t.titleMu.Lock()
	t.title = s
	t.titleMu.Unlock()
}

// BellPending reports whether the tab beeped while unfocused.
func (t *Tab) BellPending() bool { return t.bellPending.Load() }

// SetBellPending sets/clears the bell-urgency flag.
func (t *Tab) SetBellPending(b bool) { t.bellPending.Store(b) }

func (t *Tab) titleBase() string {
	// Refresh the foreground process (throttled) BEFORE reading the
	// OSC title, so a stale title can be cleared first.
	if time.Since(t.foregroundAt) > foregroundCacheTTL && t.Terminal != nil {
		fg := t.Terminal.ForegroundProcessName()
		// An OSC title belongs to the process that set it. When the
		// foreground goes from an app back to a SHELL, that app's
		// title is stale — e.g. vim's "Thanks for flying Vim!"
		// farewell, which a shell with no title precmd never
		// overrides, so it'd otherwise stick forever. Drop it so the
		// title falls back to the live foreground name. Gated on
		// app→shell specifically: don't clear a shell's own title on
		// the first poll, and don't clobber an app's fresh title when
		// ENTERING it (shell→app).
		if t.foregroundCache != "" && !isShellProcess(t.foregroundCache) && isShellProcess(fg) {
			t.SetTitle("")
		}
		t.foregroundCache = fg
		t.foregroundAt = time.Now()
	}
	if tt := t.Title(); tt != "" {
		return tt
	}
	if t.foregroundCache != "" {
		return t.foregroundCache
	}
	return "shell"
}

// Manager manages the set of open tabs.
type Manager struct {
	Tabs      []*Tab
	ActiveIdx int
	NextID    int
	cfg       *config.Config

	// SourceFactory builds a terminal.Source for a new tab. When
	// nil (the default), NewTab spawns an in-process *terminal.Terminal.
	// The GUI sets this to a daemon-backed factory when the user
	// configured tab_source = "daemon" so all new tabs go through
	// the daemon connection instead.
	SourceFactory func(cols, rows int, cwd string) (terminal.Source, error)

	// ClipboardSetFn / ClipboardGetFn inject OS-clipboard access
	// into each new/adopted tab's source for OSC 52. The tabs
	// package can't import the SDL-backed clipboard directly
	// (cgo), so the app sets these. nil → OSC 52 clipboard sync
	// is disabled for that direction.
	ClipboardSetFn func(string)
	ClipboardGetFn func() string
}

// NewManager creates a new tab manager.
func NewManager(cfg *config.Config) *Manager {
	return &Manager{
		cfg:    cfg,
		NextID: 1,
	}
}

// RemoveTab pulls a tab out of the manager without closing its
// terminal — used by drag-between-windows to detach a tab so a
// different Manager can AdoptTab it. Returns the removed *Tab (or
// nil on out-of-range). Adjusts ActiveIdx so the active tab stays
// the same (or shifts to a neighbor if the removed tab was active).
func (m *Manager) RemoveTab(idx int) *Tab {
	if idx < 0 || idx >= len(m.Tabs) {
		return nil
	}
	tab := m.Tabs[idx]
	m.Tabs = append(m.Tabs[:idx], m.Tabs[idx+1:]...)
	if len(m.Tabs) == 0 {
		m.ActiveIdx = 0
	} else if m.ActiveIdx >= len(m.Tabs) {
		m.ActiveIdx = len(m.Tabs) - 1
	} else if idx < m.ActiveIdx {
		m.ActiveIdx--
	}
	return tab
}

// AdoptTab takes ownership of an already-running Source, wrapping
// it in a new Tab entry at the end of this manager's list and
// switching focus to it. The Source's existing PTY/connection +
// goroutines keep running; only the tab-list ownership changes.
//
// Re-installs the OSC-title callback so the new Tab's Title field
// keeps updating. Without this, adopted tabs (cross-window drag,
// daemon reattach, remote tab reattach) lose title updates — the
// old callback still points at the previous (now-dead) tab struct.
//
// Used by drag-between-windows after the source Manager
// RemoveTab'd the entry, and by daemon-source reattach paths.
func (m *Manager) AdoptTab(term terminal.Source) *Tab {
	tab := &Tab{
		ID:       m.NextID,
		Terminal: term,
	}
	m.NextID++
	m.Tabs = append(m.Tabs, tab)
	m.ActiveIdx = len(m.Tabs) - 1
	term.SetOnTitle(func(title string) {
		tab.SetTitle(title)
	})
	term.SetOnBell(func() {
		tab.SetBellPending(true)
	})
	m.installClipboardHooks(term)
	return tab
}

// installClipboardHooks wires the source's OSC 52 clipboard
// callbacks to the manager's injected OS-clipboard funcs (set by
// the app). No-op when the app didn't provide them.
func (m *Manager) installClipboardHooks(term terminal.Source) {
	if m.ClipboardSetFn != nil {
		term.SetOnClipboardSet(m.ClipboardSetFn)
	}
	if m.ClipboardGetFn != nil {
		term.SetClipboardProvider(m.ClipboardGetFn)
	}
}

// MoveTab reorders the tabs slice: removes the tab currently at
// index `from` and inserts it at `to`. Keeps ActiveIdx pointing at
// the same Tab (its slice position shifts based on the move). Used
// by drag-to-reorder in the tab bar. No-op on out-of-range or
// no-change moves.
func (m *Manager) MoveTab(from, to int) {
	n := len(m.Tabs)
	if from < 0 || from >= n || to < 0 || to >= n || from == to {
		return
	}
	active := m.Tabs[m.ActiveIdx]
	tab := m.Tabs[from]
	// Remove from current position.
	m.Tabs = append(m.Tabs[:from], m.Tabs[from+1:]...)
	// Insert at target position (which may need adjusting if the
	// removal shifted the indices, but `to` is the user-visible
	// target slot — interpret as "I want the dragged tab to end up
	// at slice index `to` in the final order").
	if to > len(m.Tabs) {
		to = len(m.Tabs)
	}
	m.Tabs = append(m.Tabs[:to], append([]*Tab{tab}, m.Tabs[to:]...)...)
	// Re-resolve ActiveIdx so the same Tab stays selected after move.
	for i, t := range m.Tabs {
		if t == active {
			m.ActiveIdx = i
			break
		}
	}
}

// NewTab creates a new tab with a fresh terminal. cwd is the starting
// directory for the shell; pass "" to inherit xerotty's CWD. Callers
// thread the parent tab's CWD when cfg.Tabs.InheritCWD is set so
// "New Tab" opens in the same directory the user was already in.
//
// If m.SourceFactory is set (daemon-backed mode) the new tab's
// source comes from there instead of an in-process PTY.
func (m *Manager) NewTab(cols, rows int, cwd string) (*Tab, error) {
	var term terminal.Source
	var err error
	if m.SourceFactory != nil {
		term, err = m.SourceFactory(cols, rows, cwd)
	} else {
		term, err = terminal.New(m.cfg, cols, rows, cwd)
	}
	if err != nil {
		return nil, err
	}

	tab := &Tab{
		ID:       m.NextID,
		Terminal: term,
		// Title intentionally left empty — DisplayTitle() falls back
		// to the foreground process name (or "shell" if even that
		// lookup fails) until the app emits an OSC 0/2 title.
	}
	term.SetOnTitle(func(title string) {
		tab.SetTitle(title)
	})
	term.SetOnBell(func() {
		tab.SetBellPending(true)
	})
	m.installClipboardHooks(term)
	m.NextID++
	m.Tabs = append(m.Tabs, tab)
	m.ActiveIdx = len(m.Tabs) - 1
	return tab, nil
}

// CloseTab closes the tab at the given index.
func (m *Manager) CloseTab(idx int) {
	if idx < 0 || idx >= len(m.Tabs) {
		return
	}

	m.Tabs[idx].Terminal.Close()
	m.Tabs = append(m.Tabs[:idx], m.Tabs[idx+1:]...)

	if m.ActiveIdx >= len(m.Tabs) {
		m.ActiveIdx = len(m.Tabs) - 1
	}
	if m.ActiveIdx < 0 {
		m.ActiveIdx = 0
	}
}

// CloseActive closes the currently active tab.
func (m *Manager) CloseActive() {
	m.CloseTab(m.ActiveIdx)
}

// Active returns the currently active tab, or nil if none.
func (m *Manager) Active() *Tab {
	if m.ActiveIdx < 0 || m.ActiveIdx >= len(m.Tabs) {
		return nil
	}
	return m.Tabs[m.ActiveIdx]
}

// Next switches to the next tab (clamped, no wrap).
func (m *Manager) Next() {
	if m.ActiveIdx < len(m.Tabs)-1 {
		m.ActiveIdx++
	}
}

// Prev switches to the previous tab (clamped, no wrap).
func (m *Manager) Prev() {
	if m.ActiveIdx > 0 {
		m.ActiveIdx--
	}
}

// GoTo switches to tab number n (1-indexed).
func (m *Manager) GoTo(n int) {
	idx := n - 1
	if idx >= 0 && idx < len(m.Tabs) {
		m.ActiveIdx = idx
	}
}

// SetTitle sets the title of the active tab.
func (m *Manager) SetTitle(title string) {
	if tab := m.Active(); tab != nil {
		tab.SetTitle(title)
	}
}

// Count returns the number of open tabs.
func (m *Manager) Count() int {
	return len(m.Tabs)
}

// DrainData drains data notifications from all tabs, marking dirty ones.
func (m *Manager) DrainData() {
	for _, tab := range m.Tabs {
		select {
		case <-tab.Terminal.DataChan():
			tab.Dirty = true
		default:
		}
	}
}

// CheckClosed checks for tabs whose child processes have exited.
func (m *Manager) CheckClosed() {
	for _, tab := range m.Tabs {
		if !tab.Closed && tab.Terminal.IsClosed() {
			tab.Closed = true
		}
	}
}

// isShellProcess reports whether name looks like an interactive shell
// — used to detect "an app exited back to the shell" so a stale OSC
// title set by the app (e.g. vim's farewell) can be dropped. Matches
// the bare executable name (ForegroundProcessName returns comm, not a
// path), tolerating the leading-dash login-shell form (e.g. "-zsh").
func isShellProcess(name string) bool {
	switch strings.TrimPrefix(name, "-") {
	case "zsh", "bash", "sh", "dash", "fish", "ksh", "tcsh", "csh", "nu", "elvish", "xonsh", "ash", "mksh":
		return true
	}
	return false
}
