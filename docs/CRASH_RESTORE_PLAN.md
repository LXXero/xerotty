# Daemon crash restore — containment + supervisor

Status: SHIPPED 2026-10-06 — Phase A (18a59b2) and Phase B (this
commit). `xerotty serve` is now a supervisor plus a daemon child;
`internal/runner/crash_restore_e2e_test.go` kills the child with
SIGKILL and proves the same shell answers on the same socket with
its scrollback. See docs/UPGRADE_PLAN.md for the hot-upgrade handoff
this reuses.

Deploying it: `serve --upgrade` on an UNSUPERVISED daemon migrates it
with no session loss. The exec-in-place lands in the new binary with
every tab's fds and the listener still in the fd table; instead of
resuming them itself the image becomes the supervisor, adopts the
handoff (screens, modes and scrollback index included), and starts a
daemon child from it. The shells stay the supervisor's own process
children, so exits reach it directly; the child sees them as foreign.
`internal/runner/upgrade_adopt_e2e_test.go` runs that migration and
then SIGKILLs the child to prove the adopted tabs resume as well.

## Why

2026-10-06: an emulator bug (stale DECSTBM after a shrink, fixed in
the vt/ultraviolet forks) panicked the xlin daemon mid-resize. Every
session on that box was gone in the time it took the ssh bridge to
auto-spawn a fresh daemon. Two separate failures were on display:

1. One tab's byte stream could unwind the whole process. The PTY
   reader has no recovery; a panic there is fatal for every tab.
2. Nothing outlives the daemon. The GUI half already survives a GUI
   crash (sessions live in the daemon), and `serve --upgrade`
   survives a binary swap (exec-in-place keeps the pid), but a
   daemon death loses the PTY masters, the child pids, and the
   unlinked scrollback files with it.

iTerm2's "session restoration" solves (2) by putting a tiny server
process between the UI and each PTY. xerotty already has that split
for the UI; what is missing is the same split between the daemon and
the process plumbing.

## Phase A — containment (internal/terminal)

The PTY reader goroutine recovers panics raised while feeding the
emulator. Recovery, in order:

- log the panic + stack to stderr (the daemon log);
- release publishMu (the ingest holds it across Emu.Write, so the
  write section is a function with a deferred unlock);
- reset the SAME emulator with RIS (`ESC c`): SafeEmulator releases
  its lock through a defer, so the object is usable, and swapping
  the `Emu` pointer would race every unsynchronized reader;
- SIGWINCH the child so full-screen apps repaint; shells redraw their
  prompt line;
- resume reading. The OSC pre-processor state restarts clean.

A terminal that panics repeatedly (3 times) is closed instead: the
byte stream is poison and the alternative is a hot loop. The child
dies with it, which is the pre-existing outcome for every tab, now
limited to one.

Test: `internal/terminal/panic_contain_test.go` injects a panic
through an unexported ingest hook and proves the shell survives, the
reader keeps going, and the third panic closes the tab.

## Phase B — supervisor (`xerotty serve` becomes two processes)

### Process shape

    xerotty serve                 supervisor: owns the wire LISTENER,
      └─ xerotty serve --child    holds dup'd PTY/scrollback fds,
           ├─ shell (tab 1)       restarts the daemon with --resume
           ├─ shell (tab 2)       when it dies.
           └─ ...

The child is today's daemon, unchanged in role. `serve --stdio`
auto-spawn and the GUI's EnsureLocalDaemon keep running plain
`xerotty serve`, so they get the supervisor for free.

`serve --upgrade` targets the SUPERVISOR: SO_PEERCRED on the wire
socket names the process that called listen(), and that is the
supervisor. (The first version of this plan assumed it named the
child; it never did, so `--upgrade` under a supervisor signalled a
process that ignored SIGUSR2 and then reported success because the
pid was alive. Fixed 2026-10-07; see "Upgrading under the
supervisor".)

### Control channel (internal/supervise)

A socketpair inherited by the child (`--control-fd`). Frames are
`uint32 length, uint8 kind, payload`; the only fds that ever travel
ride as SCM_RIGHTS on the `tab` frame.

child → supervisor:
- `tab {id, pid}` + fds [ptmx, disk-scrollback] — on every tab
  spawn. The supervisor dups nothing else; these are its copies.
- `tab_gone {id}` — tab closed; the supervisor drops its copies.
- `state <msgpack handoff.State>` — the session topology (windows,
  counters, names, titles, InstanceID) WITHOUT fds or screens.
  Sent on every topology revision bump and on name/title changes,
  debounced to 100 ms. This is what the supervisor would otherwise
  have to reconstruct.

supervisor → child:
- `exit {pid, code}` — a shell exited. Only meaningful for tabs the
  child did not spawn itself (see "foreign children"); for its own
  children waitpid already told it.
- `caps {upgrade}` — sent once right after spawn by a supervisor
  that handles SIGUSR2 itself. A child that has it ignores SIGUSR2;
  one that never got it (old supervisor) keeps exec-in-place.
- `upgrade {handoff}` — write the full handoff to that path and exit
  with status 75 (ExitUpgrade).

### Who holds what

- Wire listener: the supervisor binds it and passes it to every
  child generation (`--listen-fd`), the way the handoff already
  passes it across an exec. While the child is dead the socket
  stays bound, so reconnecting clients queue in the backlog instead
  of getting ECONNREFUSED — and the ssh bridge's "no daemon, spawn
  one" path never fires against a supervisor that is about to
  resume. The MCP socket stays child-owned and re-binds on resume,
  as it does across an upgrade.
- PTY masters + scrollback files: dup'd in the supervisor from the
  `tab` frame onward. Go opens fds cloexec; the supervisor passes
  them to a resumed child through ExtraFiles, writing the ExtraFiles
  index (3 + i) into the handoff as the fd number.
- Scrollback offset index: lives in daemon memory and is NOT
  streamed (it grows by one int64 per evicted line). The disk record
  format is length-prefixed, so a resumed daemon rebuilds the index
  by scanning the file once (`DiskScrollback.RebuildIndex`). The
  handoff marks this with `DiskSize = -1`.
- Scrollback contents: disk-backed scrollback became WRITE-THROUGH
  for this. Before, lines reached the disk store only when they aged
  out of vt's in-memory ring (up to 8192 lines), which a crash would
  have lost; now every line is appended (batched, one write per PTY
  read) as it scrolls off the grid, and the ring is a read cache for
  the newest ones. Total rows = disk count + the not-yet-written
  tail; `Terminal.scrollbackLayout` is the one place that math lives.

### Foreign children

After a crash the shells are no longer the daemon's children: on
Linux they re-parent to the supervisor (PR_SET_CHILD_SUBREAPER), on
macOS to launchd. The resumed daemon can never waitpid them. The
handoff marks such tabs `ForeignChild: true`; `terminal.Adopt` then
waits on an exit-notification channel instead of the process, fed by
the supervisor's `exit` frames:

- Linux: the supervisor is a subreaper and reaps re-parented shells
  with waitid(P_ALL, WNOWAIT) peeks so it never steals its own
  daemon child from os/exec's Wait.
- macOS: kqueue EVFILT_PROC with NOTE_EXIT|NOTE_EXITSTATUS on every
  pid it learned from a `tab` frame; launchd reaps.

Tabs that exit while the daemon is down are marked Exited in the
handoff and skipped on resume. Shells spawned by the resumed daemon
itself are its own children again; nothing changes for them.

The hot-upgrade handoff keeps its "shells remain our children"
property: exec-in-place does not change the parent. Upgrade and
crash-resume therefore coexist; a tab can be foreign across any
number of later upgrades, and the flag rides along in the handoff.

### Restart policy

- Child exits 0 after the supervisor asked it to (SIGTERM/SIGINT
  forwarded): supervisor exits.
- Child dies any other way: write handoff, spawn `--child --resume`.
  Three resume failures inside 30 s means the state is poison: start
  a fresh child without --resume (service restored, sessions lost,
  which is today's behavior) and log loudly.
- SIGUSR2 is the supervisor's: see below. It is not forwarded, and
  the child ignores its own copy, so a pkill that hits both processes
  upgrades once.

### Upgrading under the supervisor

1. SIGUSR2 reaches the supervisor. It runs the validation gate
   (`<binary> serve --validate-handoff`) on the binary at its path;
   a rejected binary aborts with the child untouched.
2. It sends `upgrade {handoff}`. The child quiesces exactly like an
   exec-in-place upgrade, writes the full handoff (screens, modes,
   scrollback index), and exits 75. No fd travels: the supervisor
   has held a copy of every PTY master and scrollback file since
   each tab spawned, and the listener never leaves it.
3. The supervisor drains the control channel, loads that handoff,
   and execs the new binary in place with the listener and every
   tab's fds left open (`serve --resume`, the same adoption an
   unsupervised daemon's upgrade uses). Same pid, so the shells
   stay its children. The new image starts a child from the full
   handoff. If the exec fails, the old supervisor starts the child
   itself from the same handoff.
4. A child that does not exit within 20 s is SIGKILLed and resumed
   from the new binary as after a crash.

A supervisor that handles SIGUSR2 writes `<socket>.supervisor`
(`{"pid":…,"upgrade":true}`). `serve --upgrade` uses it to tell such
a supervisor from one that predates this. For an old supervisor the
only lever is its crash-resume path: the CLI SIGKILLs the child (a
clean exit would make the supervisor stop and drop the sessions) and
the supervisor restarts it from the binary on disk. Screens come
back blank as after any crash, and the supervisor keeps its old code
until `xerotty serve` is restarted. The CLI prints which of these it
did.

`serve --upgrade` reports success only when the kernel shows the new
code running: the target binary's inode in /proc/PID/maps of the new
child (and of the supervisor, when it re-execs), a new child pid,
and a daemon answering the wire handshake. /proc/PID/exe is not
used: its path reads the same after the file was replaced. On macOS,
which has no maps file, only the new pid is checked.

### What comes back after a crash

Shells, scrollback (disk-backed, rebuilt index), tab ids, names,
titles, window topology, InstanceID (clients keep their
tombstones), the activity clock. Screens come back blank with a
SIGWINCH wiggle, exactly like the upgrade path's "deep emulator
internals" caveat: full-screen apps redraw, a shell at a prompt
redraws its line and the rest is in scrollback.

### Out of scope

Reboot. Nothing survives it, for us or for iTerm2 (its post-reboot
restore brings back arrangement and text with fresh shells).

## Verification

- `go test ./internal/terminal/` — Phase A containment test.
- `go test ./internal/supervise/` — frame + fd passing round trip,
  reaper/kqueue exit notices (platform files).
- `internal/runner/crash_restore_e2e_test.go` — start a supervised
  daemon, attach, echo the shell pid, `kill -9` the child daemon,
  reattach through the SAME socket, prove the same shell pid answers
  and the scrollback marker survived.
- `internal/runner/upgrade_adopt_e2e_test.go` — unsupervised daemon,
  `serve --upgrade`, supervisor + child with the same shell and
  screen, then a child SIGKILL, same shell again.
- `internal/runner/upgrade_supervised_e2e_test.go` — supervised
  daemon: a build the gate rejects leaves everything running and the
  CLI fails; a new build (new inode at the same path) ends with a
  new child and the supervisor mapping it, same shell, screen and
  scrollback. Plus the old-supervisor fallback
  (`XEROTTY_TEST_LEGACY_SUPERVISOR=1`).
- Fleet: `serve --upgrade` is the whole rollout; see "Deploying it".
