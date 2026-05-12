//go:build !linux && !darwin

package terminal

func processCWD(pid int) string {
	return ""
}
