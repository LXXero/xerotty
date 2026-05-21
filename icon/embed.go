// Package icon ships the runtime-embedded copies of the xerotty app
// icon. The directory also holds the SVG source-of-truth and the
// .icns built for the macOS bundle; only the PNG is shipped inside
// the binary itself (used on Linux/X11 for SDL_SetWindowIcon — the
// macOS path reads the bundle's .icns directly).
package icon

import _ "embed"

//go:embed xerotty-256.png
var pngBytes []byte

// PNG256 returns the embedded 256×256 PNG bytes. Caller decodes via
// image/png. Returns an empty slice if the build somehow excluded
// the asset.
func PNG256() []byte { return pngBytes }
