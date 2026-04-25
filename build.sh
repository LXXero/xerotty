#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

go build -o xerotty ./cmd/xerotty
echo "built: $(pwd)/xerotty"
