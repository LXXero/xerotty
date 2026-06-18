//go:build !headless

package main

import (
	"fmt"
	"os"

	"github.com/LXXero/xerotty/internal/app"
	"github.com/LXXero/xerotty/internal/config"
)

// launchGUI runs the SDL3/ImGui GUI. This file is the ONLY place
// internal/app is imported — gated behind !headless so a
// `-tags headless` build excludes it and never links the GUI's
// cgo deps (SDL3, GL, freetype, fontconfig). Keep it that way:
// if any compiled-in file imports internal/app unconditionally,
// the headless build silently re-fattens. build.sh asserts
// against that with an ldd check.
func launchGUI(cfg config.Config, launchArgv []string, launchShell bool) int {
	// GUI launchers (Finder/launchd on macOS, some Linux menus) start
	// us with CWD "/". New tabs inherit the process CWD, so without
	// this every shell would open in / — start them in $HOME instead,
	// like every other terminal. Only the "/" case is rewritten; a
	// shell-launched `cd somewhere && xerotty` keeps its CWD.
	if wd, err := os.Getwd(); err != nil || wd == "/" {
		if home, herr := os.UserHomeDir(); herr == nil {
			_ = os.Chdir(home)
		}
	}
	a := app.New(cfg)
	// Cold-start `-e`/`-x` (no running instance to forward to): the
	// initial window's first tab runs the command instead of the shell.
	a.SetPendingLaunch(launchArgv, launchShell)
	if err := a.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty: %v\n", err)
		return 1
	}
	return 0
}
