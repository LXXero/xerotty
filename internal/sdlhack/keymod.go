package sdlhack

/*
#cgo pkg-config: sdl3
#include <SDL3/SDL.h>
*/
import "C"

// NumLockOn reports whether NumLock is currently active. cimgui-go's
// ImGui binding doesn't expose ModNum, so we reach into SDL directly.
// Used by the input handler to decide whether keypad keys should act
// as digits/operators (NumLock on) or as navigation (NumLock off).
func NumLockOn() bool {
	return uint32(C.SDL_GetModState())&C.SDL_KMOD_NUM != 0
}
