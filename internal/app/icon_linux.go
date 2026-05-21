//go:build linux

package app

import (
	"bytes"
	"image"
	"image/png"

	"github.com/LXXero/xerotty/icon"
	"github.com/LXXero/xerotty/internal/platform"
)

// iconRGBA / iconW / iconH hold the decoded form of the embedded
// 256×256 app PNG. SDL_SetWindowIcon wants raw RGBA8 row-major, so
// we decode once on first need and reuse for every Window.
var (
	iconRGBA  []byte
	iconW     int
	iconH     int
	iconReady bool
)

func loadIcon() {
	if iconReady {
		return
	}
	data := icon.PNG256()
	if len(data) == 0 {
		return
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return
	}
	bounds := img.Bounds()
	iconW = bounds.Dx()
	iconH = bounds.Dy()
	iconRGBA = make([]byte, iconW*iconH*4)
	// Fast path: PNG decodes directly to *image.RGBA in tightly-packed
	// row-major top-to-bottom RGBA8 — exactly the layout SDL wants.
	if rgba, ok := img.(*image.RGBA); ok && rgba.Stride == iconW*4 {
		copy(iconRGBA, rgba.Pix)
	} else {
		i := 0
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				r, g, b, a := img.At(x, y).RGBA()
				iconRGBA[i+0] = byte(r >> 8)
				iconRGBA[i+1] = byte(g >> 8)
				iconRGBA[i+2] = byte(b >> 8)
				iconRGBA[i+3] = byte(a >> 8)
				i += 4
			}
		}
	}
	iconReady = true
}

// applyWindowIcon attaches the embedded app icon to the SDL_Window
// with the given ID. Idempotent — SDL_SetWindowIcon just replaces
// any prior icon. No-op if the PNG failed to decode or the handle
// is zero.
func applyWindowIcon(windowID uintptr) {
	if windowID == 0 {
		return
	}
	loadIcon()
	if !iconReady {
		return
	}
	platform.SetWindowIcon(windowID, iconRGBA, iconW, iconH)
}
