package supervise

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/LXXero/xerotty/internal/handoff"
	"github.com/LXXero/xerotty/internal/protocol"
)

// Config is what the supervisor needs to spawn and respawn daemons.
type Config struct {
	Binary        string // executable to run as the child
	SocketPath    string // wire socket (already bound: Listener)
	MCPSocketPath string // "" with NoMCP
	NoMCP         bool
	Listener      *os.File // the bound wire listener, supervisor-owned
	Log           io.Writer
}

// tabPlumb is the supervisor's copy of one tab's process plumbing.
type tabPlumb struct {
	pid    int
	ptmx   *os.File
	disk   *os.File // nil when the daemon reported no disk store
	exited bool
	code   int
}

type childStatus struct {
	pid    int
	status syscall.WaitStatus
}

// Supervisor runs daemon children and keeps their tabs alive across
// their deaths.
type Supervisor struct {
	cfg Config

	mu    sync.Mutex
	tabs  map[uint32]*tabPlumb
	state []byte // last KindState payload (msgpack handoff.State)
	ctl   *Conn  // current child's control channel (nil between children)

	// reapMu is held across starting a child and recording its pid,
	// and around each wait4 in reapAll, so a child that dies at once
	// is never reaped before childPID names it.
	reapMu    sync.Mutex
	childPID  int
	childExit chan childStatus
	shellExit chan ExitMsg
	stopping  bool

	// resumeFirst: the first child starts from a handoff built from
	// adopted state (AdoptHandoff) instead of fresh.
	resumeFirst     bool
	adoptedInstance string
	// stateFull: s.state is a complete daemon handoff (screens, modes,
	// scrollback index) adopted from an exec-in-place upgrade, not
	// the fd-less topology the child streams. The first KindState
	// frame from a child clears it.
	stateFull bool

	afterStart func() // test hook: runs between Start and recording the pid
}

// New builds a supervisor. Run does the work.
func New(cfg Config) *Supervisor {
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	return &Supervisor{
		cfg:       cfg,
		tabs:      make(map[uint32]*tabPlumb),
		childExit: make(chan childStatus, 4),
		shellExit: make(chan ExitMsg, 64),
	}
}

func (s *Supervisor) logf(format string, args ...any) {
	fmt.Fprintf(s.cfg.Log, "xerotty serve: supervisor: "+format+"\n", args...)
}

// resumeWindow / resumeBudget: more than resumeBudget resumes inside
// resumeWindow means the state is poison; the next child starts
// fresh (service restored, sessions lost — today's behavior) rather
// than crash-looping forever.
const (
	resumeWindow = 30 * time.Second
	resumeBudget = 3
)

// Run supervises daemon children until one exits cleanly (its own
// choice, or ours after SIGTERM/SIGINT). It only returns on that or
// on a spawn failure.
func (s *Supervisor) Run() error {
	if err := platformInit(); err != nil {
		s.logf("%v", err)
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)
	go s.reapLoop(sigs)
	go s.forwardShellExits()
	setExitSink(func(m ExitMsg) { s.shellExit <- m })

	resumeFile := ""
	if s.resumeFirst {
		resumeFile = s.writeHandoff()
	}
	var resumes []time.Time
	for {
		pid, err := s.spawn(resumeFile)
		if err != nil {
			return err
		}
		st := <-s.childExit
		_ = pid
		s.mu.Lock()
		stopping := s.stopping
		if s.ctl != nil {
			_ = s.ctl.Close()
			s.ctl = nil
		}
		s.mu.Unlock()
		if stopping {
			s.logf("daemon %d exited after shutdown request; done", st.pid)
			return nil
		}
		if st.status.Exited() && st.status.ExitStatus() == 0 {
			s.logf("daemon %d exited cleanly; done", st.pid)
			return nil
		}
		s.logf("daemon %d died (%s); resuming its sessions", st.pid, describe(st.status))

		now := time.Now()
		kept := resumes[:0]
		for _, t := range resumes {
			if now.Sub(t) < resumeWindow {
				kept = append(kept, t)
			}
		}
		resumes = append(kept, now)
		if len(resumes) > resumeBudget {
			s.logf("%d resumes in %s — handoff state is poison, starting a fresh daemon (sessions lost)", len(resumes), resumeWindow)
			s.dropAll()
			resumeFile = ""
			resumes = nil
			continue
		}
		resumeFile = s.writeHandoff()
	}
}

// spawn starts one daemon child. ExtraFiles layout, which the
// handoff file written for a resume must agree with: 3 = wire
// listener, 4 = control channel, 5.. = per-tab [ptmx, disk?] in the
// order writeHandoff recorded them.
func (s *Supervisor) spawn(resumeFile string) (int, error) {
	parent, childEnd, err := Pair()
	if err != nil {
		return 0, err
	}
	args := []string{"serve", "--child", "--socket", s.cfg.SocketPath, "--listen-fd", "3", "--control-fd", "4"}
	if s.cfg.NoMCP || s.cfg.MCPSocketPath == "" {
		args = append(args, "--no-mcp")
	} else {
		args = append(args, "--mcp-socket", s.cfg.MCPSocketPath)
	}
	if resumeFile != "" {
		args = append(args, "--resume", resumeFile)
	}
	cmd := exec.Command(s.cfg.Binary, args...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{s.cfg.Listener, childEnd}
	if resumeFile != "" {
		cmd.ExtraFiles = append(cmd.ExtraFiles, s.resumeFiles()...)
	}
	s.reapMu.Lock()
	if err := cmd.Start(); err != nil {
		s.reapMu.Unlock()
		parent.Close()
		childEnd.Close()
		return 0, fmt.Errorf("supervise: start daemon: %w", err)
	}
	if s.afterStart != nil {
		s.afterStart()
	}
	pid := cmd.Process.Pid
	s.mu.Lock()
	s.childPID = pid
	s.ctl = parent
	s.mu.Unlock()
	s.reapMu.Unlock()
	_ = childEnd.Close()
	// Our reaper owns every wait(); os/exec must never reap this pid.
	// (Release also forgets the pid, hence the copy above.)
	_ = cmd.Process.Release()
	s.logf("daemon %d started%s", pid, map[bool]string{true: " (resume)", false: ""}[resumeFile != ""])
	go s.readControl(parent)
	return pid, nil
}

// readControl consumes one child's frames until it closes.
func (s *Supervisor) readControl(c *Conn) {
	for {
		kind, payload, files, err := c.Recv()
		if err != nil {
			return
		}
		switch kind {
		case KindTab:
			var m TabMsg
			if json.Unmarshal(payload, &m) != nil || len(files) == 0 {
				closeAll(files)
				continue
			}
			tp := &tabPlumb{pid: m.PID, ptmx: files[0]}
			if len(files) > 1 {
				tp.disk = files[1]
			}
			closeAll(files[2:])
			s.mu.Lock()
			if old := s.tabs[m.ID]; old != nil {
				old.close()
			}
			s.tabs[m.ID] = tp
			s.mu.Unlock()
			watchPID(m.PID)
		case KindTabGone:
			var m TabGoneMsg
			closeAll(files)
			if json.Unmarshal(payload, &m) != nil {
				continue
			}
			s.mu.Lock()
			if tp := s.tabs[m.ID]; tp != nil {
				tp.close()
				delete(s.tabs, m.ID)
			}
			s.mu.Unlock()
		case KindState:
			closeAll(files)
			s.mu.Lock()
			s.state = payload
			s.stateFull = false
			s.mu.Unlock()
		default:
			closeAll(files)
		}
	}
}

// forwardShellExits marks exited tabs and tells the live child.
func (s *Supervisor) forwardShellExits() {
	for m := range s.shellExit {
		s.mu.Lock()
		for _, tp := range s.tabs {
			if tp.pid == m.PID {
				tp.exited = true
				tp.code = m.Code
			}
		}
		ctl := s.ctl
		s.mu.Unlock()
		if ctl != nil {
			_ = ctl.SendJSON(KindExit, m)
		}
	}
}

// reapLoop turns SIGCHLD into child/shell exit notices and forwards
// shutdown signals to the child.
func (s *Supervisor) reapLoop(sigs <-chan os.Signal) {
	for sig := range sigs {
		switch sig {
		case syscall.SIGCHLD:
			s.reapAll()
		case syscall.SIGTERM, syscall.SIGINT:
			s.mu.Lock()
			s.stopping = true
			pid := s.childPID
			s.mu.Unlock()
			if pid > 0 {
				_ = syscall.Kill(pid, sig.(syscall.Signal))
			}
		}
	}
}

// reapAll collects every exited child: the daemon goes to childExit,
// anything else (shells re-parented to us on Linux) to shellExit.
func (s *Supervisor) reapAll() {
	for {
		var ws syscall.WaitStatus
		s.reapMu.Lock()
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		s.mu.Lock()
		isDaemon := pid > 0 && pid == s.childPID
		s.mu.Unlock()
		s.reapMu.Unlock()
		if err == syscall.EINTR {
			continue
		}
		if err != nil || pid <= 0 {
			return
		}
		if isDaemon {
			s.childExit <- childStatus{pid: pid, status: ws}
			continue
		}
		s.shellExit <- ExitMsg{PID: pid, Code: exitCode(ws)}
	}
}

func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

func describe(ws syscall.WaitStatus) string {
	if ws.Signaled() {
		return "signal " + ws.Signal().String()
	}
	return fmt.Sprintf("exit status %d", ws.ExitStatus())
}

func (tp *tabPlumb) close() {
	if tp.ptmx != nil {
		_ = tp.ptmx.Close()
	}
	if tp.disk != nil {
		_ = tp.disk.Close()
	}
}

func (s *Supervisor) dropAll() {
	s.mu.Lock()
	for id, tp := range s.tabs {
		tp.close()
		delete(s.tabs, id)
	}
	s.state = nil
	s.mu.Unlock()
}

// liveTabIDs returns the ids of tabs whose shell is still running,
// in a stable order — the order resumeFiles and writeHandoff share.
func (s *Supervisor) liveTabIDs() []uint32 {
	ids := make([]uint32, 0, len(s.tabs))
	for id, tp := range s.tabs {
		if !tp.exited {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// resumeFiles lists the per-tab files in ExtraFiles order. Caller
// must not hold s.mu.
func (s *Supervisor) resumeFiles() []*os.File {
	s.mu.Lock()
	defer s.mu.Unlock()
	var files []*os.File
	for _, id := range s.liveTabIDs() {
		tp := s.tabs[id]
		files = append(files, tp.ptmx)
		if tp.disk != nil {
			files = append(files, tp.disk)
		}
	}
	return files
}

// writeHandoff builds the resume state from the last topology the
// child sent plus our plumbing, and writes it next to the socket.
// Returns "" (fresh start) when there is nothing to resume.
func (s *Supervisor) writeHandoff() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st handoff.State
	if len(s.state) > 0 {
		if _, err := st.UnmarshalMsg(s.state); err != nil {
			s.logf("stored state unreadable (%v); resuming from plumbing only", err)
			st = handoff.State{}
		}
	}
	st.Version = handoff.Version
	st.WireListenFD = 3
	st.MCPListenFD = -1
	st.SocketPath = s.cfg.SocketPath
	st.MCPSocket = s.cfg.MCPSocketPath

	known := make(map[uint32]handoff.TabState, len(st.Tabs))
	for _, ts := range st.Tabs {
		known[ts.ID] = ts
	}
	st.InstanceID = firstNonEmpty(st.InstanceID, s.adoptedInstance)
	st.Tabs = st.Tabs[:0]
	live := s.liveTabIDs()
	if len(live) == 0 {
		return ""
	}
	fd := 5 // after listener (3) and control (4)
	alive := make(map[uint32]bool, len(live))
	for _, id := range live {
		tp := s.tabs[id]
		ts, ok := known[id]
		if !ok {
			// The child died before it ever sent a topology frame
			// for this tab: resume it with defaults rather than lose
			// the shell. The daemon re-learns size from clients.
			ts = handoff.TabState{ID: id, Cols: 80, Rows: 24}
		}
		ts.PtmxFD = fd
		fd++
		ts.DiskFD = -1
		if tp.disk != nil {
			ts.DiskFD = fd
			fd++
		}
		if !s.stateFull {
			// Crash path: the daemon never streams its offset index
			// or screens; the resumed daemon rebuilds the index from
			// the file and apps repaint on the resume SIGWINCH. (An
			// adopted hot-upgrade handoff carries both; they ride
			// through untouched.)
			ts.DiskOffsets = nil
			ts.DiskSize = -1
			ts.Screen = nil
		}
		ts.ChildPID = tp.pid
		ts.ForeignChild = true
		ts.Exited = false
		st.Tabs = append(st.Tabs, ts)
		alive[id] = true
	}
	// Windows reference only live tabs; a child that never reported
	// topology gets one window holding everything.
	var wins []protocol.WindowInfo
	for _, w := range st.Windows {
		kept := w.TabIDs[:0:0]
		for _, id := range w.TabIDs {
			if alive[id] {
				kept = append(kept, id)
			}
		}
		if len(kept) == 0 {
			continue
		}
		w.TabIDs = kept
		if !alive[w.FocusedTabID] {
			w.FocusedTabID = kept[0]
		}
		wins = append(wins, w)
	}
	if len(wins) == 0 {
		wins = []protocol.WindowInfo{{ID: 1, TabIDs: live, FocusedTabID: live[0], Width: 800, Height: 600}}
		if st.NextWindowID < 2 {
			st.NextWindowID = 2
		}
	}
	st.Windows = wins
	for _, id := range live {
		if id >= st.NextTabID {
			st.NextTabID = id + 1
		}
	}

	path := filepath.Join(filepath.Dir(s.cfg.SocketPath), "xerottyd.handoff")
	if err := st.WriteFile(path); err != nil {
		s.logf("write handoff: %v — starting fresh", err)
		return ""
	}
	s.logf("handoff written: %d tabs", len(st.Tabs))
	return path
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// AdoptHandoff turns an exec-in-place upgrade of an UNSUPERVISED
// daemon into a supervised one with no session loss. The old daemon
// serialized its session and exec'd this image with every tab's
// ptmx/disk fd (and the wire listener) still open in the fd table;
// instead of resuming them ourselves we keep them, become the
// supervisor, and start a child from the same state. The shells
// stay our process children (same pid as the old daemon), so their
// exits reach reapAll like any re-parented shell would. Returns the
// listener file recorded in the state, or nil to bind fresh.
func (s *Supervisor) AdoptHandoff(st *handoff.State) *os.File {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptedInstance = st.InstanceID
	for _, ts := range st.Tabs {
		if ts.Exited || ts.PtmxFD < 0 || ts.ChildPID <= 0 {
			continue
		}
		tp := &tabPlumb{pid: ts.ChildPID, ptmx: os.NewFile(uintptr(ts.PtmxFD), "ptmx-adopted")}
		if ts.DiskFD >= 0 {
			tp.disk = os.NewFile(uintptr(ts.DiskFD), "scrollback-adopted")
		}
		if old := s.tabs[ts.ID]; old != nil {
			old.close()
		}
		s.tabs[ts.ID] = tp
		watchPID(ts.ChildPID)
	}
	// Keep the full state (screens, modes, offsets): writeHandoff only
	// remaps the fd numbers and marks the children foreign.
	if b, err := st.MarshalMsg(nil); err == nil {
		s.state = b
		s.stateFull = true
	}
	s.resumeFirst = len(s.tabs) > 0
	var lf *os.File
	if st.WireListenFD >= 0 {
		lf = os.NewFile(uintptr(st.WireListenFD), "wire-listener-adopted")
	}
	return lf
}

// SetListener installs the bound wire listener before Run.
func (s *Supervisor) SetListener(f *os.File) { s.cfg.Listener = f }
