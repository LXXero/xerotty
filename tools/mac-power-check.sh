#!/bin/sh
# macOS thread-pathology regression check (run ON a mac).
#
# Two killers found via `sample` during the 2026-06 wakeup hunt,
# both invisible to in-process counters because they live in AppKit
# / driver threads:
#
#  1. Stuck NSAnimation threads: macOS 26 runs implicit window
#     animations as BLOCKING NSAnimations on dispatch workers; with
#     Metal-layer windows they never complete — one permanently
#     parked thread PER WINDOW, each sipping sub-millisecond
#     run-loop timers forever. Fixed with
#     NSWindowAnimationBehaviorNone; budget here is ZERO.
#  2. nextDrawable pool starvation: PRESENTMODE_VSYNC held each
#     window's drawables to the display composite, so the serial
#     viewport loop queued the main thread on CAMetalLayer dispatch
#     semaphores (18% of main thread on a 15-window session). Fixed
#     with MAILBOX/IMMEDIATE presents; small budget for the stray
#     acquire during bursts.
#
# Usage: tools/mac-power-check.sh [num_windows]   (default 8)
set -e
[ "$(uname -s)" = "Darwin" ] || { echo "SKIP: darwin only"; exit 0; }
N="${1:-8}"
BIN="${XEROTTY_BIN:-./xerotty}"
TMP=$(mktemp -d)
trap 'pkill -f "xerotty --separate" 2>/dev/null; rm -rf "$TMP"' EXIT
# XEROTTY_CONFIG_DIR, not XDG_CONFIG_HOME: os.UserConfigDir ignores
# XDG on macOS, so the glow config below used to be silently skipped
# in favor of the real one.
export XDG_RUNTIME_DIR="$TMP/rt" XEROTTY_CONFIG_DIR="$TMP/cfg" XEROTTY_CACHE_DIR="$TMP/cache"
mkdir -p -m 700 "$XDG_RUNTIME_DIR" "$XEROTTY_CONFIG_DIR"
printf '[appearance.glow]\nenabled = true\nfps = 20\n' > "$XEROTTY_CONFIG_DIR/config.toml"

"$BIN" --separate >/dev/null 2>&1 &
sleep 2
i=1; while [ "$i" -lt "$N" ]; do "$BIN" >/dev/null 2>&1; sleep 0.3; i=$((i+1)); done
sleep 2
P=$(pgrep -f "xerotty --separate" | head -1)
[ -n "$P" ] || { echo "FAIL: GUI process died"; exit 1; }
sample "$P" 5 -file "$TMP/sample.txt" >/dev/null 2>&1

ANIM=$(grep -c "_runBlocking" "$TMP/sample.txt" || true)
DRAW=$(grep -c "nextDrawable" "$TMP/sample.txt" || true)
echo "windows=$N  NSAnimation_runBlocking=$ANIM (budget 0)  nextDrawable=$DRAW (budget 10)"
FAIL=0
[ "$ANIM" -gt 0 ]  && { echo "FAIL: stuck NSAnimation threads are back — check NSWindowAnimationBehaviorNone coverage"; FAIL=1; }
[ "$DRAW" -gt 10 ] && { echo "FAIL: main thread queuing on nextDrawable again — check viewport present mode (MAILBOX/IMMEDIATE)"; FAIL=1; }
[ "$FAIL" = 0 ] && echo "OK: no stuck animations, no drawable starvation"
exit "$FAIL"
