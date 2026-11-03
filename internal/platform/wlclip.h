// wlclip — Wayland clipboard via the data-control protocol
// (ext-data-control-v1, falling back to wlr-data-control-unstable-v1).
//
// Why not SDL's clipboard on Wayland: wl_data_device.set_selection
// needs an input-event serial, and wlroots silently rejects a request
// whose serial is older than the current selection's (no error, no
// wl_data_source.cancelled). SDL 3.4 sends its last click/key serial
// and records itself as clipboard owner before the compositor answers,
// so a write that arrives without fresh input — an OSC 52 write from a
// program in a tab is the usual case — leaves SDL believing it owns a
// clipboard it never got. From then on SDL_HasClipboardData answers
// from its own bookkeeping and SDL_GetClipboardData returns nothing,
// until xerotty's next *accepted* write (verified against labwc 0.20 +
// SDL 3.4.18 with gdb and a standalone reproduction, 2026-11-03).
//
// data-control is the clipboard-manager protocol: it needs no serial
// and no focus, the device receives the current selection whenever it
// changes, and a source we set gets a reliable cancelled event when
// another client replaces it. Binding it on the wl_display SDL owns
// (same pattern as wlgrab) keeps everything in-process — no wl-copy /
// wl-paste subprocesses.
//
// Threading: all functions are called from the main (SDL) thread. The
// listeners run inside SDL's event dispatch on that same thread.
// wlclip_receive blocks the caller while the pipe drains (bounded by
// timeout_ms per read); it never needs our own dispatch because the
// owning client writes the pipe independently. Serving our own source
// to other clients happens on a detached thread per request so a slow
// reader can't stall the UI.
//
// No-op on non-Wayland video drivers and on compositors that don't
// advertise data-control (GNOME): wlclip_available() returns 0 and the
// caller falls back to SDL's clipboard.

#ifndef XEROTTY_PLATFORM_WLCLIP_H
#define XEROTTY_PLATFORM_WLCLIP_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

// wlclip_init binds a data-control manager and a wl_seat from the
// given wl_display (cast from SDL_PROP_WINDOW_WAYLAND_DISPLAY_POINTER)
// and creates the data-control device. Returns 1 when the clipboard
// is reachable through data-control, 0 otherwise. Idempotent.
int wlclip_init(void* wl_display);

// wlclip_available reports whether wlclip_init succeeded and the
// compositor has not since revoked the device (finished event).
int wlclip_available(void);

// wlclip_mimes writes the current clipboard offer's MIME types into
// buf, '\n'-separated, NUL-terminated, truncating at cap. Returns the
// number of MIME types (0 when the clipboard is empty), or -1 when
// data-control is unavailable.
int wlclip_mimes(char* buf, int cap);

// wlclip_receive reads the current clipboard offer as mime. Returns a
// malloc'd buffer the caller frees and sets *len; NULL when there is
// no offer, the offer lacks mime, or no byte arrives within timeout_ms.
// When our own source is the current selection the bytes come
// straight from our store (reading our own source through a pipe
// would deadlock: its send event is served by this thread).
unsigned char* wlclip_receive(const char* mime, size_t* len, int timeout_ms);

// wlclip_set makes xerotty the clipboard owner with n (mime, data,
// len) entries. Data is copied. Returns 1 if the request was issued.
int wlclip_set(const char* const* mimes, const unsigned char* const* datas,
               const size_t* lens, int n);

void wlclip_shutdown(void);

#ifdef __cplusplus
}
#endif

#endif
