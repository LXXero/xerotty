// wlclip_darwin — empty stubs for the Wayland data-control clipboard on
// macOS. The real implementation lives in wlclip_linux.c and depends on
// libwayland-client. macOS has no Wayland; sdl3.cpp only calls
// wlclip_init when the video driver is "wayland", but the symbols still
// need to resolve at link time.

#include "wlclip.h"

int  wlclip_init(void* wl_display)              { (void)wl_display; return 0; }
int  wlclip_available(void)                     { return 0; }
uint64_t wlclip_generation(void)                { return 0; }
int  wlclip_mimes(char* buf, int cap)           { if (buf && cap > 0) buf[0] = 0; return -1; }
unsigned char* wlclip_receive(const char* mime, size_t* len, int timeout_ms) {
    (void)mime; (void)timeout_ms; if (len) *len = 0; return 0;
}
char* wlclip_receive_text(size_t* len, int timeout_ms) {
    (void)timeout_ms; if (len) *len = 0; return 0;
}
int  wlclip_set(const char* const* mimes, const unsigned char* const* datas,
                const size_t* lens, int n)      { (void)mimes; (void)datas; (void)lens; (void)n; return 0; }
int  wlclip_set_text(const char* text)          { (void)text; return 0; }
void wlclip_shutdown(void)                      { }
