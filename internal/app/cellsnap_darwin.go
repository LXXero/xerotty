//go:build darwin

package app

/*
#cgo pkg-config: sdl2
#cgo LDFLAGS: -framework Cocoa
extern void xerottySetContentResizeIncrements(double w, double h);
*/
import "C"

// setContentResizeIncrements installs (cellW, cellH) as the content-area
// resize increment on the main NSWindow. AppKit then snaps drag-resize
// to cell boundaries, eliminating sub-cell remainder and confining the
// OS framebuffer stretch to one cell width / height per drag step.
func setContentResizeIncrements(cellW, cellH float32) {
	C.xerottySetContentResizeIncrements(C.double(cellW), C.double(cellH))
}
