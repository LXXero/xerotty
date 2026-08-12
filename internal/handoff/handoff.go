// Package handoff is the serialized state a daemon writes before
// exec-in-place upgrading itself, and the new image reads to resume.
// See docs/UPGRADE_PLAN.md for the full design.
//
// Versioning: Version gates the WHOLE file and is deliberately
// decoupled from protocol.ProtocolVersion — the wire format can
// change without breaking upgrades and vice versa. A binary that
// reads an unknown handoff version must refuse (the old daemon
// validates this with a dry run BEFORE exec, so refusal means
// "upgrade aborted, sessions intact", never "sessions lost").
//
// File descriptors are recorded as raw fd NUMBERS: exec-in-place
// keeps the process's fd table, so a number recorded pre-exec
// designates the same open description post-exec (FD_CLOEXEC
// cleared first). The handoff file itself contains terminal
// contents — written 0600 and deleted on resume.
package handoff

//go:generate msgp -unexported=false

import (
	"fmt"
	"os"

	"github.com/LXXero/xerotty/internal/protocol"
)

// Version is the handoff schema version this build writes and the
// only one it reads.
//
// History:
//
//	1 — initial: session topology, per-tab ptmx fd + child pid,
//	    screen/scrollback as protocol.Cell rows, disk-scrollback
//	    fd + offset index, listener fds.
const Version uint16 = 1

// State is the whole daemon, minus what deliberately does not
// survive: client connections (they reconnect), propose-queue
// entries (agents re-propose), deep emulator internals (SIGWINCH
// wiggle repaints full-screen apps).
type State struct {
	Version uint16 `msg:"version"`

	// InstanceID is KEPT across upgrades: clients scope their
	// close-tombstones to it, and a hot upgrade is the same logical
	// daemon with the same tab-ID space.
	InstanceID string `msg:"instance_id"`

	// Session counters / topology ("default" session — the only one
	// that exists today).
	NextTabID    uint32                `msg:"next_tab_id"`
	NextWindowID uint32                `msg:"next_window_id"`
	Revision     uint64                `msg:"revision"`
	Windows      []protocol.WindowInfo `msg:"windows"`
	Tabs         []TabState            `msg:"tabs"`

	// Inherited listener fds (-1 = not passed; resume re-listens on
	// the socket paths instead).
	WireListenFD int    `msg:"wire_listen_fd"`
	MCPListenFD  int    `msg:"mcp_listen_fd"`
	SocketPath   string `msg:"socket_path"`
	MCPSocket    string `msg:"mcp_socket"`
}

// TabState is one live tab: enough to rebuild a terminal.Terminal
// around the surviving ptmx fd and child process.
type TabState struct {
	ID    uint32 `msg:"id"`
	Name  string `msg:"name,omitempty"`
	Title string `msg:"title,omitempty"`
	CWD   string `msg:"cwd,omitempty"`
	Cols  int    `msg:"cols"`
	Rows  int    `msg:"rows"`

	// Process plumbing. PtmxFD is the master fd number (survives
	// exec). ChildPID is waited on by pid in the new image —
	// exec-in-place keeps us the parent, so waitpid stays legal.
	PtmxFD   int  `msg:"ptmx_fd"`
	ChildPID int  `msg:"child_pid"`
	Exited   bool `msg:"exited,omitempty"`
	ExitCode int  `msg:"exit_code,omitempty"`

	// Activity clock (unix nanos) — carried across the upgrade so a
	// long-idle tab keeps its real last-output/last-input age instead
	// of resetting to "just now" after `serve --upgrade`. 0 = unknown
	// (a pre-this-version handoff), reseeded to now on adopt.
	LastOutputAt int64 `msg:"last_output_at,omitempty"`
	LastInputAt  int64 `msg:"last_input_at,omitempty"`

	// Emulator-visible state, replayed via SetCell (the daemonsource
	// shadow-grid technique).
	Screen      [][]protocol.Cell `msg:"screen"`
	CursorRow   int               `msg:"cursor_row"`
	CursorCol   int               `msg:"cursor_col"`
	CursorStyle uint8             `msg:"cursor_style,omitempty"`
	CursorBlink bool              `msg:"cursor_blink,omitempty"`
	StyleSet    bool              `msg:"style_set,omitempty"`
	AppCursor   bool              `msg:"app_cursor,omitempty"`

	// Scrollback. In-memory rows serialize as cells; the disk store
	// (unlinked temp file — exists only through its fd) passes as
	// DiskFD (-1 = none) plus its in-memory offset index, which is
	// not recoverable from the fd alone without a full scan.
	MemScrollback [][]protocol.Cell `msg:"mem_scrollback,omitempty"`
	DiskFD        int               `msg:"disk_fd"`
	DiskOffsets   []int64           `msg:"disk_offsets,omitempty"`
	DiskSize      int64             `msg:"disk_size,omitempty"`
}

// WriteFile serializes s to path, 0600. The caller owns deletion
// (resume deletes it; an aborted upgrade deletes it too).
func (s *State) WriteFile(path string) error {
	s.Version = Version
	b, err := s.MarshalMsg(nil)
	if err != nil {
		return fmt.Errorf("handoff: marshal: %w", err)
	}
	return os.WriteFile(path, b, 0o600)
}

// ReadFile loads + version-gates a handoff file. An unknown version
// is a hard refusal — the validation gate runs this exact path
// before the old daemon commits to exec.
func ReadFile(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("handoff: read: %w", err)
	}
	var s State
	if _, err := s.UnmarshalMsg(b); err != nil {
		return nil, fmt.Errorf("handoff: decode: %w", err)
	}
	if s.Version != Version {
		return nil, fmt.Errorf("handoff: version %d, this binary speaks %d — refusing (upgrade aborted cleanly)", s.Version, Version)
	}
	return &s, nil
}

// writeRaw is WriteFile without the version stamp — test hook for
// exercising the version gate with hand-built bytes.
func writeRaw(path string, b []byte) error {
	return os.WriteFile(path, b, 0o600)
}
