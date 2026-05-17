//go:build darwin

package app

/*
#cgo pkg-config: sdl2
#cgo LDFLAGS: -framework Cocoa
#include <stdint.h>
extern void xerottySetContentResizeIncrementsOn(uintptr_t handle, double w, double h);
*/
import "C"

// setContentResizeIncrements installs (cellW, cellH) as the content-area
// resize increment on the given Window's NSWindow. AppKit then snaps
// drag-resize to cell boundaries, eliminating sub-cell remainder and
// confining the OS framebuffer stretch to one cell width / height per
// drag step. Per-Window because each visible Window has its own
// NSWindow under multi-viewport and they each need their own snap
// installed (or removed when the user resizes via a non-drag path).
func setContentResizeIncrements(handle uintptr, cellW, cellH float32) {
	if handle == 0 {
		return
	}
	C.xerottySetContentResizeIncrementsOn(C.uintptr_t(handle), C.double(cellW), C.double(cellH))
}
