package daemon

import (
	"sync"
	"sync/atomic"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/terminal"
)

// Session is a collection of tabs that survives client detach. The
// daemon may host multiple sessions in a later phase; Phase 0 has
// exactly one named "default".
type Session struct {
	Name string

	cfg *config.Config

	mu      sync.Mutex
	nextID  uint32
	tabs    []*Tab
	focused uint32 // ID of focused tab, 0 if none
}

// Tab is one PTY-backed terminal inside a session. The wrapped
// *terminal.Terminal already owns the PTY, the SafeEmulator, and
// the reader goroutines. We just track its identity in the session.
type Tab struct {
	ID    uint32
	Title string

	Term *terminal.Terminal

	// Per-tab serial number incremented every time the daemon
	// observes a "potentially new render" signal (new PTY data,
	// cursor move from app side, resize). Connections compare to
	// their last-published serial to decide whether to send a fresh
	// frame.
	dirty atomic.Uint64
}

func newSession(name string, cfg *config.Config) *Session {
	return &Session{
		Name:   name,
		cfg:    cfg,
		nextID: 1,
	}
}

// NewTab spawns a PTY-backed tab and adds it to the session. cols/
// rows are the initial dimensions; cwd is the starting directory
// ("" for xerottyd's CWD).
func (s *Session) NewTab(cols, rows int, cwd string) (*Tab, error) {
	term, err := terminal.New(s.cfg, cols, rows, cwd)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &Tab{
		ID:   s.nextID,
		Term: term,
	}
	s.nextID++
	s.tabs = append(s.tabs, t)
	if s.focused == 0 {
		s.focused = t.ID
	}
	// Hook the terminal's title callback to update our cached title.
	term.OnTitle = func(title string) {
		t.Title = title
	}
	return t, nil
}

// Tab returns the tab with the given ID, or nil if not found.
func (s *Session) Tab(id uint32) *Tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tabs {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Tabs returns a snapshot copy of the tab list. Safe for the caller
// to iterate without holding the session mutex.
func (s *Session) Tabs() []*Tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Tab, len(s.tabs))
	copy(out, s.tabs)
	return out
}

// Focused returns the ID of the currently-focused tab. Phase 0 only
// tracks focus to publish it on attach.
func (s *Session) Focused() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.focused
}

// SetFocused updates the focused tab. No-op if the ID isn't a tab.
func (s *Session) SetFocused(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tabs {
		if t.ID == id {
			s.focused = id
			return
		}
	}
}

// CloseTab removes a tab from the session and kills its PTY child.
// No-op if id isn't found.
func (s *Session) CloseTab(id uint32) {
	s.mu.Lock()
	var removed *Tab
	for i, t := range s.tabs {
		if t.ID == id {
			removed = t
			s.tabs = append(s.tabs[:i], s.tabs[i+1:]...)
			if s.focused == id {
				if len(s.tabs) > 0 {
					s.focused = s.tabs[0].ID
				} else {
					s.focused = 0
				}
			}
			break
		}
	}
	s.mu.Unlock()
	if removed != nil {
		removed.Term.Close()
	}
}
