#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

case "$(uname -s)" in
Darwin)
    # macOS: assemble the .app bundle so the user gets proper Dock-icon
    # coalescing, Cmd+H, the app icon, etc. `make app` depends on `make
    # build` (so the binary still ends up at ./xerotty) and also copies
    # icon/xerotty.icns into Contents/Resources/. Generate the .icns
    # first if it's missing (e.g. after a fresh clone).
    if [[ ! -f icon/xerotty.icns && -f icon/xerotty.svg ]]; then
        make icns
    fi
    make app
    ;;
*)
    # Linux / other: bare binary. The Linux build embeds
    # icon/xerotty-256.png via icon/embed.go and applies it to each
    # SDL_Window at startup (see internal/app/icon_linux.go) — WM
    # taskbars and Alt-Tab pick that up. A proper .desktop file for
    # app-launcher menu integration (rofi / GNOME / KDE) is still
    # TODO.
    go build -o xerotty ./cmd/xerotty
    echo "built: $(pwd)/xerotty"
    ;;
esac
