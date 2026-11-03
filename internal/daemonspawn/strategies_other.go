//go:build !linux && !darwin

package daemonspawn

// No session manager we know how to talk to: a new session via fork
// is the whole story.
func strategies() []strategy {
	return []strategy{forkStrategy}
}
