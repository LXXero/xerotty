# Daemon hot upgrade — design + phases

**Status: SHIPPED (all phases).** `xerotty serve --upgrade` (or
`kill -USR2`) replaces the running daemon binary without killing
the shells it hosts; the wire listener fd passes through the exec
so clients reconnect with zero refused-connection window, and
InstanceID is preserved (same logical daemon). The pre-exec
validation gate aborts cleanly on an incompatible target binary.
E2E: internal/runner/upgrade_e2e_test.go.

Goal: `xerotty serve --upgrade` replaces the running daemon binary
WITHOUT killing the shells it hosts. Today a daemon binary swap
costs every session (the "daemon dance"); the GUI half already
survives upgrades via detach/reattach, this closes the daemon half.

## Design: exec-in-place, not fd-passing

The obvious approach (nginx-style: start a new daemon process, ship
PTY fds over SCM_RIGHTS, old exits) has a fatal flaw for us: the
shells are CHILDREN of the old daemon. Hand them to a new process
and they reparent to init — the new daemon can never waitpid() them,
so exit codes and child-death detection break permanently.

Instead the daemon execs the new binary IN ITS OWN PROCESS
(syscall.Exec). Same PID:

  - the shells remain our children — os.FindProcess(pid).Wait()
    works in the new image; exit codes survive.
  - PTY master fds survive exec (clear FD_CLOEXEC first, record the
    fd numbers in the handoff state; new image os.NewFile()s them).
  - the listening sockets pass through the same way
    (UnixListener.File()) — no unlink/re-bind window, clients see a
    connection drop and auto-reconnect (Phase 10 resilience).
  - failure is safe by construction: syscall.Exec only RETURNS on
    error — bad path, missing binary — and then the old image just
    keeps running.

The cost: at Exec every goroutine and all in-memory state vanish.
Everything that must survive is serialized to a handoff state file
beforehand, and the new image rebuilds from it. Quiesce must be
honest (stop accepting, flush disk scrollback, then serialize).

## Handoff state (internal/handoff)

msgp-encoded (reuses protocol.Cell's generated codecs for grid
rows), with its OWN version number — `HandoffVersion` is decoupled
from ProtocolVersion: the wire can change without breaking upgrades,
and vice versa.

Per tab:
  - identity: ID, Name, Title, CWD, cols×rows
  - process: ptmx fd number, child PID, exited flag + code
  - screen: SnapshotViewport as [][]protocol.Cell + cursor pos/style
    — replayed into a fresh emulator via SetCell, the exact
    technique daemonsource's shadow grid already proves out
  - scrollback: disk mode passes the file paths; memory rows
    serialize as cell rows
  - modes: app-cursor (DECCKM), title

Session level: windows + tab membership, topology revision,
InstanceID (KEPT — clients scope close-tombstones to it; a hot
upgrade is the same logical daemon), next-tab-ID counter.

Deliberately NOT carried: client connections and subscriptions
(clients reconnect and re-attach), in-flight propose-queue entries
(dropped — agents re-propose), deep emulator internals (alt-screen
buffer, saved cursor, tabstops). For the latter the new image sends
each child a SIGWINCH wiggle after resume so full-screen apps
repaint themselves — the same tradeoff GUI reattach already makes.

## The upgrade dance

```
$ xerotty serve --upgrade        # CLI side: sends Upgrade over the wire socket
daemon:
  1. resolve the NEW binary path — PATH lookup / explicit flag, NOT
     /proc/self/exe (that's the old, possibly-deleted inode)
  2. serialize handoff state to a private file
  3. VALIDATION GATE: run `newbin serve --validate-handoff <file>`
     as a child; nonzero exit = abort upgrade, old daemon unharmed.
     This is what makes a schema-incompatible or broken new binary a
     clean error instead of a session massacre — after Exec there is
     no going back.
  4. quiesce: stop accepting, flush scrollback to disk, final
     re-serialize, clear FD_CLOEXEC on ptmx + listener fds
  5. syscall.Exec(newbin, ["xerotty", "serve", "--resume", file])
new image:
  6. read + version-check the handoff file, adopt fds, rebuild
     emulators from snapshots, re-arm a waitpid watcher per child
     PID, wrap inherited listener fds, delete the state file
  7. SIGWINCH each child (repaint wiggle), open for business
```

## Phases

- **Phase 0** — `internal/handoff`: the state schema + Write/Read
  with version gate + round-trip tests. No exec, no fd games.
- **Phase 1** — `terminal.Snapshot()` / `terminal.Adopt()`: capture
  a live Terminal's handoff TabState; build a NEW Terminal around an
  existing ptmx fd + child PID + snapshot (emulator replay, waitpid
  re-arm). Test: spawn a shell, snapshot, tear down the Terminal
  WITHOUT closing the PTY, Adopt into a fresh Terminal, prove the
  same shell still answers and the screen/scrollback carried over.
  This is the heart of the feature, fully testable in-process.
- **Phase 2** — the exec: daemon-side serialize + CLOEXEC handling +
  `serve --resume`. E2E: real daemon process upgrades to the SAME
  binary; a marker shell survives with its PID and screen intact.
- **Phase 3** — listener fd passthrough, InstanceID continuity,
  client auto-reconnect across the upgrade validated end to end.
- **Phase 4** — `serve --upgrade` trigger plumbing (wire message +
  CLI), the validation gate, MCP `server/upgrade`? (maybe), docs.

## Gotchas ledger

- Go opens everything O_CLOEXEC by design — fd survival is opt-in,
  per fd, right before Exec. Keep the window between clear and Exec
  tiny (no fork between them on our side… exec.Command pre-Exec for
  the validation gate is fine: validation runs BEFORE the clear).
- exec.Cmd does not survive; child management in the adopted world
  is pid-based (FindProcess/Wait/Signal). Wait works ONLY because
  exec-in-place preserves the parent PID.
- The PTY reader goroutine must be stopped before serialize (a read
  between snapshot and Exec would be lost). Quiesce order: stop
  readers → drain emulator feed → snapshot → exec. Output produced
  by the child DURING the gap sits in the kernel PTY buffer and is
  read by the new image — nothing is lost, it just renders late.
- vt emulator rebuild is lossy (see above). SIGWINCH wiggle is the
  documented mitigation, same as reattach.
- The handoff file contains terminal contents — 0600, private dir,
  deleted on resume.
