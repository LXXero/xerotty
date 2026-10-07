package supervise

import (
	"encoding/json"
	"os"
)

// PIDFile is what a supervisor that handles SIGUSR2 upgrades writes
// next to its wire socket. `serve --upgrade` reads it to tell such a
// supervisor from one that predates the feature: both own the socket
// (SO_PEERCRED names either), but an old one ignores SIGUSR2, and
// the CLI has to take its crash-resume path instead.
type PIDFile struct {
	PID     int  `json:"pid"`
	Upgrade bool `json:"upgrade"`
}

// PIDFilePath is the pidfile for the supervisor serving socketPath.
func PIDFilePath(socketPath string) string { return socketPath + ".supervisor" }

// ReadPIDFile reads the supervisor pidfile for socketPath. A stale
// file is the caller's problem: compare PID with the socket's peer.
func ReadPIDFile(socketPath string) (PIDFile, error) {
	var pf PIDFile
	b, err := os.ReadFile(PIDFilePath(socketPath))
	if err != nil {
		return pf, err
	}
	err = json.Unmarshal(b, &pf)
	return pf, err
}

func writePIDFile(socketPath string) error {
	b, _ := json.Marshal(PIDFile{PID: os.Getpid(), Upgrade: true})
	tmp := PIDFilePath(socketPath) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, PIDFilePath(socketPath))
}

// removePIDFile removes the pidfile only if it is still ours.
func removePIDFile(socketPath string) {
	if pf, err := ReadPIDFile(socketPath); err == nil && pf.PID == os.Getpid() {
		_ = os.Remove(PIDFilePath(socketPath))
	}
}
