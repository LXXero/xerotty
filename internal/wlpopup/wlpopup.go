// Package wlpopup wraps a native Wayland xdg_popup-backed context
// menu. Used by the app's right-click menu on Linux/Wayland sessions
// where the BeginV fallback can't get "click anywhere outside
// dismisses" behavior — SDL2 creates xdg_toplevel surfaces, and only
// xdg_popup gets the compositor-managed grab/dismiss machinery that
// e.g. GTK menus use.
//
// On non-Linux platforms this package builds to no-ops (Available
// returns false) so the caller falls back to its in-process menu.
package wlpopup

/*
#cgo linux pkg-config: wayland-client cairo
#cgo linux CFLAGS: -Wno-unused-parameter
#include "wlpopup.h"
#include <stdlib.h>
*/
import "C"

import (
	"unsafe"
)

// ItemType discriminates menu entries handed to Show.
type ItemType int

const (
	ItemRegular   ItemType = 0 // clickable item; Action returned on selection
	ItemSeparator ItemType = 1 // horizontal divider; Label and Action ignored
	ItemDisabled  ItemType = 2 // shown dimmed and not clickable
)

// Item is one menu entry.
type Item struct {
	Type   ItemType
	Label  string
	Action string
}

// PollResult is what Poll returns.
type PollResult int

const (
	PollPending     PollResult = 0 // popup still open, no result yet
	PollDismissed   PollResult = 1 // popup closed without a selection (clicked outside / Escape)
	PollItemPicked  PollResult = 2 // a regular item was clicked; see Action
)

// Init binds the wlpopup wayland globals using handles extracted from
// SDL_SysWMinfo's wl section. display is wl_display*, parentSurface
// is wl_surface*, parentXdgSurface is xdg_surface*. All three must
// be valid pointers obtained from SDL's syswm info while running
// under the wayland video driver. Returns nil on success.
//
// Safe to call multiple times — subsequent calls after the first
// successful one are no-ops.
func Init(display, parentSurface, parentXdgSurface uintptr) error {
	rc := C.wlpopup_init(unsafe.Pointer(display), unsafe.Pointer(parentSurface), unsafe.Pointer(parentXdgSurface))
	if rc != 0 {
		return &initError{code: int(rc)}
	}
	return nil
}

// Available reports whether wlpopup is initialized and ready.
// Returns false on non-Linux builds and when Init failed or hasn't
// been called.
func Available() bool {
	return C.wlpopup_available() != 0
}

// Show pops up the menu at parent-surface-relative coordinates (x, y)
// with the given items. Returns a positive popup handle, or 0 on
// failure. Non-blocking — caller drives the popup with Pump and Poll.
func Show(x, y int, items []Item) int {
	if len(items) == 0 {
		return 0
	}
	// Build C-side item array. Labels/actions get strdup'd in C so
	// these CStrings only need to live across the call.
	cItems := make([]C.wlpopup_item, len(items))
	cstrs := make([]unsafe.Pointer, 0, len(items)*2)
	defer func() {
		for _, p := range cstrs {
			C.free(p)
		}
	}()
	for i, it := range items {
		cItems[i]._type = C.int(it.Type)
		if it.Label != "" {
			p := unsafe.Pointer(C.CString(it.Label))
			cstrs = append(cstrs, p)
			cItems[i].label = (*C.char)(p)
		}
		if it.Action != "" {
			p := unsafe.Pointer(C.CString(it.Action))
			cstrs = append(cstrs, p)
			cItems[i].action = (*C.char)(p)
		}
	}
	id := C.wlpopup_show(C.int(x), C.int(y), &cItems[0], C.int(len(items)))
	return int(id)
}

// Poll checks the status of a popup. Returns PollPending while
// still open; PollDismissed when the user dismissed without
// choosing; PollItemPicked + the chosen action string when an
// item was clicked.
func Poll(popupID int) (PollResult, string) {
	var cAction *C.char
	rc := C.wlpopup_poll(C.int(popupID), &cAction)
	switch rc {
	case 0:
		return PollPending, ""
	case 2:
		var action string
		if cAction != nil {
			action = C.GoString(cAction)
		}
		return PollItemPicked, action
	default:
		return PollDismissed, ""
	}
}

// Dismiss manually closes an open popup (e.g. user opened a peer
// menu before the first one was dismissed).
func Dismiss(popupID int) {
	C.wlpopup_dismiss(C.int(popupID))
}

// Pump drives the wayland event loop once. Caller invokes per frame
// while any popup is open so events get dispatched and Poll returns
// up-to-date status.
func Pump() {
	C.wlpopup_pump()
}

type initError struct{ code int }

func (e *initError) Error() string {
	switch e.code {
	case 1:
		return "wlpopup: invalid display or parent handle"
	case 2:
		return "wlpopup: wl_display_create_queue failed"
	case 3, 4:
		return "wlpopup: wl_display_roundtrip_queue failed"
	case 5:
		return "wlpopup: compositor doesn't advertise required globals"
	default:
		return "wlpopup: init failed"
	}
}
