package input

import (
	"os/exec"
	"strings"
)

// ClipboardRead reads from the CLIPBOARD selection.
func ClipboardRead() (string, error) {
	return xclipRead("clipboard")
}

// PrimaryRead reads from the PRIMARY selection.
func PrimaryRead() (string, error) {
	return xclipRead("primary")
}

// ClipboardWrite writes to the CLIPBOARD selection.
func ClipboardWrite(text string) error {
	return xclipWrite("clipboard", text)
}

// PrimaryWrite writes to the PRIMARY selection.
func PrimaryWrite(text string) error {
	return xclipWrite("primary", text)
}

func xclipRead(sel string) (string, error) {
	// Try xclip first, then xsel
	out, err := exec.Command("xclip", "-selection", sel, "-o").Output()
	if err == nil {
		return string(out), nil
	}

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

func xclipWrite(sel, text string) error {
	// Try xclip first, then xsel
	cmd := exec.Command("xclip", "-selection", sel, "-i")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err == nil {
		return nil
	}

	selFlag := "--clipboard"
	if sel == "primary" {
		selFlag = "--primary"
	}
	cmd = exec.Command("xsel", selFlag, "--input")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
