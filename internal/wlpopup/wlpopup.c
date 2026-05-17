// Native Wayland xdg_popup-backed context menu.
//
// Why this exists: SDL2 (including via sdl2-compat → SDL3) creates
// every window as xdg_toplevel. Compositors don't grant "click
// anywhere outside dismisses" to toplevels — that's reserved for
// xdg_popup, the protocol designed for menus. Without xdg_popup the
// menu has no way to know when the user clicked another app.
//
// What this does: extract the wl_display + parent surface from
// SDL_SysWMinfo (in Go side), bind our own wl_compositor / xdg_wm_base
// / wl_shm / wl_seat globals, then create a child xdg_popup of the
// caller's surface. Renders the menu items to a wl_shm-backed buffer
// with cairo. The compositor delivers popup_done when the user
// dismisses by clicking outside.

#include "wlpopup.h"
#include "xdg-shell-client-protocol.h"

#include <wayland-client.h>
#include <cairo/cairo.h>

#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

// -- Globals state ---------------------------------------------------

// We share the application's wl_display with SDL but want our own
// proxies to dispatch events to a custom queue so we don't race with
// SDL's main loop dispatch. All proxies we create get assigned to
// g.queue via wl_proxy_set_queue.
static struct {
    struct wl_display *display;
    struct wl_event_queue *queue;
    struct wl_registry *registry;
    struct wl_compositor *compositor;
    struct xdg_wm_base *xdg_wm_base;
    struct wl_shm *shm;
    struct wl_seat *seat;
    struct wl_pointer *pointer;

    struct wl_surface *parent_surface;
    struct xdg_surface *parent_xdg;

    int initialized;

    // Active popup (only one at a time).
    struct wl_surface *popup_surface;
    struct xdg_surface *popup_xdg_surface;
    struct xdg_popup *popup;
    struct xdg_positioner *positioner;
    struct wl_buffer *buffer;
    void *buffer_data;
    int buffer_w, buffer_h, buffer_stride, buffer_size;

    // Menu data (caller-owned strings, valid for popup lifetime).
    wlpopup_item *items;
    int item_count;

    // Layout: per-item Y range + row height.
    int row_h;
    int padding_x, padding_y;
    int item_y_offsets[256]; // top Y for each item; size limit good enough for our menus

    // Pointer state.
    int pointer_in_popup;
    double pointer_x, pointer_y;
    int hovered_index; // -1 if none

    // Result.
    int popup_id;     // 1+ when open, 0 when closed
    int next_popup_id;
    int dismissed;    // 1 when popup_done fired
    char selected_action[256]; // empty if dismissed without selection
    int has_selection;

    // Sync state for xdg_surface.configure.
    int surface_configured;
} g;

// -- wl_pointer listener ---------------------------------------------

static void pointer_enter(void *data, struct wl_pointer *p, uint32_t serial,
        struct wl_surface *surface, wl_fixed_t sx, wl_fixed_t sy) {
    (void)data; (void)p; (void)serial;
    if (surface == g.popup_surface) {
        g.pointer_in_popup = 1;
        g.pointer_x = wl_fixed_to_double(sx);
        g.pointer_y = wl_fixed_to_double(sy);
    } else {
        g.pointer_in_popup = 0;
    }
}

static void pointer_leave(void *data, struct wl_pointer *p, uint32_t serial,
        struct wl_surface *surface) {
    (void)data; (void)p; (void)serial;
    if (surface == g.popup_surface) {
        g.pointer_in_popup = 0;
        g.hovered_index = -1;
    }
}

// Returns the index of the menu item at popup-local (x, y), or -1.
static int item_at(double x, double y) {
    if (x < 0 || x >= g.buffer_w) return -1;
    if (y < g.padding_y) return -1;
    for (int i = 0; i < g.item_count; i++) {
        int top = g.item_y_offsets[i];
        int bot = (i + 1 < g.item_count) ? g.item_y_offsets[i + 1] : (g.buffer_h - g.padding_y);
        if (y >= top && y < bot) {
            if (g.items[i].type == 0) return i; // only regular items are selectable
            return -1;
        }
    }
    return -1;
}

static void render_buffer(void); // forward

static void pointer_motion(void *data, struct wl_pointer *p, uint32_t time,
        wl_fixed_t sx, wl_fixed_t sy) {
    (void)data; (void)p; (void)time;
    if (!g.pointer_in_popup) return;
    g.pointer_x = wl_fixed_to_double(sx);
    g.pointer_y = wl_fixed_to_double(sy);
    int idx = item_at(g.pointer_x, g.pointer_y);
    if (idx != g.hovered_index) {
        g.hovered_index = idx;
        render_buffer();
    }
}

static void pointer_button(void *data, struct wl_pointer *p, uint32_t serial,
        uint32_t time, uint32_t button, uint32_t state) {
    (void)data; (void)p; (void)serial; (void)time;
    if (state != WL_POINTER_BUTTON_STATE_PRESSED) return;
    if (button != 0x110 /* BTN_LEFT */) return;
    if (!g.pointer_in_popup) return;
    int idx = item_at(g.pointer_x, g.pointer_y);
    if (idx >= 0) {
        const char *a = g.items[idx].action;
        if (a) {
            strncpy(g.selected_action, a, sizeof(g.selected_action) - 1);
            g.selected_action[sizeof(g.selected_action) - 1] = '\0';
            g.has_selection = 1;
        }
        g.dismissed = 1;
        // Compositor will tear down the popup; we'll clean up on
        // popup_done callback.
    }
}

static void pointer_axis(void *data, struct wl_pointer *p, uint32_t t, uint32_t a, wl_fixed_t v) {
    (void)data; (void)p; (void)t; (void)a; (void)v;
}
static void pointer_frame(void *data, struct wl_pointer *p) { (void)data; (void)p; }
static void pointer_axis_source(void *data, struct wl_pointer *p, uint32_t s) { (void)data; (void)p; (void)s; }
static void pointer_axis_stop(void *data, struct wl_pointer *p, uint32_t t, uint32_t a) { (void)data; (void)p; (void)t; (void)a; }
static void pointer_axis_discrete(void *data, struct wl_pointer *p, uint32_t a, int32_t d) { (void)data; (void)p; (void)a; (void)d; }
static void pointer_axis_value120(void *data, struct wl_pointer *p, uint32_t a, int32_t v) { (void)data; (void)p; (void)a; (void)v; }
static void pointer_axis_relative_direction(void *data, struct wl_pointer *p, uint32_t a, uint32_t d) { (void)data; (void)p; (void)a; (void)d; }

static const struct wl_pointer_listener pointer_listener = {
    .enter = pointer_enter,
    .leave = pointer_leave,
    .motion = pointer_motion,
    .button = pointer_button,
    .axis = pointer_axis,
    .frame = pointer_frame,
    .axis_source = pointer_axis_source,
    .axis_stop = pointer_axis_stop,
    .axis_discrete = pointer_axis_discrete,
    .axis_value120 = pointer_axis_value120,
    .axis_relative_direction = pointer_axis_relative_direction,
};

// -- wl_seat listener ------------------------------------------------

static void seat_capabilities(void *data, struct wl_seat *seat, uint32_t caps) {
    (void)data;
    if ((caps & WL_SEAT_CAPABILITY_POINTER) && !g.pointer) {
        g.pointer = wl_seat_get_pointer(seat);
        wl_proxy_set_queue((struct wl_proxy *)g.pointer, g.queue);
        wl_pointer_add_listener(g.pointer, &pointer_listener, NULL);
    }
}

static void seat_name(void *data, struct wl_seat *seat, const char *name) {
    (void)data; (void)seat; (void)name;
}

static const struct wl_seat_listener seat_listener = {
    .capabilities = seat_capabilities,
    .name = seat_name,
};

// -- xdg_wm_base listener (ping/pong keepalive) ----------------------

static void xdg_wm_base_ping(void *data, struct xdg_wm_base *b, uint32_t serial) {
    (void)data;
    xdg_wm_base_pong(b, serial);
}

static const struct xdg_wm_base_listener wm_base_listener = {
    .ping = xdg_wm_base_ping,
};

// -- wl_registry listener (binds globals we need) --------------------

static void registry_global(void *data, struct wl_registry *registry, uint32_t name,
        const char *interface, uint32_t version) {
    (void)data;
    if (!strcmp(interface, "wl_compositor")) {
        uint32_t v = version > 4 ? 4 : version;
        g.compositor = wl_registry_bind(registry, name, &wl_compositor_interface, v);
        wl_proxy_set_queue((struct wl_proxy *)g.compositor, g.queue);
    } else if (!strcmp(interface, "xdg_wm_base")) {
        uint32_t v = version > 3 ? 3 : version;
        g.xdg_wm_base = wl_registry_bind(registry, name, &xdg_wm_base_interface, v);
        wl_proxy_set_queue((struct wl_proxy *)g.xdg_wm_base, g.queue);
        xdg_wm_base_add_listener(g.xdg_wm_base, &wm_base_listener, NULL);
    } else if (!strcmp(interface, "wl_shm")) {
        g.shm = wl_registry_bind(registry, name, &wl_shm_interface, 1);
        wl_proxy_set_queue((struct wl_proxy *)g.shm, g.queue);
    } else if (!strcmp(interface, "wl_seat")) {
        uint32_t v = version > 7 ? 7 : version;
        g.seat = wl_registry_bind(registry, name, &wl_seat_interface, v);
        wl_proxy_set_queue((struct wl_proxy *)g.seat, g.queue);
        wl_seat_add_listener(g.seat, &seat_listener, NULL);
    }
}

static void registry_global_remove(void *data, struct wl_registry *registry, uint32_t name) {
    (void)data; (void)registry; (void)name;
}

static const struct wl_registry_listener registry_listener = {
    .global = registry_global,
    .global_remove = registry_global_remove,
};

// -- shm buffer creation ---------------------------------------------

static int alloc_shm(int size) {
    // mkstemp + manual FD_CLOEXEC instead of mkostemp to avoid the
    // _GNU_SOURCE feature-test dance — mkstemp is POSIX, mkostemp is
    // a GNU extension that some toolchain configurations don't see
    // even on Linux without the right macros.
    char tmpl[] = "/tmp/xerotty-wlpopup-XXXXXX";
    int fd = mkstemp(tmpl);
    if (fd < 0) return -1;
    unlink(tmpl);
    int flags = fcntl(fd, F_GETFD);
    if (flags != -1) fcntl(fd, F_SETFD, flags | FD_CLOEXEC);
    if (ftruncate(fd, size) < 0) {
        close(fd);
        return -1;
    }
    return fd;
}

static int create_buffer(int w, int h) {
    int stride = w * 4;
    int size = stride * h;
    int fd = alloc_shm(size);
    if (fd < 0) return -1;
    void *data = mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    if (data == MAP_FAILED) {
        close(fd);
        return -1;
    }
    struct wl_shm_pool *pool = wl_shm_create_pool(g.shm, fd, size);
    wl_proxy_set_queue((struct wl_proxy *)pool, g.queue);
    struct wl_buffer *buffer = wl_shm_pool_create_buffer(pool, 0, w, h, stride,
            WL_SHM_FORMAT_ARGB8888);
    wl_proxy_set_queue((struct wl_proxy *)buffer, g.queue);
    wl_shm_pool_destroy(pool);
    close(fd);
    g.buffer = buffer;
    g.buffer_data = data;
    g.buffer_w = w;
    g.buffer_h = h;
    g.buffer_stride = stride;
    g.buffer_size = size;
    return 0;
}

// -- rendering -------------------------------------------------------

static void render_buffer(void) {
    if (!g.buffer_data) return;
    cairo_surface_t *cs = cairo_image_surface_create_for_data(
        (unsigned char *)g.buffer_data,
        CAIRO_FORMAT_ARGB32, g.buffer_w, g.buffer_h, g.buffer_stride);
    cairo_t *cr = cairo_create(cs);

    // Background.
    cairo_set_source_rgba(cr, 0.16, 0.16, 0.18, 1.0);
    cairo_paint(cr);

    // Border.
    cairo_set_source_rgba(cr, 0.30, 0.30, 0.33, 1.0);
    cairo_set_line_width(cr, 1.0);
    cairo_rectangle(cr, 0.5, 0.5, g.buffer_w - 1, g.buffer_h - 1);
    cairo_stroke(cr);

    cairo_select_font_face(cr, "sans-serif", CAIRO_FONT_SLANT_NORMAL, CAIRO_FONT_WEIGHT_NORMAL);
    cairo_set_font_size(cr, 13);

    for (int i = 0; i < g.item_count; i++) {
        int top = g.item_y_offsets[i];
        int bot = (i + 1 < g.item_count) ? g.item_y_offsets[i + 1] : (g.buffer_h - g.padding_y);
        if (g.items[i].type == 1) {
            // Separator
            cairo_set_source_rgba(cr, 0.30, 0.30, 0.33, 1.0);
            cairo_set_line_width(cr, 1.0);
            double y = top + (bot - top) / 2.0;
            cairo_move_to(cr, g.padding_x, y);
            cairo_line_to(cr, g.buffer_w - g.padding_x, y);
            cairo_stroke(cr);
            continue;
        }
        int hovered = (i == g.hovered_index) && g.items[i].type == 0;
        if (hovered) {
            cairo_set_source_rgba(cr, 0.20, 0.40, 0.80, 1.0);
            cairo_rectangle(cr, 1, top, g.buffer_w - 2, bot - top);
            cairo_fill(cr);
        }
        // Item label.
        if (g.items[i].type == 2) {
            cairo_set_source_rgba(cr, 0.50, 0.50, 0.52, 1.0); // dim
        } else if (hovered) {
            cairo_set_source_rgba(cr, 1.0, 1.0, 1.0, 1.0);
        } else {
            cairo_set_source_rgba(cr, 0.88, 0.88, 0.90, 1.0);
        }
        cairo_text_extents_t te;
        const char *label = g.items[i].label ? g.items[i].label : "";
        cairo_text_extents(cr, label, &te);
        double tx = g.padding_x;
        double ty = top + (bot - top) / 2.0 + te.height / 2.0 - te.y_bearing - te.height / 2.0;
        // Simpler vertical centering: middle of row + half-text-height
        ty = top + (bot - top + te.height) / 2.0;
        cairo_move_to(cr, tx, ty);
        cairo_show_text(cr, label);
    }

    cairo_destroy(cr);
    cairo_surface_destroy(cs);

    if (g.popup_surface && g.buffer) {
        wl_surface_attach(g.popup_surface, g.buffer, 0, 0);
        wl_surface_damage_buffer(g.popup_surface, 0, 0, g.buffer_w, g.buffer_h);
        wl_surface_commit(g.popup_surface);
    }
}

// Compute popup size + per-item Y offsets given the items.
static void compute_layout(void) {
    g.row_h = 22;
    g.padding_x = 12;
    g.padding_y = 6;
    int sep_h = 8;

    // Measure widest label.
    cairo_surface_t *cs = cairo_image_surface_create(CAIRO_FORMAT_ARGB32, 1, 1);
    cairo_t *cr = cairo_create(cs);
    cairo_select_font_face(cr, "sans-serif", CAIRO_FONT_SLANT_NORMAL, CAIRO_FONT_WEIGHT_NORMAL);
    cairo_set_font_size(cr, 13);
    int max_w = 100;
    for (int i = 0; i < g.item_count; i++) {
        if (g.items[i].type == 1) continue;
        cairo_text_extents_t te;
        cairo_text_extents(cr, g.items[i].label ? g.items[i].label : "", &te);
        int w = (int)(te.x_advance + 0.5);
        if (w > max_w) max_w = w;
    }
    cairo_destroy(cr);
    cairo_surface_destroy(cs);

    g.buffer_w = max_w + g.padding_x * 2;

    int y = g.padding_y;
    for (int i = 0; i < g.item_count && i < 256; i++) {
        g.item_y_offsets[i] = y;
        if (g.items[i].type == 1) {
            y += sep_h;
        } else {
            y += g.row_h;
        }
    }
    g.buffer_h = y + g.padding_y;
}

// -- xdg_surface configure ack ---------------------------------------

static void popup_xdg_surface_configure(void *data, struct xdg_surface *xdg, uint32_t serial) {
    (void)data;
    xdg_surface_ack_configure(xdg, serial);
    g.surface_configured = 1;
    render_buffer();
}

static const struct xdg_surface_listener popup_xdg_surface_listener = {
    .configure = popup_xdg_surface_configure,
};

// -- xdg_popup events ------------------------------------------------

static void popup_configure(void *data, struct xdg_popup *p,
        int32_t x, int32_t y, int32_t width, int32_t height) {
    (void)data; (void)p; (void)x; (void)y; (void)width; (void)height;
}

static void popup_popup_done(void *data, struct xdg_popup *p) {
    (void)data; (void)p;
    g.dismissed = 1;
}

static void popup_repositioned(void *data, struct xdg_popup *p, uint32_t token) {
    (void)data; (void)p; (void)token;
}

static const struct xdg_popup_listener popup_listener = {
    .configure = popup_configure,
    .popup_done = popup_popup_done,
    .repositioned = popup_repositioned,
};

// -- public API ------------------------------------------------------

int wlpopup_init(void *display, void *parent_surface, void *parent_xdg) {
    if (g.initialized) return 0;
    if (!display || !parent_surface || !parent_xdg) return 1;
    memset(&g, 0, sizeof(g));
    g.display = (struct wl_display *)display;
    g.parent_surface = (struct wl_surface *)parent_surface;
    g.parent_xdg = (struct xdg_surface *)parent_xdg;
    g.next_popup_id = 1;
    g.hovered_index = -1;

    g.queue = wl_display_create_queue(g.display);
    if (!g.queue) return 2;

    g.registry = wl_display_get_registry(g.display);
    wl_proxy_set_queue((struct wl_proxy *)g.registry, g.queue);
    wl_registry_add_listener(g.registry, &registry_listener, NULL);

    // Round-trip on our queue to receive the globals.
    if (wl_display_roundtrip_queue(g.display, g.queue) < 0) return 3;
    // Second round-trip: seat capabilities arrive after seat is bound,
    // so we need another pass to learn about wl_pointer.
    if (wl_display_roundtrip_queue(g.display, g.queue) < 0) return 4;

    if (!g.compositor || !g.xdg_wm_base || !g.shm) return 5;

    g.initialized = 1;
    return 0;
}

int wlpopup_available(void) {
    return g.initialized ? 1 : 0;
}

static void cleanup_popup(void) {
    if (g.popup) {
        xdg_popup_destroy(g.popup);
        g.popup = NULL;
    }
    if (g.popup_xdg_surface) {
        xdg_surface_destroy(g.popup_xdg_surface);
        g.popup_xdg_surface = NULL;
    }
    if (g.popup_surface) {
        wl_surface_destroy(g.popup_surface);
        g.popup_surface = NULL;
    }
    if (g.positioner) {
        xdg_positioner_destroy(g.positioner);
        g.positioner = NULL;
    }
    if (g.buffer) {
        wl_buffer_destroy(g.buffer);
        g.buffer = NULL;
    }
    if (g.buffer_data) {
        munmap(g.buffer_data, g.buffer_size);
        g.buffer_data = NULL;
    }
    if (g.items) {
        // We strdup'd labels/actions in show; free them.
        for (int i = 0; i < g.item_count; i++) {
            free((void *)g.items[i].label);
            free((void *)g.items[i].action);
        }
        free(g.items);
        g.items = NULL;
        g.item_count = 0;
    }
    g.surface_configured = 0;
    g.pointer_in_popup = 0;
    g.hovered_index = -1;
    g.popup_id = 0;
}

int wlpopup_show(int x, int y, const wlpopup_item *items, int n) {
    if (!g.initialized) return 0;
    if (n <= 0 || n > 256) return 0;
    if (g.popup_id != 0) {
        // Dismiss any active popup first.
        cleanup_popup();
    }

    // Copy items so caller's pointers don't have to outlive the popup.
    g.items = calloc(n, sizeof(wlpopup_item));
    g.item_count = n;
    for (int i = 0; i < n; i++) {
        g.items[i].type = items[i].type;
        g.items[i].label = items[i].label ? strdup(items[i].label) : NULL;
        g.items[i].action = items[i].action ? strdup(items[i].action) : NULL;
    }

    g.dismissed = 0;
    g.has_selection = 0;
    g.selected_action[0] = '\0';
    g.hovered_index = -1;
    compute_layout();

    g.popup_surface = wl_compositor_create_surface(g.compositor);
    wl_proxy_set_queue((struct wl_proxy *)g.popup_surface, g.queue);
    g.popup_xdg_surface = xdg_wm_base_get_xdg_surface(g.xdg_wm_base, g.popup_surface);
    wl_proxy_set_queue((struct wl_proxy *)g.popup_xdg_surface, g.queue);
    xdg_surface_add_listener(g.popup_xdg_surface, &popup_xdg_surface_listener, NULL);

    g.positioner = xdg_wm_base_create_positioner(g.xdg_wm_base);
    wl_proxy_set_queue((struct wl_proxy *)g.positioner, g.queue);
    xdg_positioner_set_size(g.positioner, g.buffer_w, g.buffer_h);
    xdg_positioner_set_anchor_rect(g.positioner, x, y, 1, 1);
    xdg_positioner_set_anchor(g.positioner, XDG_POSITIONER_ANCHOR_BOTTOM_RIGHT);
    xdg_positioner_set_gravity(g.positioner, XDG_POSITIONER_GRAVITY_BOTTOM_RIGHT);
    xdg_positioner_set_constraint_adjustment(g.positioner,
        XDG_POSITIONER_CONSTRAINT_ADJUSTMENT_SLIDE_X |
        XDG_POSITIONER_CONSTRAINT_ADJUSTMENT_SLIDE_Y |
        XDG_POSITIONER_CONSTRAINT_ADJUSTMENT_FLIP_X |
        XDG_POSITIONER_CONSTRAINT_ADJUSTMENT_FLIP_Y);

    g.popup = xdg_surface_get_popup(g.popup_xdg_surface, g.parent_xdg, g.positioner);
    wl_proxy_set_queue((struct wl_proxy *)g.popup, g.queue);
    xdg_popup_add_listener(g.popup, &popup_listener, NULL);

    // Grab the pointer / keyboard via xdg_popup.grab so the compositor
    // delivers popup_done on outside clicks. Need a serial — use 0
    // since labwc accepts a synthetic one for the initial grab on
    // wlroots-based compositors. (Strict compositors might require a
    // real input event serial; if dismiss-on-outside-click misbehaves
    // on those, threading the latest pointer serial through here is
    // the fix.)
    if (g.seat) {
        xdg_popup_grab(g.popup, g.seat, 0);
    }

    if (create_buffer(g.buffer_w, g.buffer_h) != 0) {
        cleanup_popup();
        return 0;
    }

    wl_surface_commit(g.popup_surface);

    g.popup_id = g.next_popup_id++;
    return g.popup_id;
}

int wlpopup_poll(int popup_id, const char **action_out) {
    if (g.popup_id != popup_id || popup_id == 0) {
        return 1; // dismissed (already cleaned up or never opened)
    }
    if (g.dismissed) {
        int result = g.has_selection ? 2 : 1;
        if (action_out) *action_out = g.has_selection ? g.selected_action : NULL;
        // Caller is expected to consume the result; clean up after we hand it back.
        // We keep g.selected_action stable until next show.
        char saved_action[256];
        int saved_has_sel = g.has_selection;
        memcpy(saved_action, g.selected_action, sizeof(saved_action));
        cleanup_popup();
        memcpy(g.selected_action, saved_action, sizeof(g.selected_action));
        g.has_selection = saved_has_sel;
        return result;
    }
    return 0;
}

void wlpopup_dismiss(int popup_id) {
    if (g.popup_id != popup_id || popup_id == 0) return;
    cleanup_popup();
}

void wlpopup_pump(void) {
    if (!g.initialized) return;
    // Drain queued events without blocking. Non-blocking dispatch is
    // crucial — the caller's main render loop runs us per-frame and
    // can't afford to stall waiting for compositor events.
    while (wl_display_prepare_read_queue(g.display, g.queue) != 0) {
        wl_display_dispatch_queue_pending(g.display, g.queue);
    }
    wl_display_flush(g.display);
    // Don't actually read — let SDL's dispatch handle the wl_display
    // fd. We just process events that have already been queued for
    // our queue.
    wl_display_cancel_read(g.display);
    wl_display_dispatch_queue_pending(g.display, g.queue);
}
