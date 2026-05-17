//go:build !darwin

package sdlhack

// LeftButtonPhysicalDown returns the current left mouse button state.
func LeftButtonPhysicalDown() bool {
	return LeftButtonGlobalDown()
}
