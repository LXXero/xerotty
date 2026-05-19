//go:build !linux && !darwin

package platform

// SetResizeIncrements is a no-op on platforms where the WM has no
// equivalent of XSetWMNormalHints / setContentResizeIncrements.
// The renderer's software-side cell-grid math handles partial cells
// regardless.
func SetResizeIncrements(windowID uintptr, incW, incH int) {}
