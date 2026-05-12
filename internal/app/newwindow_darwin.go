//go:build darwin

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// newWindowCommand returns the exec.Cmd to spawn a fresh xerotty
// window. On macOS, if we're running inside a .app bundle, we route
// the launch through `open -n <bundle>` instead of a bare fork+exec
// — LaunchServices then registers the new process under our existing
// bundle's Dock icon (instead of creating a new LSApplication entry
// per window). Outside a bundle, fall back to the plain exec path.
func newWindowCommand(exe string, envExtras []string) *exec.Cmd {
	if bundle := bundleRootFromExe(exe); bundle != "" {
		args := []string{"-n"}
		for _, env := range envExtras {
			args = append(args, "--env", env)
		}
		args = append(args, bundle)
		return exec.Command("open", args...)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), envExtras...)
	return cmd
}

// bundleRootFromExe returns the path to the enclosing .app bundle if
// the executable lives at <bundle>/Contents/MacOS/<binary>, else "".
func bundleRootFromExe(exe string) string {
	dir := filepath.Dir(exe)
	if filepath.Base(dir) != "MacOS" {
		return ""
	}
	parent := filepath.Dir(dir)
	if filepath.Base(parent) != "Contents" {
		return ""
	}
	bundle := filepath.Dir(parent)
	if !strings.HasSuffix(bundle, ".app") {
		return ""
	}
	return bundle
}
