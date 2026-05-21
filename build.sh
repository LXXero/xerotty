#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

# Always run `go generate` first so tinylib/msgp regenerates the
# wire-format encoders in internal/protocol. Direct `go build` skips
# this and produces a binary with stale encoders → cryptic decode
# errors on the wire. Always build via build.sh or `make`.
#
# Ensure $(go env GOPATH)/bin is on PATH so `go generate` can find
# the msgp binary (`go install github.com/tinylib/msgp` puts it
# there). Idempotent if already on PATH.
GOBIN="$(go env GOPATH)/bin"
case ":$PATH:" in
    *":$GOBIN:"*) ;;
    *) export PATH="$GOBIN:$PATH" ;;
esac

# Install msgp if missing — first-clone friendly. Pinned to the
# version go.mod tracks so the generator matches the runtime lib.
if ! command -v msgp >/dev/null 2>&1; then
    echo "build.sh: installing tinylib/msgp..."
    go install github.com/tinylib/msgp
fi

go generate ./...

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
    # taskbars and Alt-Tab pick that up.
    go build -o xerotty ./cmd/xerotty
    echo "built: $(pwd)/xerotty"
    go build -o xerottyd ./cmd/xerottyd
    echo "built: $(pwd)/xerottyd"
    go build -o xerotty-viewer ./cmd/xerotty-viewer
    echo "built: $(pwd)/xerotty-viewer"
    ;;
esac
