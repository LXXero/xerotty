// Package tabs manages terminal tabs.
package tabs

import (
	"time"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/terminal"
)

// Tab represents a single terminal tab.
//
// Title is set by the OSC 0/2 escape sequence (most shells set it
// from their prompt, claude/vim/etc set it as their UI title). When
// no OSC has fired yet, DisplayTitle() falls back to the PTY's
// foreground process name (vim, top, ssh) so the title bar / tab bar
// shows what's actually running — matching iTerm2/Terminal.app
// behaviour for apps that don't emit OSC themselves.
type Tab struct {
	ID       int
	Title    string // OSC-set title; empty until the shell/app emits one
	Terminal *terminal.Terminal
	Dirty    bool
	Closed   bool

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
func (t *Tab) DisplayTitle() string {
	if t.Title != "" {
		return t.Title
	}
	if time.Since(t.foregroundAt) > foregroundCacheTTL && t.Terminal != nil {
		t.foregroundCache = t.Terminal.ForegroundProcessName()
		t.foregroundAt = time.Now()
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

// AdoptTab takes ownership of an already-running Terminal,
// wrapping it in a new Tab entry at the end of this manager's
// list and switching focus to it. The Terminal's existing PTY +
// goroutines keep running; only the tab-list ownership changes.
// Used by drag-between-windows after the source Manager
// RemoveTab'd the entry.
func (m *Manager) AdoptTab(term *terminal.Terminal) *Tab {
	tab := &Tab{
		ID:       m.NextID,
		Terminal: term,
	}
	m.NextID++
	m.Tabs = append(m.Tabs, tab)
	m.ActiveIdx = len(m.Tabs) - 1
	return tab
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
func (m *Manager) NewTab(cols, rows int, cwd string) (*Tab, error) {
	term, err := terminal.New(m.cfg, cols, rows, cwd)
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
	term.OnTitle = func(title string) {
		tab.Title = title
	}
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
		tab.Title = title
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
		case <-tab.Terminal.DataCh:
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
