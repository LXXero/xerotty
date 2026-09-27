#!/bin/sh
# Zero-display survival check (Wayland, wlroots compositors).
#
# A VT switch makes the compositor drop DRM master, and every
# wl_output global goes away until you switch back. Dear ImGui
# asserts that the platform monitor list is never empty, the stock
# SDL3 backend rebuilt that list from scratch on every display event,
# and cimgui-go turns the assert into a Go panic — so the GUI died on
# every Ctrl+Alt+Fn, leaving only "client disconnected" in the daemon
# log. This pins the fix (XEROTTY PATCH in ImGui_ImplSDL3_UpdateMonitors)
# by disabling every output, which removes the wl_output globals the
# same way without needing root or a second VT.
#
# Usage: tools/no-display-check.sh     (needs wlr-randr + jq)
# BLANKS EVERY SCREEN for ~3 seconds. Outputs come back with a plain
# --on; if kanshi was running it is restarted with its original
# command line and re-applies the real layout.
set -e
[ -n "$WAYLAND_DISPLAY" ] || { echo "SKIP: needs a Wayland session"; exit 0; }
command -v wlr-randr >/dev/null || { echo "SKIP: wlr-randr not installed"; exit 0; }
command -v jq >/dev/null || { echo "SKIP: jq not installed"; exit 0; }
BIN="${XEROTTY_BIN:-./xerotty}"
OUTPUTS=$(wlr-randr --json | jq -r '.[] | select(.enabled) | .name')
[ -n "$OUTPUTS" ] || { echo "SKIP: no enabled outputs"; exit 0; }

# kanshi re-applies its profile the moment an output changes, which
# would undo the disable mid-test.
KANSHI_CMD=""
KPID=$(pgrep -x kanshi | head -n 1 || true)
[ -n "$KPID" ] && KANSHI_CMD=$(tr '\0' ' ' < "/proc/$KPID/cmdline")

TMP=$(mktemp -d)
PID=""
restore() {
    for o in $OUTPUTS; do wlr-randr --output "$o" --on 2>/dev/null || true; done
    if [ -n "$KANSHI_CMD" ] && ! pgrep -x kanshi >/dev/null; then
        # shellcheck disable=SC2086
        setsid -f $KANSHI_CMD >/dev/null 2>&1 || true
    fi
    [ -n "$PID" ] && kill "$PID" 2>/dev/null || true
    rm -rf "$TMP"
}
trap restore EXIT INT TERM

# Hermetic config + cache (see loop-health-check.sh for why not XDG).
export XEROTTY_CONFIG_DIR="$TMP/cfg" XEROTTY_CACHE_DIR="$TMP/cache"
mkdir -p "$XEROTTY_CONFIG_DIR"
cat > "$TMP/idle.sh" <<'IDLE'
#!/bin/sh
exec sleep 100000
IDLE
chmod +x "$TMP/idle.sh"
printf 'shell = "%s"\n' "$TMP/idle.sh" > "$XEROTTY_CONFIG_DIR/config.toml"

"$BIN" --separate >/dev/null 2>"$TMP/gui.err" &
PID=$!
sleep 3
kill -0 "$PID" 2>/dev/null || { echo "FAIL: GUI did not start"; cat "$TMP/gui.err"; exit 1; }

[ -n "$KPID" ] && pkill -x kanshi || true
for o in $OUTPUTS; do wlr-randr --output "$o" --off; done
sleep 3
for o in $OUTPUTS; do wlr-randr --output "$o" --on; done
sleep 3

if ! kill -0 "$PID" 2>/dev/null; then
    echo "FAIL: GUI died while no display was present"
    grep -A 4 '^panic' "$TMP/gui.err" || tail -n 20 "$TMP/gui.err"
    exit 1
fi
echo "OK: GUI survived zero displays and the output's return"
