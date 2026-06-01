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
func launchGUI(cfg config.Config) int {
	a := app.New(cfg)
	if err := a.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty: %v\n", err)
		return 1
	}
	return 0
}
