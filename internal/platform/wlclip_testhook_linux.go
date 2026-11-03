package platform

/*
#cgo pkg-config: wayland-client
#include <wayland-client.h>
#include "wlclip.h"

static struct wl_display* wlclip_own_display;

// wlclip_connect_own_display stands in for sdl3.cpp's post-window
// init when no SDL window exists (tests): a private wl_display with
// wlclip bound on it.
static int wlclip_connect_own_display(void) {
	if (!wlclip_own_display) wlclip_own_display = wl_display_connect(NULL);
	if (!wlclip_own_display) return 0;
	return wlclip_init(wlclip_own_display);
}

// One dispatch pass; in the GUI SDL's event pump does this.
static void wlclip_pump_own_display(void) {
	if (wlclip_own_display) wl_display_roundtrip(wlclip_own_display);
}
*/
import "C"

// wlclipConnectOwnDisplay and wlclipPumpOwnDisplay exist for
// wlclip_linux_test.go, which cannot use cgo itself (go test forbids
// it in _test.go files). Not used by the GUI.
func wlclipConnectOwnDisplay() bool { return C.wlclip_connect_own_display() != 0 }
func wlclipPumpOwnDisplay()         { C.wlclip_pump_own_display() }
