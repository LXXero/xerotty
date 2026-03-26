// Package tabs manages terminal tabs.
package tabs

import (
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/terminal"
)

// Tab represents a single terminal tab.
type Tab struct {
	ID       int
	Title    string
	Terminal *terminal.Terminal
	Dirty    bool
	Closed   bool
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

// NewTab creates a new tab with a fresh terminal.
func (m *Manager) NewTab(cols, rows int) (*Tab, error) {
	term, err := terminal.New(m.cfg, cols, rows)
	if err != nil {
		return nil, err
	}

	tab := &Tab{
		ID:       m.NextID,
		Title:    "shell",
		Terminal: term,
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

// Next switches to the next tab.
func (m *Manager) Next() {
	if len(m.Tabs) > 0 {
		m.ActiveIdx = (m.ActiveIdx + 1) % len(m.Tabs)
	}
}

// Prev switches to the previous tab.
func (m *Manager) Prev() {
	if len(m.Tabs) > 0 {
		m.ActiveIdx = (m.ActiveIdx - 1 + len(m.Tabs)) % len(m.Tabs)
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
