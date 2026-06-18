//go:build headless

package main

import (
	"fmt"
	"os"

	"github.com/LXXero/xerotty/internal/config"
)

// launchGUI in the headless build has no GUI to launch — this file
// imports nothing GUI-related, so `-tags headless` produces a
// binary with zero SDL3/GL/ImGui linkage. The server install
// still has full `xerotty serve` + `xerotty connect`; only the
// no-arg GUI default is unavailable.
func launchGUI(_ config.Config, _ []string, _ bool) int {
	fmt.Fprintln(os.Stderr, "xerotty: this is a headless build (no GUI). Use `xerotty serve` or `xerotty connect`.")
	return 1
}
