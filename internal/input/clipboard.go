package input

/*
#cgo pkg-config: sdl2
#include <SDL2/SDL.h>
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"unsafe"
)

// ClipboardRead reads from the OS clipboard via SDL's native binding —
// NSPasteboard on macOS, the GTK/X11/Wayland clipboard manager on Linux,
// CF_UNICODETEXT on Windows. Going through SDL means we don't depend on
// xclip/xsel/wl-paste being installed and we get the right pasteboard
// on every platform.
func ClipboardRead() (string, error) {
	p := C.SDL_GetClipboardText()
	if p == nil {
		return "", errors.New("SDL_GetClipboardText returned nil")
	}
	defer C.SDL_free(unsafe.Pointer(p))
	return C.GoString(p), nil
}

// ClipboardWrite writes to the OS clipboard via SDL.
func ClipboardWrite(text string) error {
	cs := C.CString(text)
	defer C.free(unsafe.Pointer(cs))
	if C.SDL_SetClipboardText(cs) != 0 {
		return errors.New(C.GoString(C.SDL_GetError()))
	}
	return nil
}

// PrimaryRead reads from the X11/Wayland PRIMARY selection (the
// mouse-select / middle-click-paste buffer). macOS has no equivalent —
// returns empty there.
func PrimaryRead() (string, error) {
	if runtime.GOOS == "darwin" {
		return "", nil
	}
	return primaryReadUnix()
}

// PrimaryWrite writes to the X11/Wayland PRIMARY selection. macOS has
// no equivalent — no-op there (writing to the system pasteboard on
// every drag-select would clobber the user's real clipboard, which Mac
// users don't expect).
func PrimaryWrite(text string) error {
	if runtime.GOOS == "darwin" {
		return nil
	}
	return primaryWriteUnix(text)
}

func isWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != ""
}

func primaryReadUnix() (string, error) {
	if isWayland() {
		out, err := exec.Command("wl-paste", "--no-newline", "--primary").Output()
		if err == nil {
			return string(out), nil
		}
	}
	out, err := exec.Command("xclip", "-selection", "primary", "-o").Output()
	if err == nil {
		return string(out), nil
	}
	out, err = exec.Command("xsel", "--primary", "--output").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func primaryWriteUnix(text string) error {
	if isWayland() {
		cmd := exec.Command("wl-copy", "--primary")
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	cmd := exec.Command("xclip", "-selection", "primary", "-i")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err == nil {
		return nil
	}
	cmd = exec.Command("xsel", "--primary", "--input")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
