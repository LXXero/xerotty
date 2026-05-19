// Cell-snap: constrain user drag-resize so the OS window's content
// area only changes in whole multiples of cell dimensions, the way
// xfce4-terminal and gnome-terminal do. Implementation is per-OS
// (X11 / NSWindow), and on Wayland nothing — the protocol doesn't
// support it.

#ifndef XEROTTY_PLATFORM_CELLSNAP_H
#define XEROTTY_PLATFORM_CELLSNAP_H

#ifdef __cplusplus
extern "C" {
#endif

void platform_set_resize_increments(unsigned long window_id, int inc_w, int inc_h);

#ifdef __cplusplus
}
#endif

#endif
