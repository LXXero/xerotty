package platform

/*
#include "popup_imgui.h"
*/
import "C"

import (
	"sync"
	"sync/atomic"
)

// PopupMenuDrawResult is what an ImGui popup-menu draw callback returns
// each frame to control the popup's lifecycle.
type PopupMenuDrawResult struct {
	// Close, when true, exits the popup loop after this frame's render.
	Close bool
	// DesiredWidth/Height let the caller request the OS popup window
	// be resized to fit the menu content. If both are >0 the C side
	// calls SDL_SetWindowSize before the next frame. Typically the
	// caller fills these from imgui.WindowSize() after the menu's
	// Begin/End so the OS window tracks ImGui's auto-resize.
	DesiredWidth  int
	DesiredHeight int
}

// RunImGuiPopup opens an SDL3 xdg_popup-backed window at
// (parentX, parentY) relative to the main parent window, and runs an
// internal frame loop that invokes draw each frame. draw uses
// cimgui-go's API normally — the underlying ImGui context is swapped
// to the popup's by the C side before each callback, so menu items
// render into the popup window not the main one.
//
// The popup auto-closes when:
//   - draw returns Close=true (item selected)
//   - the user presses Escape
//   - the user clicks outside (via xdg_popup_grab on Wayland or
//     XGrabPointer on X11)
//   - the OS window's close button is clicked
//
// Returns when the popup is dismissed. Caller is responsible for
// tracking what action was selected (typically via a closure
// variable in draw).
func RunImGuiPopup(parentWindowID uintptr, parentRelX, parentRelY, w, h int, draw func() PopupMenuDrawResult) {
	id := registerPopupCallback(draw)
	defer unregisterPopupCallback(id)
	C.platform_run_imgui_popup(
		C.ulong(parentWindowID),
		C.int(parentRelX), C.int(parentRelY),
		C.int(w), C.int(h),
		C.uint64_t(id),
	)
}

// Callback registry. The C loop calls back into Go via a numeric ID
// rather than a function pointer so we don't have to worry about
// cgo's restrictions on passing Go funcs across the FFI boundary.

var (
	popupCBMu   sync.Mutex
	popupCBs    = map[uint64]func() PopupMenuDrawResult{}
	popupCBNext atomic.Uint64
)

func registerPopupCallback(fn func() PopupMenuDrawResult) uint64 {
	id := popupCBNext.Add(1)
	popupCBMu.Lock()
	popupCBs[id] = fn
	popupCBMu.Unlock()
	return id
}

func unregisterPopupCallback(id uint64) {
	popupCBMu.Lock()
	delete(popupCBs, id)
	popupCBMu.Unlock()
}

//export goPopupImguiDraw
func goPopupImguiDraw(cbID C.uint64_t, outW, outH *C.int) C.int {
	popupCBMu.Lock()
	fn := popupCBs[uint64(cbID)]
	popupCBMu.Unlock()
	if fn == nil {
		return 1
	}
	res := fn()
	if outW != nil && res.DesiredWidth > 0 {
		*outW = C.int(res.DesiredWidth)
	}
	if outH != nil && res.DesiredHeight > 0 {
		*outH = C.int(res.DesiredHeight)
	}
	if res.Close {
		return 1
	}
	return 0
}
