// xerotty is the terminal emulator (default mode: GUI). Subcommands
// expose the same binary as the headless daemon and a CLI thin
// client so users only have to install one file.
//
//	xerotty                  # GUI (default)
//	xerotty serve [...]      # headless daemon (owns PTYs + sockets)
//	xerotty connect [...]    # CLI thin client attached to a daemon
//	xerotty --help           # show subcommands + per-mode flag help
package main

import (
	"fmt"
	"os"

	"github.com/LXXero/xerotty/internal/app"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/runner"
)

const helpText = `xerotty — terminal emulator + daemon + CLI client, one binary.

USAGE
  xerotty                    Launch the GUI (default).
  xerotty serve [flags]      Run the headless daemon. Owns PTYs,
                             serves the wire protocol on a unix
                             socket, exposes the MCP agent socket
                             for AI control (Claude Code, Xyphia).
  xerotty connect [flags]    Open a CLI thin client attached to a
                             local or remote daemon.

  xerotty serve   --help     Show flags for the serve subcommand.
  xerotty connect --help     Show flags for the connect subcommand.

COMMON RECIPES
  Run a local daemon and attach a CLI client to it:
    $ xerotty serve &
    $ xerotty connect

  Attach to a daemon on another machine over SSH:
    $ xerotty connect --ssh user@host
    (spawns "ssh user@host xerotty serve --stdio" — make sure the
     same xerotty is on that box's PATH, or use --remote-cmd.)

  Probe the MCP agent socket from a shell:
    $ echo '{"jsonrpc":"2.0","id":1,"method":"tabs/list"}' \
        | nc -U $XDG_RUNTIME_DIR/xerottyd.mcp.sock

SEE ALSO
  SPEC.md, docs/DAEMON_PLAN.md, CLAUDE.md
`

func main() {
	// Subcommand dispatch happens before flag.Parse so the GUI
	// keeps its own flag set untouched. Only the very first
	// positional arg can name a subcommand; everything after it
	// belongs to that subcommand's flag set.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Exit(runner.Serve(os.Args[2:]))
		case "connect":
			os.Exit(runner.Connect(os.Args[2:]))
		case "help", "--help", "-h", "-help":
			fmt.Print(helpText)
			os.Exit(0)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerotty: config error: %v\n", err)
		os.Exit(1)
	}

	a := app.New(cfg)
	if err := a.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty: %v\n", err)
		os.Exit(1)
	}
}
