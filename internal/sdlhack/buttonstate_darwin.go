//go:build darwin

package sdlhack

/*
#cgo darwin LDFLAGS: -framework ApplicationServices
#include <ApplicationServices/ApplicationServices.h>

static int xerotty_left_button_physical_down(void) {
	return CGEventSourceButtonState(kCGEventSourceStateCombinedSessionState, kCGMouseButtonLeft) ? 1 : 0;
}
*/
import "C"

// LeftButtonPhysicalDown returns the current OS-level left mouse button state.
// The macOS implementation uses CoreGraphics so a background wake poller does
// not have to call into SDL while the main thread is blocked in SDL_WaitEvent.
func LeftButtonPhysicalDown() bool {
	return C.xerotty_left_button_physical_down() != 0
}
