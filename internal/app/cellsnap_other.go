//go:build !darwin

package app

// setContentResizeIncrements is a no-op on platforms where there's no
// native equivalent. Linux X11 has _NET_WM_NORMAL_HINTS but SDL2 doesn't
// expose it; Wayland has no protocol-level equivalent (see
// RESIZING_PLAN.md for details).
func setContentResizeIncrements(cellW, cellH float32) {}
