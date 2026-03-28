package input

import (
	"os"
	"os/exec"
	"strings"
)

// ClipboardRead reads from the CLIPBOARD selection.
func ClipboardRead() (string, error) {
	return clipRead("clipboard")
}

// PrimaryRead reads from the PRIMARY selection.
func PrimaryRead() (string, error) {
	return clipRead("primary")
}

// ClipboardWrite writes to the CLIPBOARD selection.
func ClipboardWrite(text string) error {
	return clipWrite("clipboard", text)
}

// PrimaryWrite writes to the PRIMARY selection.
func PrimaryWrite(text string) error {
	return clipWrite("primary", text)
}

func isWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != ""
}

func clipRead(sel string) (string, error) {
	if isWayland() {
		args := []string{"--no-newline"}
		if sel == "primary" {
			args = append(args, "--primary")
		}
		out, err := exec.Command("wl-paste", args...).Output()
		if err == nil {
			return string(out), nil
		}
		// Fall through to X11 tools (XWayland)
	}

	// xclip
	out, err := exec.Command("xclip", "-selection", sel, "-o").Output()
	if err == nil {
		return string(out), nil
	}

	// xsel
	selFlag := "--clipboard"
	if sel == "primary" {
		selFlag = "--primary"
	}
	out, err = exec.Command("xsel", selFlag, "--output").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func clipWrite(sel, text string) error {
	if isWayland() {
		args := []string{}
		if sel == "primary" {
			args = append(args, "--primary")
		}
		cmd := exec.Command("wl-copy", args...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return nil
		}
		// Fall through to X11 tools (XWayland)
	}

	// xclip
	cmd := exec.Command("xclip", "-selection", sel, "-i")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err == nil {
		return nil
	}

	// xsel
	selFlag := "--clipboard"
	if sel == "primary" {
		selFlag = "--primary"
	}
	cmd = exec.Command("xsel", selFlag, "--input")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
