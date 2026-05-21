//go:build !linux

package app

// macOS reads its app icon from the .app bundle's CFBundleIconFile
// (icon/xerotty.icns, installed by `make app`) — SDL_SetWindowIcon
// per viewport isn't useful there. Other non-Linux platforms don't
// have a runtime icon path wired yet.
func applyWindowIcon(_ uintptr) {}
