package daemon

import "log"

// logf writes one timestamped line to the daemon's stderr (which the
// GUI's auto-spawn redirects to ~/.cache/xerotty/xerottyd.log). All
// daemon-package diagnostics go through here so every line carries a
// timestamp — correlating reaps/resizes with what the user saw is
// impossible without one.
func logf(format string, args ...any) {
	log.Printf("xerottyd: "+format, args...)
}
