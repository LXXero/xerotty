#!/bin/sh
# GL-vs-GPU pixel parity regression check.
#
# The two backends must render functionally identical pixels — when
# this was first measured (GPU slice 2 bring-up) the diff exposed
# that EVERY secondary window had been clearing to hardcoded black
# instead of the theme background since multi-viewport landed, in
# both backends. Healthy baseline: ~100 of 5.6M pixels differ (quad-
# edge FP tie-breaks), mean diff ~0.004/255.
#
# Usage: tools/render-ab-check.sh    (needs a display + imagemagick)
set -e
command -v magick >/dev/null || { echo "SKIP: imagemagick not installed"; exit 0; }
BIN="${XEROTTY_BIN:-./xerotty}"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/xerotty"
cat > "$TMP/fill.sh" <<'SH'
#!/bin/sh
printf '\033[1mBOLD\033[0m plain \033[4munderline\033[0m \033[9mstrike\033[0m \033[7mreverse\033[0m\n'
printf 'emoji: 1F30B 1F680  CJK: \345\256\275\345\255\227\347\254\246  box: \342\224\214\342\224\200\342\224\254\342\224\200\342\224\220 \342\226\200\342\226\204\342\226\210\342\226\221\342\226\222\342\226\223\n'
printf '\033[38;5;196mred\033[0m \033[38;5;46mgreen\033[0m \033[38;5;21mblue\033[0m \033[48;5;226;30myellow-bg\033[0m\n'
i=0; while [ $i -lt 30 ]; do printf 'line%03d the quick brown fox jumps over the lazy dog 0123456789 ' $i; i=$((i+1)); done
echo
exec sleep 100000
SH
chmod +x "$TMP/fill.sh"
cat > "$TMP/xerotty/config.toml" <<TOML
shell = "$TMP/fill.sh"
[appearance.glow]
enabled = false
TOML

XDG_CONFIG_HOME="$TMP" XEROTTY_GPU=0 "$BIN" --separate \
    --screenshot "$TMP/gl.png" --screenshot-frames 90 >/dev/null 2>&1
XDG_CONFIG_HOME="$TMP" XEROTTY_GPU=1 "$BIN" --separate \
    --screenshot "$TMP/gpu.png" --screenshot-frames 90 >/dev/null 2>&1
[ -f "$TMP/gl.png" ] && [ -f "$TMP/gpu.png" ] || { echo "FAIL: screenshot capture broke on a backend"; exit 1; }

AE=$(magick compare -metric AE "$TMP/gl.png" "$TMP/gpu.png" null: 2>&1 | awk '{print int($1)}')
MEAN=$(magick "$TMP/gl.png" "$TMP/gpu.png" -compose difference -composite -format "%[fx:mean*255]" info:)
echo "differing-pixels=$AE (budget 5000)  mean-diff=$MEAN/255 (budget 0.1)"
OK=$(echo "$MEAN" | awk '{print ($1 < 0.1) ? 1 : 0}')
if [ "$AE" -gt 5000 ] || [ "$OK" != 1 ]; then
    echo "FAIL: backends diverged — check clears, premul blit, compositor"
    exit 1
fi
echo "OK: GL and GPU render pixel-equivalent output"
