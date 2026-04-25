// Package dpi resolves the active display DPI so font sizes specified in
// points (the convention used by xfce4-terminal, gnome-terminal, etc.) can be
// converted to pixels for ImGui, which loads fonts at an explicit pixel size.
package dpi

/*
#cgo pkg-config: sdl2
#include <SDL2/SDL.h>
*/
import "C"

import (
	"os"
	"strconv"
	"sync"
)

const Default = 96.0

var (
	cached float32
	once   sync.Once
)

// Display returns the resolved screen DPI used to convert points to pixels.
// Resolution order: $XFT_DPI override -> SDL_GetDisplayDPI(0).hdpi -> 96.
// SDL must already be initialised (CreateWindow has been called).
func Display() float32 {
	once.Do(func() {
		cached = resolve()
	})
	return cached
}

// PointsToPixels converts a point size to a pixel size at the active DPI.
func PointsToPixels(pt float32) float32 {
	return pt * Display() / 72.0
}

func resolve() float32 {
	if v := os.Getenv("XFT_DPI"); v != "" {
		if d, err := strconv.ParseFloat(v, 32); err == nil && d > 0 {
			return float32(d)
		}
	}
	if C.SDL_WasInit(C.SDL_INIT_VIDEO) != 0 {
		var ddpi, hdpi, vdpi C.float
		if C.SDL_GetDisplayDPI(0, &ddpi, &hdpi, &vdpi) == 0 && hdpi > 0 {
			return float32(hdpi)
		}
	}
	return Default
}
