// wlclip — see wlclip.h. Compiled as plain C (cgo CFLAGS).

#define _GNU_SOURCE
#include "wlclip.h"

#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include <wayland-client.h>
#include "ext-data-control-v1-client-protocol.h"
#include "wlr-data-control-unstable-v1-client-protocol.h"

// Defined in sdl3.cpp (extern "C"): wake the render loop when the
// selection changes so a paste right after a copy sees fresh state.
extern void platform_post_wake(void);

// ---- data-control flavour -------------------------------------------
//
// ext-data-control-v1 and wlr-data-control-unstable-v1 have the same
// objects, requests and events; only the interface names differ.
// Everything below goes through these thin adapters so the rest of
// the file is flavour-agnostic.

enum flavour { FLAVOUR_NONE = 0, FLAVOUR_EXT, FLAVOUR_WLR };

// g_mu guards every g_* below and the offer/source objects they point
// to. Held briefly: never across a pipe read or a thread join.
static pthread_mutex_t    g_mu = PTHREAD_MUTEX_INITIALIZER;
static int                g_initialized = 0;
static int                g_available   = 0;
static uint64_t           g_generation  = 0;
static enum flavour       g_flavour     = FLAVOUR_NONE;
static struct wl_display* g_display     = NULL;
static struct wl_seat*    g_seat        = NULL;

static struct ext_data_control_manager_v1*  g_ext_mgr = NULL;
static struct ext_data_control_device_v1*   g_ext_dev = NULL;
static struct zwlr_data_control_manager_v1* g_wlr_mgr = NULL;
static struct zwlr_data_control_device_v1*  g_wlr_dev = NULL;

static const int DEFAULT_TIMEOUT_MS = 5000;

// Text MIME types, in the order we look for them and the set we
// advertise. First is what everything modern offers; the X11-era
// atoms keep XWayland apps pasting.
static const char* const TEXT_MIMES[] = {
    "text/plain;charset=utf-8", "UTF8_STRING", "text/plain", "TEXT", "STRING",
};
static const int N_TEXT_MIMES = (int)(sizeof TEXT_MIMES / sizeof TEXT_MIMES[0]);

// ---- offers ----------------------------------------------------------

struct offer {
    void*   proxy;      // ext_data_control_offer_v1* or zwlr_data_control_offer_v1*
    char**  mimes;
    int     n_mimes;
    int     cap_mimes;
};

static struct offer* g_selection = NULL; // current clipboard offer, or NULL

static struct offer* offer_new(void* proxy) {
    struct offer* o = (struct offer*)calloc(1, sizeof *o);
    if (o) o->proxy = proxy;
    return o;
}

static void offer_add_mime(struct offer* o, const char* mime) {
    if (!o || !mime) return;
    if (o->n_mimes == o->cap_mimes) {
        int ncap = o->cap_mimes ? o->cap_mimes * 2 : 8;
        char** nm = (char**)realloc(o->mimes, (size_t)ncap * sizeof *nm);
        if (!nm) return;
        o->mimes = nm;
        o->cap_mimes = ncap;
    }
    o->mimes[o->n_mimes++] = strdup(mime);
}

static int offer_has_mime(const struct offer* o, const char* mime) {
    if (!o || !mime) return 0;
    for (int i = 0; i < o->n_mimes; i++)
        if (strcmp(o->mimes[i], mime) == 0) return 1;
    return 0;
}

static void offer_free(struct offer* o) {
    if (!o) return;
    if (o->proxy) {
        if (g_flavour == FLAVOUR_EXT)
            ext_data_control_offer_v1_destroy((struct ext_data_control_offer_v1*)o->proxy);
        else
            zwlr_data_control_offer_v1_destroy((struct zwlr_data_control_offer_v1*)o->proxy);
    }
    for (int i = 0; i < o->n_mimes; i++) free(o->mimes[i]);
    free(o->mimes);
    free(o);
}

static void offer_receive(const struct offer* o, const char* mime, int fd) {
    if (g_flavour == FLAVOUR_EXT)
        ext_data_control_offer_v1_receive((struct ext_data_control_offer_v1*)o->proxy, mime, fd);
    else
        zwlr_data_control_offer_v1_receive((struct zwlr_data_control_offer_v1*)o->proxy, mime, fd);
}

// offer.offer event: one MIME type the owner can provide.
static void ext_offer_offer(void* data, struct ext_data_control_offer_v1* p, const char* mime) {
    (void)p;
    pthread_mutex_lock(&g_mu);
    offer_add_mime((struct offer*)data, mime);
    pthread_mutex_unlock(&g_mu);
}
static void wlr_offer_offer(void* data, struct zwlr_data_control_offer_v1* p, const char* mime) {
    (void)p;
    pthread_mutex_lock(&g_mu);
    offer_add_mime((struct offer*)data, mime);
    pthread_mutex_unlock(&g_mu);
}
static const struct ext_data_control_offer_v1_listener  ext_offer_listener = { ext_offer_offer };
static const struct zwlr_data_control_offer_v1_listener wlr_offer_listener = { wlr_offer_offer };

// ---- our own source --------------------------------------------------

struct store_item { char* mime; unsigned char* data; size_t len; };

// store holds the bytes behind one of our sources. Refcounted because
// send threads outlive the source: a slow reader may still be draining
// after the compositor has cancelled us.
struct store {
    struct store_item* items;
    int                n;
    int                refs;      // 1 for the source + 1 per in-flight send thread
    pthread_mutex_t    mu;
};

struct source {
    void*         proxy;  // ext_data_control_source_v1* or zwlr_data_control_source_v1*
    struct store* store;
};

static struct source* g_source = NULL; // our current source, NULL when we don't own

static void store_release(struct store* s) {
    pthread_mutex_lock(&s->mu);
    int left = --s->refs;
    pthread_mutex_unlock(&s->mu);
    if (left > 0) return;
    for (int i = 0; i < s->n; i++) { free(s->items[i].mime); free(s->items[i].data); }
    free(s->items);
    pthread_mutex_destroy(&s->mu);
    free(s);
}

static void store_retain(struct store* s) {
    pthread_mutex_lock(&s->mu);
    s->refs++;
    pthread_mutex_unlock(&s->mu);
}

static const struct store_item* store_find(const struct store* s, const char* mime) {
    for (int i = 0; i < s->n; i++)
        if (strcmp(s->items[i].mime, mime) == 0) return &s->items[i];
    return NULL;
}

// write_all pushes data into the paste pipe. A reader that gives up
// early (wl-paste's timeout, a crashed app) closes its end and the
// next write raises SIGPIPE, which would kill the whole GUI. Block it
// for the duration, let write fail with EPIPE instead, and swallow the
// signal left pending before restoring the mask.
static void write_all(int fd, const unsigned char* data, size_t len) {
    sigset_t pipe_set, old_set;
    sigemptyset(&pipe_set);
    sigaddset(&pipe_set, SIGPIPE);
    pthread_sigmask(SIG_BLOCK, &pipe_set, &old_set);

    size_t off = 0;
    while (off < len) {
        ssize_t w = write(fd, data + off, len - off);
        if (w < 0) {
            if (errno == EINTR) continue;
            if (errno == EAGAIN) {
                struct pollfd pfd = { fd, POLLOUT, 0 };
                if (poll(&pfd, 1, DEFAULT_TIMEOUT_MS) <= 0) break;
                continue;
            }
            break; // EPIPE: reader went away
        }
        off += (size_t)w;
    }

    if (!sigismember(&old_set, SIGPIPE)) {
        struct timespec zero = { 0, 0 };
        siginfo_t info;
        while (sigtimedwait(&pipe_set, &info, &zero) > 0) { /* consumed */ }
        pthread_sigmask(SIG_SETMASK, &old_set, NULL);
    }
}

struct send_job { struct store* store; int fd; char* mime; };

static void* send_thread(void* arg) {
    struct send_job* job = (struct send_job*)arg;
    const struct store_item* it = store_find(job->store, job->mime);
    if (it) write_all(job->fd, it->data, it->len);
    close(job->fd);
    store_release(job->store);
    free(job->mime);
    free(job);
    return NULL;
}

// source_send runs under g_mu (called from a listener).
static void source_send(struct source* src, const char* mime, int fd) {
    if (!src || !src->store) { close(fd); return; }
    struct send_job* job = (struct send_job*)calloc(1, sizeof *job);
    if (!job) { close(fd); return; }
    store_retain(src->store);
    job->store = src->store;
    job->fd = fd;
    job->mime = strdup(mime ? mime : "");
    pthread_t th;
    pthread_attr_t attr;
    pthread_attr_init(&attr);
    pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
    if (pthread_create(&th, &attr, send_thread, job) != 0) {
        // Fall back to a blocking write on this thread rather than
        // dropping the paste.
        send_thread(job);
    }
    pthread_attr_destroy(&attr);
}

// source_free runs under g_mu.
static void source_free(struct source* src) {
    if (!src) return;
    if (src->proxy) {
        if (g_flavour == FLAVOUR_EXT)
            ext_data_control_source_v1_destroy((struct ext_data_control_source_v1*)src->proxy);
        else
            zwlr_data_control_source_v1_destroy((struct zwlr_data_control_source_v1*)src->proxy);
    }
    if (src->store) store_release(src->store);
    free(src);
}

static void source_cancelled(struct source* src) {
    pthread_mutex_lock(&g_mu);
    if (g_source == src) g_source = NULL;
    source_free(src);
    pthread_mutex_unlock(&g_mu);
}

static void ext_source_send(void* data, struct ext_data_control_source_v1* p, const char* mime, int32_t fd) {
    (void)p;
    pthread_mutex_lock(&g_mu);
    source_send((struct source*)data, mime, fd);
    pthread_mutex_unlock(&g_mu);
}
static void ext_source_cancelled(void* data, struct ext_data_control_source_v1* p) {
    (void)p; source_cancelled((struct source*)data);
}
static void wlr_source_send(void* data, struct zwlr_data_control_source_v1* p, const char* mime, int32_t fd) {
    (void)p;
    pthread_mutex_lock(&g_mu);
    source_send((struct source*)data, mime, fd);
    pthread_mutex_unlock(&g_mu);
}
static void wlr_source_cancelled(void* data, struct zwlr_data_control_source_v1* p) {
    (void)p; source_cancelled((struct source*)data);
}
static const struct ext_data_control_source_v1_listener  ext_source_listener = { ext_source_send, ext_source_cancelled };
static const struct zwlr_data_control_source_v1_listener wlr_source_listener = { wlr_source_send, wlr_source_cancelled };

// ---- device ------------------------------------------------------------

// A new offer object. We attach the listener so its MIME list fills
// in; the following selection / primary_selection event tells us what
// it is for.
static void device_data_offer(void* proxy, int is_ext) {
    struct offer* o = offer_new(proxy);
    if (!o) return;
    if (is_ext)
        ext_data_control_offer_v1_add_listener((struct ext_data_control_offer_v1*)proxy, &ext_offer_listener, o);
    else
        zwlr_data_control_offer_v1_add_listener((struct zwlr_data_control_offer_v1*)proxy, &wlr_offer_listener, o);
}

static struct offer* offer_from_proxy(void* proxy, int is_ext) {
    if (!proxy) return NULL;
    return (struct offer*)(is_ext
        ? ext_data_control_offer_v1_get_user_data((struct ext_data_control_offer_v1*)proxy)
        : zwlr_data_control_offer_v1_get_user_data((struct zwlr_data_control_offer_v1*)proxy));
}

static void device_selection(void* proxy, int is_ext) {
    pthread_mutex_lock(&g_mu);
    struct offer* o = offer_from_proxy(proxy, is_ext);
    if (g_selection && g_selection != o) offer_free(g_selection);
    g_selection = o;
    g_generation++;
    pthread_mutex_unlock(&g_mu);
    platform_post_wake();
}

// We don't track PRIMARY (SDL handles it with fresh mouse serials);
// the offer object still has to be destroyed or it leaks.
static void device_primary_selection(void* proxy, int is_ext) {
    pthread_mutex_lock(&g_mu);
    struct offer* o = offer_from_proxy(proxy, is_ext);
    if (o && o != g_selection) offer_free(o);
    pthread_mutex_unlock(&g_mu);
}

static void device_finished(void) {
    fprintf(stderr, "wlclip: compositor revoked the data-control device; clipboard falls back to SDL\n");
    pthread_mutex_lock(&g_mu);
    g_available = 0;
    pthread_mutex_unlock(&g_mu);
}

static void ext_dev_data_offer(void* d, struct ext_data_control_device_v1* p, struct ext_data_control_offer_v1* id) {
    (void)d; (void)p; device_data_offer(id, 1);
}
static void ext_dev_selection(void* d, struct ext_data_control_device_v1* p, struct ext_data_control_offer_v1* id) {
    (void)d; (void)p; device_selection(id, 1);
}
static void ext_dev_finished(void* d, struct ext_data_control_device_v1* p) {
    (void)d; (void)p; device_finished();
}
static void ext_dev_primary_selection(void* d, struct ext_data_control_device_v1* p, struct ext_data_control_offer_v1* id) {
    (void)d; (void)p; device_primary_selection(id, 1);
}
static const struct ext_data_control_device_v1_listener ext_dev_listener = {
    ext_dev_data_offer, ext_dev_selection, ext_dev_finished, ext_dev_primary_selection,
};

static void wlr_dev_data_offer(void* d, struct zwlr_data_control_device_v1* p, struct zwlr_data_control_offer_v1* id) {
    (void)d; (void)p; device_data_offer(id, 0);
}
static void wlr_dev_selection(void* d, struct zwlr_data_control_device_v1* p, struct zwlr_data_control_offer_v1* id) {
    (void)d; (void)p; device_selection(id, 0);
}
static void wlr_dev_finished(void* d, struct zwlr_data_control_device_v1* p) {
    (void)d; (void)p; device_finished();
}
static void wlr_dev_primary_selection(void* d, struct zwlr_data_control_device_v1* p, struct zwlr_data_control_offer_v1* id) {
    (void)d; (void)p; device_primary_selection(id, 0);
}
static const struct zwlr_data_control_device_v1_listener wlr_dev_listener = {
    wlr_dev_data_offer, wlr_dev_selection, wlr_dev_finished, wlr_dev_primary_selection,
};

// ---- registry ----------------------------------------------------------

static void registry_global(void* data, struct wl_registry* reg, uint32_t name,
                            const char* interface, uint32_t version) {
    (void)data;
    if (strcmp(interface, ext_data_control_manager_v1_interface.name) == 0 && !g_ext_mgr) {
        g_ext_mgr = (struct ext_data_control_manager_v1*)wl_registry_bind(
            reg, name, &ext_data_control_manager_v1_interface, 1);
    } else if (strcmp(interface, zwlr_data_control_manager_v1_interface.name) == 0 && !g_wlr_mgr) {
        uint32_t v = version < 2 ? version : 2;
        g_wlr_mgr = (struct zwlr_data_control_manager_v1*)wl_registry_bind(
            reg, name, &zwlr_data_control_manager_v1_interface, v);
    } else if (strcmp(interface, wl_seat_interface.name) == 0 && !g_seat) {
        // Version 1 is all a data-control device needs from the seat.
        g_seat = (struct wl_seat*)wl_registry_bind(reg, name, &wl_seat_interface, 1);
    }
}
static void registry_global_remove(void* data, struct wl_registry* reg, uint32_t name) {
    (void)data; (void)reg; (void)name;
}
static const struct wl_registry_listener registry_listener = { registry_global, registry_global_remove };

// ---- public API --------------------------------------------------------

int wlclip_init(void* display) {
    if (g_initialized) return g_available;
    g_initialized = 1;
    if (!display) return 0;
    g_display = (struct wl_display*)display;

    struct wl_registry* registry = wl_display_get_registry(g_display);
    wl_registry_add_listener(registry, &registry_listener, NULL);
    wl_display_roundtrip(g_display);
    wl_registry_destroy(registry);

    if (!g_seat) {
        fprintf(stderr, "wlclip: no wl_seat advertised; clipboard via SDL\n");
        return 0;
    }
    if (g_ext_mgr) {
        g_flavour = FLAVOUR_EXT;
        if (g_wlr_mgr) { zwlr_data_control_manager_v1_destroy(g_wlr_mgr); g_wlr_mgr = NULL; }
        g_ext_dev = ext_data_control_manager_v1_get_data_device(g_ext_mgr, g_seat);
        ext_data_control_device_v1_add_listener(g_ext_dev, &ext_dev_listener, NULL);
    } else if (g_wlr_mgr) {
        g_flavour = FLAVOUR_WLR;
        g_wlr_dev = zwlr_data_control_manager_v1_get_data_device(g_wlr_mgr, g_seat);
        zwlr_data_control_device_v1_add_listener(g_wlr_dev, &wlr_dev_listener, NULL);
    } else {
        fprintf(stderr, "wlclip: compositor has no data-control protocol; clipboard via SDL\n");
        return 0;
    }
    // The compositor answers get_data_device with the current
    // selection; pull it in now so the first paste sees it.
    wl_display_roundtrip(g_display);
    pthread_mutex_lock(&g_mu);
    g_available = 1;
    pthread_mutex_unlock(&g_mu);
    fprintf(stderr, "xerotty: clipboard via %s\n",
            g_flavour == FLAVOUR_EXT ? "ext-data-control-v1" : "wlr-data-control-unstable-v1");
    return 1;
}

int wlclip_available(void) {
    pthread_mutex_lock(&g_mu);
    int a = g_available;
    pthread_mutex_unlock(&g_mu);
    return a;
}

uint64_t wlclip_generation(void) {
    pthread_mutex_lock(&g_mu);
    uint64_t g = g_generation;
    pthread_mutex_unlock(&g_mu);
    return g;
}

int wlclip_mimes(char* buf, int cap) {
    if (buf && cap > 0) buf[0] = '\0';
    pthread_mutex_lock(&g_mu);
    if (!g_available) { pthread_mutex_unlock(&g_mu); return -1; }
    if (!g_selection) { pthread_mutex_unlock(&g_mu); return 0; }
    int used = 0;
    for (int i = 0; i < g_selection->n_mimes && buf && cap > 0; i++) {
        int w = snprintf(buf + used, (size_t)(cap - used), "%s%s", i ? "\n" : "", g_selection->mimes[i]);
        if (w < 0 || used + w >= cap) { buf[used] = '\0'; break; }
        used += w;
    }
    int n = g_selection->n_mimes;
    pthread_mutex_unlock(&g_mu);
    return n;
}

static int64_t now_ms(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

// read_all drains fd to EOF within timeout_ms IN TOTAL. The budget
// does not reset per chunk: a slow owner trickling bytes could
// otherwise hold the calling thread (often the render loop) for as
// long as it likes.
static unsigned char* read_all(int fd, size_t* len, int timeout_ms) {
    const size_t limit = (size_t)256 * 1024 * 1024;
    const int64_t deadline = now_ms() + timeout_ms;
    unsigned char* buf = NULL;
    size_t n = 0, cap = 0;
    for (;;) {
        int64_t left = deadline - now_ms();
        if (left <= 0) break;
        struct pollfd pfd = { fd, POLLIN, 0 };
        int r = poll(&pfd, 1, (int)left);
        if (r < 0 && errno == EINTR) continue;
        if (r <= 0) break; // timeout or error: give up
        if (n == cap) {
            size_t ncap = cap ? cap * 2 : 64 * 1024;
            if (ncap > limit) break;
            unsigned char* nb = (unsigned char*)realloc(buf, ncap);
            if (!nb) break;
            buf = nb; cap = ncap;
        }
        ssize_t got = read(fd, buf + n, cap - n);
        if (got < 0) { if (errno == EINTR || errno == EAGAIN) continue; break; }
        if (got == 0) { *len = n; return buf ? buf : (unsigned char*)calloc(1, 1); } // EOF: complete
        n += (size_t)got;
    }
    free(buf);
    *len = 0;
    return NULL;
}

unsigned char* wlclip_receive(const char* mime, size_t* len, int timeout_ms) {
    if (len) *len = 0;
    if (!mime || !len) return NULL;
    if (timeout_ms <= 0) timeout_ms = DEFAULT_TIMEOUT_MS;

    pthread_mutex_lock(&g_mu);
    if (!g_available) { pthread_mutex_unlock(&g_mu); return NULL; }

    // Our own source is current: answer from the store. Reading it
    // through a pipe would wait on a send event dispatched by the
    // main thread, which may be the caller.
    if (g_source && g_source->store) {
        const struct store_item* it = store_find(g_source->store, mime);
        unsigned char* out = NULL;
        if (it) {
            out = (unsigned char*)malloc(it->len ? it->len : 1);
            if (out) { memcpy(out, it->data, it->len); *len = it->len; }
        }
        pthread_mutex_unlock(&g_mu);
        return out;
    }

    if (!g_selection || !offer_has_mime(g_selection, mime)) { pthread_mutex_unlock(&g_mu); return NULL; }
    int fds[2];
    if (pipe2(fds, O_CLOEXEC) != 0) { pthread_mutex_unlock(&g_mu); return NULL; }
    offer_receive(g_selection, mime, fds[1]);
    wl_display_flush(g_display);
    // From here the transfer belongs to the compositor and the owner;
    // the offer object may be replaced while we drain.
    pthread_mutex_unlock(&g_mu);

    close(fds[1]);
    unsigned char* out = read_all(fds[0], len, timeout_ms);
    close(fds[0]);
    return out;
}

char* wlclip_receive_text(size_t* len, int timeout_ms) {
    if (len) *len = 0;
    char mimes[8192];
    if (wlclip_mimes(mimes, sizeof mimes) <= 0) return NULL;
    for (int i = 0; i < N_TEXT_MIMES; i++) {
        // offered?
        const char* m = TEXT_MIMES[i];
        size_t ml = strlen(m);
        int found = 0;
        for (const char* p = mimes; *p; ) {
            const char* e = strchr(p, '\n');
            size_t l = e ? (size_t)(e - p) : strlen(p);
            if (l == ml && memcmp(p, m, ml) == 0) { found = 1; break; }
            if (!e) break;
            p = e + 1;
        }
        if (!found) continue;
        size_t n = 0;
        unsigned char* b = wlclip_receive(m, &n, timeout_ms);
        if (!b) return NULL;
        char* s = (char*)realloc(b, n + 1);
        if (!s) { free(b); return NULL; }
        s[n] = '\0';
        if (len) *len = n;
        return s;
    }
    return NULL;
}

int wlclip_set(const char* const* mimes, const unsigned char* const* datas,
               const size_t* lens, int n) {
    if (n <= 0) return 0;

    struct store* st = (struct store*)calloc(1, sizeof *st);
    if (!st) return 0;
    st->items = (struct store_item*)calloc((size_t)n, sizeof *st->items);
    if (!st->items) { free(st); return 0; }
    pthread_mutex_init(&st->mu, NULL);
    st->refs = 1;
    for (int i = 0; i < n; i++) {
        st->items[i].mime = strdup(mimes[i]);
        st->items[i].data = (unsigned char*)malloc(lens[i] ? lens[i] : 1);
        if (!st->items[i].mime || !st->items[i].data) { st->n = i + 1; store_release(st); return 0; }
        memcpy(st->items[i].data, datas[i], lens[i]);
        st->items[i].len = lens[i];
        st->n = i + 1;
    }

    struct source* src = (struct source*)calloc(1, sizeof *src);
    if (!src) { store_release(st); return 0; }
    src->store = st;

    pthread_mutex_lock(&g_mu);
    if (!g_available) { pthread_mutex_unlock(&g_mu); source_free(src); return 0; }
    if (g_flavour == FLAVOUR_EXT) {
        struct ext_data_control_source_v1* s = ext_data_control_manager_v1_create_data_source(g_ext_mgr);
        src->proxy = s;
        ext_data_control_source_v1_add_listener(s, &ext_source_listener, src);
        for (int i = 0; i < n; i++) ext_data_control_source_v1_offer(s, mimes[i]);
    } else {
        struct zwlr_data_control_source_v1* s = zwlr_data_control_manager_v1_create_data_source(g_wlr_mgr);
        src->proxy = s;
        zwlr_data_control_source_v1_add_listener(s, &wlr_source_listener, src);
        for (int i = 0; i < n; i++) zwlr_data_control_source_v1_offer(s, mimes[i]);
    }

    // Replacing our own previous source: the compositor will send it
    // cancelled once the new one is current, which frees it. Point at
    // the new one now so a read in between doesn't serve stale bytes.
    g_source = src;

    if (g_flavour == FLAVOUR_EXT)
        ext_data_control_device_v1_set_selection(g_ext_dev, (struct ext_data_control_source_v1*)src->proxy);
    else
        zwlr_data_control_device_v1_set_selection(g_wlr_dev, (struct zwlr_data_control_source_v1*)src->proxy);
    wl_display_flush(g_display);
    pthread_mutex_unlock(&g_mu);
    return 1;
}

int wlclip_set_text(const char* text) {
    if (!text) return 0;
    const unsigned char* datas[sizeof TEXT_MIMES / sizeof TEXT_MIMES[0]];
    size_t lens[sizeof TEXT_MIMES / sizeof TEXT_MIMES[0]];
    size_t l = strlen(text);
    for (int i = 0; i < N_TEXT_MIMES; i++) { datas[i] = (const unsigned char*)text; lens[i] = l; }
    return wlclip_set(TEXT_MIMES, datas, lens, N_TEXT_MIMES);
}

void wlclip_shutdown(void) {
    if (!g_initialized) return;
    pthread_mutex_lock(&g_mu);
    if (g_source) { source_free(g_source); g_source = NULL; }
    if (g_selection) { offer_free(g_selection); g_selection = NULL; }
    if (g_ext_dev) { ext_data_control_device_v1_destroy(g_ext_dev); g_ext_dev = NULL; }
    if (g_wlr_dev) { zwlr_data_control_device_v1_destroy(g_wlr_dev); g_wlr_dev = NULL; }
    if (g_ext_mgr) { ext_data_control_manager_v1_destroy(g_ext_mgr); g_ext_mgr = NULL; }
    if (g_wlr_mgr) { zwlr_data_control_manager_v1_destroy(g_wlr_mgr); g_wlr_mgr = NULL; }
    if (g_seat) { wl_seat_destroy(g_seat); g_seat = NULL; }
    g_available = 0;
    g_flavour = FLAVOUR_NONE;
    pthread_mutex_unlock(&g_mu);
}
