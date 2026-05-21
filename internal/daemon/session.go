package daemon

import (
	"sync"
	"sync/atomic"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/terminal"
)

// Session is a collection of windows that each hold tabs. Survives
// client detach. The daemon may host multiple sessions later;
// Phase 0 has exactly one named "default".
//
// Session > Window > Tab is the canonical layout. WindowInfo.TabIDs
// is the source of truth for tab-bar ordering within a window.
type Session struct {
	Name string

	cfg *config.Config

	mu       sync.Mutex
	nextTabID uint32
	nextWinID uint32

	// All tabs in the session, keyed by ID. Each tab also lives
	// in exactly one Window's TabIDs slice (the daemon enforces
	// this invariant on every Window/Tab mutation).
	tabs map[uint32]*Tab

	// All windows in the session, in stable creation order.
	windows []*Window

	// App-wide focused tab — independent of per-window focus.
	focusedTabID uint32

	// Most recent clipboard text seen on this session (from any
	// attached client's ClipboardData frame). Daemon-side helpers
	// read it via Clipboard()/SetClipboard().
	clipboard string
}

// Clipboard returns the most recent clipboard text recorded on this
// session.
func (s *Session) Clipboard() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clipboard
}

// SetClipboard records new clipboard text for the session. Called
// when a client pushes ClipboardData. Use to backfill OSC 52 reads
// later.
func (s *Session) SetClipboard(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clipboard = text
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

// Window is a logical UI window — a grouping of tabs. The daemon
// tracks geometry as hints (the compositor on Wayland may override
// position) so a UI restoring the layout from another machine has
// something to work with.
type Window struct {
	ID           uint32
	PosX         int32
	PosY         int32
	Width        int32
	Height       int32
	TabIDs       []uint32 // tab-bar order, left-to-right
	FocusedTabID uint32   // which tab is in front within this window
}

func newSession(name string, cfg *config.Config) *Session {
	return &Session{
		Name:      name,
		cfg:       cfg,
		nextTabID: 1,
		nextWinID: 1,
		tabs:      make(map[uint32]*Tab),
	}
}

// ensureDefaultWindowLocked creates a default window if the session
// has none yet. Caller must hold s.mu.
func (s *Session) ensureDefaultWindowLocked() *Window {
	if len(s.windows) > 0 {
		return s.windows[0]
	}
	w := &Window{
		ID:     s.nextWinID,
		Width:  80,
		Height: 24,
	}
	s.nextWinID++
	s.windows = append(s.windows, w)
	return w
}

// NewWindow registers a new logical window in the session. The
// returned window has no tabs yet.
func (s *Session) NewWindow(posX, posY, width, height int32) *Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := &Window{
		ID:     s.nextWinID,
		PosX:   posX,
		PosY:   posY,
		Width:  width,
		Height: height,
	}
	s.nextWinID++
	s.windows = append(s.windows, w)
	return w
}

// CloseWindow removes a window. Tabs in the window get reassigned
// to the first remaining window in the session; if there are no
// other windows, a fresh default window is created so tabs stay
// accessible from a future reattach.
func (s *Session) CloseWindow(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed *Window
	var removedIdx int
	for i, w := range s.windows {
		if w.ID == id {
			removed = w
			removedIdx = i
			break
		}
	}
	if removed == nil {
		return
	}
	s.windows = append(s.windows[:removedIdx], s.windows[removedIdx+1:]...)
	if len(removed.TabIDs) == 0 {
		return
	}
	var dest *Window
	if len(s.windows) > 0 {
		dest = s.windows[0]
	} else {
		dest = s.ensureDefaultWindowLocked()
	}
	dest.TabIDs = append(dest.TabIDs, removed.TabIDs...)
	if dest.FocusedTabID == 0 && len(dest.TabIDs) > 0 {
		dest.FocusedTabID = dest.TabIDs[0]
	}
}

// NewTab spawns a PTY-backed tab and adds it to a window. windowID
// of 0 means "session's default window" (one is created if absent).
// cols/rows are the initial PTY dimensions; cwd is the starting
// directory ("" for xerottyd's CWD).
//
// Returns the new tab and the window it joined.
func (s *Session) NewTab(windowID uint32, cols, rows int, cwd string) (*Tab, *Window, error) {
	term, err := terminal.New(s.cfg, cols, rows, cwd)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &Tab{
		ID:   s.nextTabID,
		Term: term,
	}
	s.nextTabID++
	s.tabs[t.ID] = t
	term.OnTitle = func(title string) {
		t.Title = title
	}

	var w *Window
	if windowID != 0 {
		for _, candidate := range s.windows {
			if candidate.ID == windowID {
				w = candidate
				break
			}
		}
	}
	if w == nil {
		w = s.ensureDefaultWindowLocked()
	}
	w.TabIDs = append(w.TabIDs, t.ID)
	if w.FocusedTabID == 0 {
		w.FocusedTabID = t.ID
	}
	if s.focusedTabID == 0 {
		s.focusedTabID = t.ID
	}
	return t, w, nil
}

// Tab returns the tab with the given ID, or nil if not found.
func (s *Session) Tab(id uint32) *Tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tabs[id]
}

// Tabs returns a snapshot slice of all tabs. Caller may iterate
// without holding the session mutex.
func (s *Session) Tabs() []*Tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Tab, 0, len(s.tabs))
	for _, t := range s.tabs {
		out = append(out, t)
	}
	return out
}

// Windows returns a snapshot copy of the window list with each
// window's TabIDs copied so the caller can iterate safely.
func (s *Session) Windows() []*Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Window, len(s.windows))
	for i, w := range s.windows {
		// shallow-copy + fresh TabIDs slice
		ids := make([]uint32, len(w.TabIDs))
		copy(ids, w.TabIDs)
		out[i] = &Window{
			ID:           w.ID,
			PosX:         w.PosX,
			PosY:         w.PosY,
			Width:        w.Width,
			Height:       w.Height,
			TabIDs:       ids,
			FocusedTabID: w.FocusedTabID,
		}
	}
	return out
}

// FocusedTab returns the app-wide focused tab ID.
func (s *Session) FocusedTab() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.focusedTabID
}

// SetFocusedTab updates the app-wide focused tab. No-op if id isn't
// a tab in this session.
func (s *Session) SetFocusedTab(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tabs[id]; ok {
		s.focusedTabID = id
	}
}

// SetWindowFocusedTab updates per-window focus.
func (s *Session) SetWindowFocusedTab(windowID, tabID uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.windows {
		if w.ID == windowID {
			// Confirm the tab actually belongs to this window
			for _, id := range w.TabIDs {
				if id == tabID {
					w.FocusedTabID = tabID
					return
				}
			}
		}
	}
}

// SetWindowGeometry updates a window's position/size hints.
func (s *Session) SetWindowGeometry(id uint32, posX, posY, width, height int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.windows {
		if w.ID == id {
			w.PosX = posX
			w.PosY = posY
			w.Width = width
			w.Height = height
			return
		}
	}
}

// MoveTab moves a tab to a different window at the given index.
// Negative or out-of-range index appends to the destination's tail.
func (s *Session) MoveTab(tabID, toWindowID uint32, index int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Locate source window + remove the tab.
	var srcWin *Window
	for _, w := range s.windows {
		for i, id := range w.TabIDs {
			if id == tabID {
				srcWin = w
				w.TabIDs = append(w.TabIDs[:i], w.TabIDs[i+1:]...)
				if w.FocusedTabID == tabID {
					if len(w.TabIDs) > 0 {
						w.FocusedTabID = w.TabIDs[0]
					} else {
						w.FocusedTabID = 0
					}
				}
				break
			}
		}
		if srcWin != nil {
			break
		}
	}
	if srcWin == nil {
		return // tab not found
	}
	// Locate destination window.
	var dst *Window
	for _, w := range s.windows {
		if w.ID == toWindowID {
			dst = w
			break
		}
	}
	if dst == nil {
		// Bad destination — re-insert in source so we don't lose
		// the tab.
		srcWin.TabIDs = append(srcWin.TabIDs, tabID)
		if srcWin.FocusedTabID == 0 {
			srcWin.FocusedTabID = tabID
		}
		return
	}
	if index < 0 || index > len(dst.TabIDs) {
		index = len(dst.TabIDs)
	}
	dst.TabIDs = append(dst.TabIDs, 0)
	copy(dst.TabIDs[index+1:], dst.TabIDs[index:])
	dst.TabIDs[index] = tabID
	if dst.FocusedTabID == 0 {
		dst.FocusedTabID = tabID
	}
}

// CloseTab removes a tab from the session and kills its PTY child.
// Updates whichever window contained it. No-op if id isn't found.
func (s *Session) CloseTab(id uint32) {
	s.mu.Lock()
	t, ok := s.tabs[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.tabs, id)
	for _, w := range s.windows {
		for i, tid := range w.TabIDs {
			if tid == id {
				w.TabIDs = append(w.TabIDs[:i], w.TabIDs[i+1:]...)
				if w.FocusedTabID == id {
					if len(w.TabIDs) > 0 {
						w.FocusedTabID = w.TabIDs[0]
					} else {
						w.FocusedTabID = 0
					}
				}
				break
			}
		}
	}
	if s.focusedTabID == id {
		// Pick another tab if any exist.
		s.focusedTabID = 0
		for _, w := range s.windows {
			if w.FocusedTabID != 0 {
				s.focusedTabID = w.FocusedTabID
				break
			}
		}
	}
	s.mu.Unlock()
	t.Term.Close()
}
