#!/usr/bin/env bash
# build.sh - cross-compile ldi with the version stamped into the binary.
#
# The version comes from git: the nearest tag, plus a suffix when HEAD has moved
# past it or the tree is dirty. Override it with $VERSION for a release build.
#
#   ./build.sh                 # v1.6.0, or v1.6.0-3-ge300fdc-dirty
#   VERSION=1.7.0 ./build.sh   # exactly that
#
# The -X path must be "main.Version". The fully qualified form
# "latest-docker-image/cmd.Version" is accepted by the linker and then silently
# ignored, leaving the default from the source in the binary.
#
# Kept POSIX so it runs under sh as well as bash: there are no pipelines here,
# so pipefail would buy nothing and dash rejects it outright.
set -eu

cd "$(dirname "$0")"

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
LDFLAGS="-w -s -X main.Version=${VERSION}"

mkdir -p ./bin

build() {
    local goos=$1 goarch=$2 out=$3
    printf '  %-14s -> %s\n' "${goos}/${goarch}" "$out"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOAMD64=v3 \
        go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd
}

echo "building ldi ${VERSION}"

build linux   amd64 ./bin/ldi-linux
build windows amd64 ./bin/ldi.exe
build darwin  amd64 ./bin/ldi-macos
build darwin  arm64 ./bin/ldi-macos-arm64
# build linux   arm64 ./bin/ldi-linux-arm64
