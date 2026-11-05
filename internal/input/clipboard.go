package input

/*
#cgo pkg-config: sdl3
#include <SDL3/SDL.h>
#include <stdlib.h>
*/
import "C"

import (
	"bytes"
	"errors"
	"image/png"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/image/tiff"

	"github.com/LXXero/xerotty/internal/platform"
)

// textMIMEs is the order we look for text on a Wayland offer, and the
// set we advertise when writing. First entry is what everything
// modern offers; the X11-era atoms keep XWayland apps pasting.
var textMIMEs = []string{
	"text/plain;charset=utf-8",
	"UTF8_STRING",
	"text/plain",
	"TEXT",
	"STRING",
}

// pickMIME returns the first of wanted that offered contains, or "".
func pickMIME(offered, wanted []string) string {
	for _, w := range wanted {
		for _, o := range offered {
			if o == w {
				return w
			}
		}
	}
	return ""
}

// ClipboardRead reads the OS clipboard.
//
// On Wayland with data-control available it reads the compositor's
// current offer directly (platform.WaylandClipboardRead). SDL's own
// clipboard view is not trusted there: SDL marks itself owner on
// every write without waiting for the compositor, and wlroots
// silently rejects writes carrying an older input serial than the
// current selection, so after one rejected write (an OSC 52 write
// with no fresh input is enough) SDL answers every read from its own
// stale bookkeeping. See internal/platform/wlclip.h.
//
// Elsewhere — macOS, X11, compositors without data-control — SDL's
// native binding (NSPasteboard, X11 selections) is the path, so we
// never depend on xclip/xsel/wl-paste being installed.
func ClipboardRead() (string, error) { return clipboardRead(5 * time.Second) }

// ClipboardReadQuick is ClipboardRead with a short transfer budget,
// for callers on the render thread that poll (the daemon clipboard
// mirror). A stalled owner costs them a few hundred ms, not seconds.
func ClipboardReadQuick() (string, error) { return clipboardRead(300 * time.Millisecond) }

// ClipboardGeneration reports a counter that changes whenever the
// clipboard owner changes, and whether such a counter exists here.
// Pollers skip the read when it hasn't moved. Only the Wayland
// data-control path has one; elsewhere ok is false and callers read.
func ClipboardGeneration() (gen uint64, ok bool) {
	if !platform.WaylandClipboardAvailable() {
		return 0, false
	}
	return platform.WaylandClipboardGeneration(), true
}

func clipboardRead(timeout time.Duration) (string, error) {
	if platform.WaylandClipboardAvailable() {
		offered := platform.WaylandClipboardMimes()
		m := pickMIME(offered, textMIMEs)
		if m == "" {
			return "", nil
		}
		data, ok := platform.WaylandClipboardReadTimeout(m, timeout)
		if !ok {
			return "", errors.New("wayland clipboard: owner did not deliver " + m)
		}
		return string(data), nil
	}
	p := C.SDL_GetClipboardText()
	if p == nil {
		return "", errors.New("SDL_GetClipboardText returned nil")
	}
	defer C.SDL_free(unsafe.Pointer(p))
	return C.GoString(p), nil
}

// ClipboardWrite writes text to the OS clipboard. On Wayland with
// data-control it sets the selection through that protocol (no input
// serial involved, so writes from OSC 52 / MCP contexts can't be
// rejected behind SDL's back); otherwise via SDL.
func ClipboardWrite(text string) error {
	if platform.WaylandClipboardAvailable() {
		entries := make(map[string][]byte, len(textMIMEs))
		for _, m := range textMIMEs {
			entries[m] = []byte(text)
		}
		if platform.WaylandClipboardWrite(entries) {
			return nil
		}
		// Fall through: data-control went away between the check and
		// the write (compositor sent finished).
	}
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
	// macOS "Copy Image" (Safari, Preview, Messages) and Qt apps like
	// Flameshot put ONLY public.tiff on the pasteboard, and SDL does no
	// format conversion. Last so a real PNG always wins; converted to
	// PNG on read since most paste consumers (Claude Code) reject TIFF.
	"image/tiff",
}

// ClipboardReadImage looks for an image on the OS clipboard and
// returns its MIME type + raw bytes. Returns ("", nil, nil) when
// the clipboard doesn't contain an image. Used by the paste
// handler so Ctrl+Shift+V on a copied screenshot ships the bytes
// to the daemon (which writes them to a temp file the PTY child
// can read).
//
// On Wayland with data-control the offer is read directly (same
// reasons as ClipboardRead). Otherwise SDL3's clipboard API supports
// arbitrary MIME types via SDL_GetClipboardData; we walk our preferred
// type list and grab the first match.
func ClipboardReadImage() (mime string, data []byte, err error) {
	if platform.WaylandClipboardAvailable() {
		m := pickMIME(platform.WaylandClipboardMimes(), imagePasteMIMEs)
		if m == "" {
			return "", nil, nil
		}
		buf, ok := platform.WaylandClipboardRead(m)
		if !ok || len(buf) == 0 {
			return "", nil, errors.New("wayland clipboard: owner did not deliver " + m)
		}
		if m == "image/tiff" {
			if pngBuf, err := tiffToPNG(buf); err == nil {
				return "image/png", pngBuf, nil
			}
		}
		return m, buf, nil
	}
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
		if m == "image/tiff" {
			if pngBuf, err := tiffToPNG(buf); err == nil {
				return "image/png", pngBuf, nil
			}
		}
		return m, buf, nil
	}
	return "", nil, nil
}

func tiffToPNG(data []byte) ([]byte, error) {
	img, err := tiff.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
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
