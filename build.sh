#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

# Always run `go generate` first. Currently a no-op (no //go:generate
# directives in the tree yet) but the daemon protocol work will add
# tinylib/msgp codegen for the wire format — having it wired into the
# canonical build script means schema-drift between Go structs and
# generated encoders can never happen. Direct `go build` skips this;
# always build via build.sh or `make` (which also runs it).
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
    ;;
esac
