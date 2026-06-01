// Package daemon implements the headless xerottyd terminal session
// host. It owns PTYs + scrollback + tab layout and serves UI / MCP
// clients over the wire protocol defined in internal/protocol.
//
// Phase 0 scope (current):
//   - single in-process session named "default"
//   - tabs spawn shell PTYs via internal/terminal
//   - one UI client at a time can attach (more in Phase 7)
//   - cell updates sent as full-grid frames each tick the PTY
//     produced new data; cell-diff optimization is Phase 1+
//   - MCP socket not yet exposed (Phase 4)
//
// See docs/DAEMON_PLAN.md for the multi-phase roadmap.
package daemon
