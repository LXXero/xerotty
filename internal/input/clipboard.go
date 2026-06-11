package input

/*
#cgo pkg-config: sdl3
#include <SDL3/SDL.h>
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
	// SDL3 changed SDL_SetClipboardText to return bool (true=success).
	if !C.SDL_SetClipboardText(cs) {
		return errors.New(C.GoString(C.SDL_GetError()))
	}
	return nil
}

// imagePasteMIMEs is the priority order we check on the clipboard.
// Try lossless formats first; fall back to JPEG only if nothing
// else is offered (some pasteboards only have JPEG e.g. when the
// source was a phone photo).
var imagePasteMIMEs = []string{
	"image/png",
	"image/webp",
	"image/jpeg",
	"image/gif",
	"image/bmp",
}

// ClipboardReadImage looks for an image on the OS clipboard and
// returns its MIME type + raw bytes. Returns ("", nil, nil) when
// the clipboard doesn't contain an image. Used by the paste
// handler so Ctrl+Shift+V on a copied screenshot ships the bytes
// to the daemon (which writes them to a temp file the PTY child
// can read).
//
// SDL3's clipboard API supports arbitrary MIME types via
// SDL_GetClipboardData; we walk our preferred type list and grab
// the first match.
func ClipboardReadImage() (mime string, data []byte, err error) {
	for _, m := range imagePasteMIMEs {
		cmime := C.CString(m)
		has := C.SDL_HasClipboardData(cmime)
		C.free(unsafe.Pointer(cmime))
		if !has {
			continue
		}
		cmime = C.CString(m)
		var sz C.size_t
		ptr := C.SDL_GetClipboardData(cmime, &sz)
		C.free(unsafe.Pointer(cmime))
		if ptr == nil || sz == 0 {
			continue
		}
		// Copy the bytes out before SDL_free; SDL owns the buffer
		// until we release it.
		buf := C.GoBytes(unsafe.Pointer(ptr), C.int(sz))
		C.SDL_free(unsafe.Pointer(ptr))
		return m, buf, nil
	}
	return "", nil, nil
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
	// Native first: SDL3 speaks the PRIMARY selection directly
	// (zwp_primary_selection on Wayland, XA_PRIMARY on X11) — no
	// subprocess. The tool chain below survives only as a fallback
	// for SDL failure; the old order forked wl-paste/xclip on every
	// middle-click.
	if p := C.SDL_GetPrimarySelectionText(); p != nil {
		defer C.SDL_free(unsafe.Pointer(p))
		return C.GoString(p), nil
	}
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
	// Native first (see primaryReadUnix). The old path forked
	// wl-copy on EVERY selection release — and wl-copy daemonizes to
	// serve the selection, so each drag-select left a lingering
	// process behind.
	cs := C.CString(text)
	ok := bool(C.SDL_SetPrimarySelectionText(cs))
	C.free(unsafe.Pointer(cs))
	if ok {
		return nil
	}
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
