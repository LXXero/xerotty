// xerotty is the terminal emulator (default mode: GUI). Subcommands
// expose the same binary as the headless daemon and a CLI thin
// client so users only have to install one file.
//
//	xerotty                  # GUI (default)
//	xerotty serve [...]      # headless daemon (owns PTYs + sockets)
//	xerotty connect [...]    # CLI thin client attached to a daemon
//	xerotty --help           # GUI flag help
//
// `xerotty serve --help` and `xerotty connect --help` print the
// per-subcommand flag set.
package main

import (
	"fmt"
	"os"

	"github.com/LXXero/xerotty/internal/app"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/runner"
)

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
		case "help", "--help", "-h":
			// Fall through to the GUI's flag/help handling.
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
