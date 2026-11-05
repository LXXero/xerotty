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
// One clipboard path per process: once wlclip is available, EVERY
// in-process clipboard write must go through it — including ImGui's
// text fields, which sdl3.cpp re-points here. If anything in this
// process still sets the selection through SDL, a later wlclip_receive
// of that selection would wait for a send event that only SDL's
// dispatch on the main thread can serve; when the reader is the main
// thread that is a stall for the whole timeout.
//
// Threading: callers may be on any thread (the render loop, the
// daemon hub's router goroutine delivering OSC 52 writes, a PTY reader
// goroutine). The listeners run inside SDL's event dispatch on the
// main thread. One mutex guards the offer/source bookkeeping; it is
// never held across a blocking read — wlclip_receive issues the
// receive request under the lock and then drains the pipe without it,
// so the dispatch thread is free to replace the offer meanwhile (the
// compositor completes a transfer already requested). Serving our own
// source to other clients happens on a detached thread per request.
//
// No-op on non-Wayland video drivers and on compositors that don't
// advertise data-control (GNOME): wlclip_available() returns 0 and the
// caller falls back to SDL's clipboard.

#ifndef XEROTTY_PLATFORM_WLCLIP_H
#define XEROTTY_PLATFORM_WLCLIP_H

#include <stddef.h>
#include <stdint.h>

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

// wlclip_generation counts selection changes seen by the device.
// Pollers compare it instead of reading the clipboard every tick.
uint64_t wlclip_generation(void);

// wlclip_mimes writes the current clipboard offer's MIME types into
// buf, '\n'-separated, NUL-terminated, truncating at cap. Returns the
// number of MIME types (0 when the clipboard is empty), or -1 when
// data-control is unavailable.
int wlclip_mimes(char* buf, int cap);

// wlclip_receive reads the current clipboard offer as mime. Returns a
// malloc'd buffer the caller frees and sets *len; NULL when there is
// no offer, the offer lacks mime, or the transfer has not completed
// within timeout_ms in total (0 = 5000). When our own source is the
// current selection the bytes come straight from our store.
unsigned char* wlclip_receive(const char* mime, size_t* len, int timeout_ms);

// wlclip_receive_text is wlclip_receive for text: tries the text MIME
// types in preference order and returns a NUL-terminated malloc'd
// string (NULL when the clipboard holds no text).
char* wlclip_receive_text(size_t* len, int timeout_ms);

// wlclip_set makes xerotty the clipboard owner with n (mime, data,
// len) entries. Data is copied. Returns 1 if the request was issued.
int wlclip_set(const char* const* mimes, const unsigned char* const* datas,
               const size_t* lens, int n);

// wlclip_set_text is wlclip_set for a NUL-terminated string, offered
// under the usual text MIME types.
int wlclip_set_text(const char* text);

void wlclip_shutdown(void);

#ifdef __cplusplus
}
#endif

#endif
