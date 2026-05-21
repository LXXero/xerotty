// wlgrab — see wlgrab.h. Compiled as plain C (cgo CFLAGS).

#include "wlgrab.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>  // close() for the keymap fd we don't use

#include <wayland-client.h>
#include "xdg-shell-client-protocol.h"

// One-shot init guard. Wayland binds are idempotent enough at the
// compositor level but we'd leak proxies on repeated calls.
static int               g_initialized = 0;
static struct wl_display*  g_display    = NULL;
static struct wl_seat*     g_seat       = NULL;
static struct wl_pointer*  g_pointer    = NULL;
static struct wl_keyboard* g_keyboard   = NULL;
static uint32_t            g_last_serial = 0;

// Wayland drag-and-drop state. The data_device manager lets us
// create a data_device for a seat, and a data_device lets us start
// drag sessions whose enter/leave/drop events are routed by the
// compositor — the only way to do cross-surface drag on Wayland.
static struct wl_data_device_manager* g_ddm    = NULL;
static struct wl_data_device*         g_dd     = NULL;
static struct wl_surface*             g_drag_origin = NULL; // source surface for current drag
static struct wl_surface*             g_drag_target = NULL; // currently-hovered surface during a drag
static struct wl_surface*             g_drop_target = NULL; // target snapshotted at drop time
static struct wl_surface*             g_last_drag_target = NULL; // last surface entered by this drag
static int                            g_drop_fired  = 0;
static int                            g_drop_performed = 0;
static int                            g_drag_active = 0;
static struct wl_data_offer*          g_current_offer = NULL;

// --- wl_pointer listener — every event carries the serial we need. ---

static void pointer_enter(void* data, struct wl_pointer* p, uint32_t serial,
                          struct wl_surface* surf, wl_fixed_t x, wl_fixed_t y) {
    (void)data; (void)p; (void)surf; (void)x; (void)y;
    g_last_serial = serial;
}
static void pointer_leave(void* data, struct wl_pointer* p, uint32_t serial,
                          struct wl_surface* surf) {
    (void)data; (void)p; (void)surf;
    g_last_serial = serial;
}
static void pointer_motion(void* data, struct wl_pointer* p, uint32_t t,
                           wl_fixed_t x, wl_fixed_t y) {
    (void)data; (void)p; (void)t; (void)x; (void)y;
    // motion events don't carry a serial — xdg_popup.grab needs a
    // serial bound to an input event the compositor will accept, and
    // motion serials don't count anyway.
}
static void pointer_button(void* data, struct wl_pointer* p, uint32_t serial,
                           uint32_t time, uint32_t button, uint32_t state) {
    (void)data; (void)p; (void)time; (void)button; (void)state;
    g_last_serial = serial;
}
static void pointer_axis(void* data, struct wl_pointer* p, uint32_t time,
                         uint32_t axis, wl_fixed_t value) {
    (void)data; (void)p; (void)time; (void)axis; (void)value;
}
static void pointer_frame(void* data, struct wl_pointer* p) { (void)data; (void)p; }
static void pointer_axis_source(void* data, struct wl_pointer* p, uint32_t s) { (void)data; (void)p; (void)s; }
static void pointer_axis_stop(void* data, struct wl_pointer* p, uint32_t t, uint32_t a) { (void)data; (void)p; (void)t; (void)a; }
static void pointer_axis_discrete(void* data, struct wl_pointer* p, uint32_t a, int32_t d) { (void)data; (void)p; (void)a; (void)d; }
static void pointer_axis_value120(void* data, struct wl_pointer* p, uint32_t a, int32_t v) { (void)data; (void)p; (void)a; (void)v; }
static void pointer_axis_relative_direction(void* data, struct wl_pointer* p, uint32_t a, uint32_t d) { (void)data; (void)p; (void)a; (void)d; }

static const struct wl_pointer_listener pointer_listener = {
    .enter                   = pointer_enter,
    .leave                   = pointer_leave,
    .motion                  = pointer_motion,
    .button                  = pointer_button,
    .axis                    = pointer_axis,
    .frame                   = pointer_frame,
    .axis_source             = pointer_axis_source,
    .axis_stop               = pointer_axis_stop,
    .axis_discrete           = pointer_axis_discrete,
    .axis_value120           = pointer_axis_value120,
    .axis_relative_direction = pointer_axis_relative_direction,
};

// --- wl_keyboard listener — for keyboard-triggered popups + Esc. ---

static void kb_keymap(void* d, struct wl_keyboard* k, uint32_t fmt, int32_t fd, uint32_t size) {
    (void)d; (void)k; (void)fmt; (void)size;
    if (fd >= 0) close(fd);  // we don't actually use the keymap; just don't leak the fd
}
static void kb_enter(void* d, struct wl_keyboard* k, uint32_t serial,
                     struct wl_surface* surf, struct wl_array* keys) {
    (void)d; (void)k; (void)surf; (void)keys;
    g_last_serial = serial;
}
static void kb_leave(void* d, struct wl_keyboard* k, uint32_t serial, struct wl_surface* surf) {
    (void)d; (void)k; (void)surf;
    g_last_serial = serial;
}
static void kb_key(void* d, struct wl_keyboard* k, uint32_t serial, uint32_t time,
                   uint32_t key, uint32_t state) {
    (void)d; (void)k; (void)time; (void)key; (void)state;
    g_last_serial = serial;
}
static void kb_modifiers(void* d, struct wl_keyboard* k, uint32_t serial,
                         uint32_t mods_depressed, uint32_t mods_latched,
                         uint32_t mods_locked, uint32_t group) {
    (void)d; (void)k; (void)mods_depressed; (void)mods_latched;
    (void)mods_locked; (void)group;
    g_last_serial = serial;
}
static void kb_repeat_info(void* d, struct wl_keyboard* k, int32_t rate, int32_t delay) {
    (void)d; (void)k; (void)rate; (void)delay;
}

static const struct wl_keyboard_listener keyboard_listener = {
    .keymap      = kb_keymap,
    .enter       = kb_enter,
    .leave       = kb_leave,
    .key         = kb_key,
    .modifiers   = kb_modifiers,
    .repeat_info = kb_repeat_info,
};

// --- wl_seat listener — caps tell us what input devices to grab. ---

static void seat_capabilities(void* data, struct wl_seat* seat, uint32_t caps) {
    (void)data;
    if ((caps & WL_SEAT_CAPABILITY_POINTER) && !g_pointer) {
        g_pointer = wl_seat_get_pointer(seat);
        wl_pointer_add_listener(g_pointer, &pointer_listener, NULL);
    }
    if ((caps & WL_SEAT_CAPABILITY_KEYBOARD) && !g_keyboard) {
        g_keyboard = wl_seat_get_keyboard(seat);
        wl_keyboard_add_listener(g_keyboard, &keyboard_listener, NULL);
    }
}
static void seat_name(void* data, struct wl_seat* seat, const char* name) {
    (void)data; (void)seat; (void)name;
}
static const struct wl_seat_listener seat_listener = {
    .capabilities = seat_capabilities,
    .name         = seat_name,
};

// --- wl_registry listener — find and bind seat + data_device_manager.
//     Real implementation is below in registry_global_v2 (after the
//     data_device handlers are declared, so it can reference them). ---

static void registry_global_remove(void* data, struct wl_registry* reg, uint32_t name) {
    (void)data; (void)reg; (void)name;
}
// --- wl_data_offer listener — minimal, we don't care about the data. ---

static void offer_offer(void* d, struct wl_data_offer* o, const char* mime) {
    (void)d; (void)o; (void)mime;
}
static void offer_source_actions(void* d, struct wl_data_offer* o, uint32_t actions) {
    (void)d; (void)o; (void)actions;
}
static void offer_action(void* d, struct wl_data_offer* o, uint32_t action) {
    (void)d; (void)o; (void)action;
}
static const struct wl_data_offer_listener offer_listener = {
    .offer          = offer_offer,
    .source_actions = offer_source_actions,
    .action         = offer_action,
};

// --- wl_data_device listener — this is where the cross-surface drag
//     enter/leave/drop events come in. ---

static void dd_data_offer(void* d, struct wl_data_device* dd, struct wl_data_offer* offer) {
    (void)d; (void)dd;
    wl_data_offer_add_listener(offer, &offer_listener, NULL);
    g_current_offer = offer;
}
static void dd_enter(void* d, struct wl_data_device* dd, uint32_t serial,
                     struct wl_surface* surf, wl_fixed_t x, wl_fixed_t y,
                     struct wl_data_offer* offer) {
    (void)d; (void)dd; (void)x; (void)y;
    g_drag_target = surf;
    g_last_drag_target = surf;
    g_last_serial = serial;
    if (offer) {
        wl_data_offer_accept(offer, serial, "application/x-xerotty-tab");
        if (wl_data_offer_get_version(offer) >= 3) {
            wl_data_offer_set_actions(offer,
                WL_DATA_DEVICE_MANAGER_DND_ACTION_MOVE,
                WL_DATA_DEVICE_MANAGER_DND_ACTION_MOVE);
        }
    }
}
static void dd_leave(void* d, struct wl_data_device* dd) {
    (void)d; (void)dd;
    g_drag_target = NULL;
    g_current_offer = NULL;
}
static void dd_motion(void* d, struct wl_data_device* dd, uint32_t time,
                      wl_fixed_t x, wl_fixed_t y) {
    (void)d; (void)dd; (void)time; (void)x; (void)y;
}
static void dd_drop(void* d, struct wl_data_device* dd) {
    (void)d; (void)dd;
    g_drop_fired = 1;
    g_drop_target = g_drag_target;
    if (g_current_offer && wl_data_offer_get_version(g_current_offer) >= 3) {
        wl_data_offer_finish(g_current_offer);
    }
}
static void dd_selection(void* d, struct wl_data_device* dd, struct wl_data_offer* offer) {
    (void)d; (void)dd; (void)offer;
}
static const struct wl_data_device_listener dd_listener = {
    .data_offer = dd_data_offer,
    .enter      = dd_enter,
    .leave      = dd_leave,
    .motion     = dd_motion,
    .drop       = dd_drop,
    .selection  = dd_selection,
};

// --- wl_data_source listener — for OUR drag, we don't transfer real
//     data; the receiver is in-process and uses Go-side state. The
//     listener still has to be present so the compositor's source-
//     side lifecycle works. ---

static void src_target(void* d, struct wl_data_source* s, const char* mime) {
    (void)d; (void)s; (void)mime;
}
static void src_send(void* d, struct wl_data_source* s, const char* mime, int32_t fd) {
    (void)d; (void)s; (void)mime;
    // We don't transfer data — close the pipe immediately so the
    // receiver's read returns EOF.
    if (fd >= 0) close(fd);
}
static void src_cancelled(void* d, struct wl_data_source* s) {
    (void)d;
    if (!g_drop_target) {
        g_drop_target = g_drag_target ? g_drag_target : g_last_drag_target;
        if (g_drop_target) g_drop_fired = 1;
    }
    g_drag_active = 0;
    g_drag_target = NULL;
    wl_data_source_destroy(s);
}
static void src_dnd_drop_performed(void* d, struct wl_data_source* s) {
    (void)d; (void)s;
    g_drop_performed = 1;
    if (!g_drop_target) {
        g_drop_target = g_drag_target ? g_drag_target : g_last_drag_target;
        if (g_drop_target) g_drop_fired = 1;
    }
}
static void src_dnd_finished(void* d, struct wl_data_source* s) {
    (void)d;
    if (g_drop_performed && !g_drop_target) {
        g_drop_target = g_drag_target ? g_drag_target : g_last_drag_target;
        if (g_drop_target) g_drop_fired = 1;
    }
    g_drag_active = 0;
    g_drag_target = NULL;
    wl_data_source_destroy(s);
}
static void src_action(void* d, struct wl_data_source* s, uint32_t action) {
    (void)d; (void)s; (void)action;
}
static const struct wl_data_source_listener src_listener = {
    .target              = src_target,
    .send                = src_send,
    .cancelled           = src_cancelled,
    .dnd_drop_performed  = src_dnd_drop_performed,
    .dnd_finished        = src_dnd_finished,
    .action              = src_action,
};

// Registry handler reads compositor globals and binds the seat and
// data_device manager.
static void registry_global_v2(void* d, struct wl_registry* reg, uint32_t name,
                               const char* iface, uint32_t version) {
    (void)d;
    if (strcmp(iface, wl_seat_interface.name) == 0 && !g_seat) {
        uint32_t v = version > 8 ? 8 : version;
        g_seat = (struct wl_seat*)wl_registry_bind(reg, name,
                                                   &wl_seat_interface, v);
        wl_seat_add_listener(g_seat, &seat_listener, NULL);
    } else if (strcmp(iface, "wl_data_device_manager") == 0 && !g_ddm) {
        uint32_t v = version > 3 ? 3 : version;
        g_ddm = (struct wl_data_device_manager*)wl_registry_bind(
            reg, name, &wl_data_device_manager_interface, v);
    }
}

static const struct wl_registry_listener registry_listener = {
    .global        = registry_global_v2,
    .global_remove = registry_global_remove,
};

int wlgrab_init(void* display) {
    if (g_initialized) return 1;
    if (!display) return 0;
    g_display = (struct wl_display*)display;

    struct wl_registry* registry = wl_display_get_registry(g_display);
    wl_registry_add_listener(registry, &registry_listener, NULL);
    // One roundtrip → globals arrive → registry_global fires → seat bound.
    // Second roundtrip → seat_capabilities fires → pointer/keyboard bound.
    wl_display_roundtrip(g_display);
    wl_display_roundtrip(g_display);
    wl_registry_destroy(registry);

    if (!g_seat) {
        fprintf(stderr, "wlgrab: no wl_seat advertised by compositor\n");
        return 0;
    }
    // Optional: bind a data_device for cross-surface drag support.
    // Compositors that don't advertise wl_data_device_manager just
    // mean no drag-and-drop — degrades gracefully.
    if (g_ddm) {
        g_dd = wl_data_device_manager_get_data_device(g_ddm, g_seat);
        wl_data_device_add_listener(g_dd, &dd_listener, NULL);
    }
    g_initialized = 1;
    return 1;
}

int wldrag_start(void* origin_surface) {
    if (!g_initialized || !g_dd || !origin_surface) return 0;
    if (g_last_serial == 0) return 0;
    struct wl_data_source* src = wl_data_device_manager_create_data_source(g_ddm);
    if (!src) return 0;
    wl_data_source_offer(src, "application/x-xerotty-tab");
    if (wl_data_source_get_version(src) >= 3) {
        wl_data_source_set_actions(src, WL_DATA_DEVICE_MANAGER_DND_ACTION_MOVE);
    }
    wl_data_source_add_listener(src, &src_listener, NULL);
    wl_data_device_start_drag(g_dd, src, (struct wl_surface*)origin_surface,
                              NULL, g_last_serial);
    wl_display_flush(g_display);
    g_drag_active = 1;
    g_drop_fired = 0;
    g_drop_performed = 0;
    g_drag_origin = (struct wl_surface*)origin_surface;
    g_drag_target = NULL;
    g_drop_target = NULL;
    g_last_drag_target = NULL;
    return 1;
}

void* wldrag_target_surface(void) { return g_drag_target; }

int wldrag_drop_fired(void) {
    int v = g_drop_fired;
    g_drop_fired = 0;
    return v;
}

void* wldrag_drop_target_surface(void) { return g_drop_target; }

int wldrag_active(void) { return g_drag_active; }

int wlgrab_popup(void* xdg_popup_ptr) {
    if (!g_initialized || !g_seat || !xdg_popup_ptr) {
        return 0;
    }
    if (g_last_serial == 0) {
        // grab(0) gets rejected by compositors. The user must have
        // generated at least one input event before opening the popup
        // — right-click counts. If we hit this in practice it means
        // our listeners aren't seeing events, which is a bug worth
        // surfacing.
        fprintf(stderr, "wlgrab_popup: no input serial captured yet\n");
        return 0;
    }
    struct xdg_popup* popup = (struct xdg_popup*)xdg_popup_ptr;
    xdg_popup_grab(popup, g_seat, g_last_serial);
    wl_display_flush(g_display);
    return 1;
}

uint32_t wlgrab_last_serial(void) { return g_last_serial; }

void wlgrab_shutdown(void) {
    if (g_pointer)  { wl_pointer_release(g_pointer);   g_pointer = NULL; }
    if (g_keyboard) { wl_keyboard_release(g_keyboard); g_keyboard = NULL; }
    if (g_seat)     { wl_seat_release(g_seat);         g_seat = NULL; }
    g_initialized = 0;
}
