#!/bin/sh
# Cross-compile camback for every supported OS into dist/ (or $1).
#   ./scripts/build.sh
#   ./scripts/build.sh ~/Dropbox/tools/camback
set -eu

cd "$(dirname "$0")/.."
out="${1:-dist}"
version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
mkdir -p "$out"

for target in darwin/arm64 darwin/amd64 windows/amd64 windows/arm64 linux/amd64 linux/arm64; do
    os="${target%/*}"
    arch="${target#*/}"
    name="camback-$os-$arch"
    [ "$os" = windows ] && name="$name.exe"
    echo "building $out/$name"
    # CGO_ENABLED=0: pure Go, no C toolchain needed, one self-contained binary.
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X github.com/TatsuyaOGth/camback/internal/cli.Version=$version" \
        -o "$out/$name" ./cmd/camback
done
