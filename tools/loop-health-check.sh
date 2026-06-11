#!/bin/sh
# Render-loop wakeup regression check.
#
# The wakeup-storm hunt (2026-06) found the loop rendering 3 frames
# per glow tick, slicing sub-2ms SDL_WaitEventTimeout arms at the
# 4x-refresh cap, and relaying every tick through a Go timer ->
# PostWake -> SDL event chain. The XEROTTY_DEBUG_LOOP counters were
# built to see that; this script pins the healthy steady state:
# with an idle shell and the lamp at 20fps, one second of loop must
# be ~20 idle-timeout wakes, ~20 frames, ZERO paced waits, ZERO
# sub-2ms arms, ~ZERO events.
#
# Usage: tools/loop-health-check.sh [gl|gpu]   (default: gl)
# Needs a display (Wayland/X11). Exits nonzero on budget violation.
set -e
BACKEND="${1:-gl}"
BIN="${XEROTTY_BIN:-./xerotty}"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/xerotty"
cat > "$TMP/idle.sh" <<'SH'
#!/bin/sh
exec sleep 100000
SH
chmod +x "$TMP/idle.sh"
cat > "$TMP/xerotty/config.toml" <<TOML
shell = "$TMP/idle.sh"
[appearance.glow]
enabled = true
fps = 20
TOML

GPU=0
[ "$BACKEND" = "gpu" ] && GPU=1
OUT=$(XDG_CONFIG_HOME="$TMP" XEROTTY_GPU=$GPU XEROTTY_DEBUG_LOOP=1 \
      timeout 7 "$BIN" --separate 2>&1 | grep '^\[loop\]' || true)
LINES=$(echo "$OUT" | wc -l)
if [ "$LINES" -lt 4 ]; then
    echo "FAIL($BACKEND): expected >=4 loop-health lines, got $LINES"
    echo "$OUT"
    exit 1
fi

# Skip the first two seconds (startup churn), judge the rest.
echo "$OUT" | tail -n +3 | awk -v backend="$BACKEND" '
{
    # [loop] frames=N waits=N (sub2ms=N) idle_waits=N min_idle_ms=N events=N
    for (i = 1; i <= NF; i++) {
        split($i, kv, "=")
        if (kv[1] == "frames")          frames = kv[2]
        if (kv[1] == "waits")           waits = kv[2]
        if (kv[1] == "(sub2ms")         sub2 = kv[2] + 0
        if (kv[1] == "idle_waits")      idle = kv[2]
        if (kv[1] == "events")          events = kv[2]
    }
    n++
    if (sub2 > 1)              { bad = bad sprintf("  line %d: sub2ms=%d (budget 1) — sub-2ms timer arms are the powermetrics storm signature\n", n, sub2) }
    if (waits > 2)             { bad = bad sprintf("  line %d: waits=%d (budget 2) — paced waits mean settle credits leak to non-input wakes\n", n, waits) }
    if (events > 2)            { bad = bad sprintf("  line %d: events=%d (budget 2) — glow must pace via idle timeout, not PostWake relays\n", n, events) }
    if (frames < 12 || frames > 40) { bad = bad sprintf("  line %d: frames=%d (want ~20±) — lamp pacing broken\n", n, frames) }
    if (idle < 12 || idle > 40)     { bad = bad sprintf("  line %d: idle_waits=%d (want ~20±) — idle timeout not driving the lamp\n", n, idle) }
    print "  [" backend "] " $0
}
END {
    if (bad != "") { printf "FAIL(%s):\n%s", backend, bad; exit 1 }
    print "OK(" backend "): loop at wakeup floor"
}'
