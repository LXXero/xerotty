// xerotty is the terminal emulator (default mode: GUI). Subcommands
// expose the same binary as the headless daemon and a CLI thin
// client so users only have to install one file.
//
//	xerotty                  # GUI (default)
//	xerotty serve [...]      # headless daemon (owns PTYs + sockets)
//	xerotty connect [...]    # CLI thin client attached to a daemon
//	xerotty --help           # show subcommands + per-mode flag help
//
// Build with `-tags headless` to produce a server artifact that
// does NOT link SDL3/GL/ImGui/freetype/fontconfig: the GUI launch
// lives in gui.go (//go:build !headless); the headless build
// substitutes gui_headless.go, so internal/app is never imported
// and its cgo deps never link. serve + connect work identically
// in both builds. Install the lean artifact AS `xerotty` on
// servers so the SSH bridge + auto-spawn re-exec (which run
// `xerotty serve`) stay uniform.
package main

import (
	"fmt"
	"os"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/launchipc"
	"github.com/LXXero/xerotty/internal/runner"
)

const helpText = `xerotty — terminal emulator + daemon + CLI client, one binary.

USAGE
  xerotty                    Launch the GUI (default). If a GUI is
                             already running in this session, ask IT
                             to open a new window (in your CWD) and
                             exit, instead of starting a second GUI.
  xerotty --tab | -t         Same, but open a new tab in the running
                             GUI's focused window.
  xerotty --separate         Skip the running-GUI check and force a
                             brand-new GUI process.
  xerotty -e CMD [ARGS...]   Open a new window running CMD directly
                             instead of the shell (like xterm -e).
                             Consumes the rest of the arguments.
  xerotty -x "CMD STRING"    Open a new window running the command
                             string via $SHELL -c (like xterm -x), so
                             pipes/&&/globs work. When CMD exits, the
                             tab follows your on_child_exit setting.
  xerotty serve [flags]      Run the headless daemon. Owns PTYs,
                             serves the wire protocol on a unix
                             socket, exposes the MCP agent socket
                             for AI control (Claude Code, Xyphia).
  xerotty connect [flags]    Open a CLI thin client attached to a
                             local or remote daemon.
  xerotty mcp [flags]        Bridge stdio <-> an MCP socket so MCP
                             clients can drive xerotty as a tool
                             server. Targets the GUI's aggregating
                             socket, falling back to the local
                             daemon's. See docs/MCP.md.

  xerotty serve --upgrade    Hot-upgrade the RUNNING daemon to the
                             currently-installed binary. Shells and
                             tabs survive (exec-in-place; see
                             docs/UPGRADE_PLAN.md).

  xerotty serve   --help     Show flags for the serve subcommand.
  xerotty connect --help     Show flags for the connect subcommand.
  xerotty mcp     --help     Show flags for the mcp bridge.

COMMON RECIPES
  Run a local daemon and attach a CLI client to it:
    $ xerotty serve &
    $ xerotty connect

  Attach to a daemon on another machine over SSH:
    $ xerotty connect --ssh user@host
    (spawns "ssh user@host xerotty serve --stdio" — make sure the
     same xerotty is on that box's PATH, or use --remote-cmd.)

  Upgrade a running daemon after installing a new build (sessions
  survive — no more losing tabs to a binary swap):
    $ xerotty serve --upgrade

  Let Claude Code drive xerotty tabs as MCP tools (one-time setup):
    $ claude mcp add xerotty -- xerotty mcp

  Probe the MCP agent socket from a shell (debugging):
    $ echo '{"jsonrpc":"2.0","id":1,"method":"tabs/list"}' \
        | nc -U $XDG_RUNTIME_DIR/xerottyd.mcp.sock

SEE ALSO
  SPEC.md, docs/MCP.md, docs/DAEMON_PLAN.md, CLAUDE.md
`

func main() {
	// Subcommand dispatch happens before any GUI involvement so
	// the headless build (which has no GUI) still serves + connects.
	// Only the very first positional arg names a subcommand;
	// everything after it belongs to that subcommand's flag set.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Exit(runner.Serve(os.Args[2:]))
		case "connect":
			os.Exit(runner.Connect(os.Args[2:]))
		case "mcp":
			os.Exit(runner.MCPBridge(os.Args[2:]))
		case "help", "--help", "-h", "-help":
			fmt.Print(helpText)
			os.Exit(0)
		}
	}

	// GUI-mode flags. Parsed here, not in internal/app: the
	// single-instance forward must be decided BEFORE any GUI/SDL
	// code runs (and before config load — a running GUI already has
	// its own config). serve/connect dispatch above is unaffected.
	newTab, separate := false, false
	// -e/-x program override (the xterm-style launch feature). launchArgv
	// non-empty means "run this in the new window's first tab instead of
	// the shell"; launchShell picks `$SHELL -c` (`-x`) over direct exec
	// (`-e`).
	var launchArgv []string
	var launchShell bool
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tab", "-t":
			newTab = true
		case "--separate":
			separate = true
		case "--screenshot":
			// Visual-regression mode: render, dump the framebuffer as
			// PNG, exit. Implies --separate (a forwarded request can't
			// screenshot). Frames-to-settle via --screenshot-frames.
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "xerotty: --screenshot needs a path")
				os.Exit(2)
			}
			i++
			os.Setenv("XEROTTY_SCREENSHOT", args[i])
			separate = true
		case "--screenshot-frames":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "xerotty: --screenshot-frames needs a count")
				os.Exit(2)
			}
			i++
			os.Setenv("XEROTTY_SCREENSHOT_FRAMES", args[i])
		case "-e", "--exec":
			// Everything AFTER -e is the program + args, exec'd directly
			// (like `xterm -e`). Must be the last flag — it consumes the
			// rest of argv.
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "xerotty: -e needs a command")
				os.Exit(2)
			}
			launchArgv = append([]string(nil), args[i+1:]...)
			launchShell = false
			i = len(args) // consume the remaining args
		case "-x", "--exec-shell":
			// The next single arg is a command STRING run via `$SHELL -c`
			// (like `xterm -x` / `sh -c`), so pipes/&&/globs work.
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "xerotty: -x needs a command string")
				os.Exit(2)
			}
			i++
			launchArgv = []string{args[i]}
			launchShell = true
		default:
			fmt.Fprintf(os.Stderr, "xerotty: unknown argument %q (see xerotty --help)\n", args[i])
			os.Exit(2)
		}
	}

	// Single-instance: if a GUI is already running in this session,
	// hand it the request and exit. Any failure (no socket, stale
	// socket, no session identity at all) falls through to a normal
	// launch — which also covers "a daemon exists but no UI is
	// attached": the fresh GUI dials the daemon and adopts its
	// session exactly as before.
	if !separate {
		action := "window"
		// --tab only applies without a command; an `-e`/`-x` launch
		// always opens a fresh window (the tab forward path doesn't
		// carry the command).
		if newTab && len(launchArgv) == 0 {
			action = "tab"
		}
		cwd, _ := os.Getwd()
		req := launchipc.Request{Action: action, CWD: cwd, Argv: launchArgv, Shell: launchShell}
		if err := launchipc.Forward(req); err == nil {
			os.Exit(0)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerotty: config error: %v\n", err)
		os.Exit(1)
	}

	// launchGUI is defined in gui.go (full build) or
	// gui_headless.go (-tags headless). The headless variant has
	// no internal/app import, so SDL never links.
	os.Exit(launchGUI(cfg, launchArgv, launchShell))
}
