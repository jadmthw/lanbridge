#!/usr/bin/env bash
# Cross-compiles LANBridge for every supported platform into ./dist.
# Usage: scripts/build.sh [version] [outdir]    (DEFAULT_RELAY=... bakes in a relay)
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${1:-$(cat VERSION)}"
OUT="${2:-dist}"
LDFLAGS="-s -w -X main.version=${VERSION} -X main.defaultRelay=${DEFAULT_RELAY:-}"
rm -rf "$OUT"
mkdir -p "$OUT"

build() { # goos goarch output
  echo "building $1/$2"
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" go build -trimpath -ldflags "$LDFLAGS" -o "$3" ./cmd/lanbridge
}

build windows amd64 "$OUT/LANBridge-windows-x64.exe"
build windows arm64 "$OUT/LANBridge-windows-arm64.exe"
for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do
  goos="${target%/*}"; goarch="${target#*/}"
  label="$goos"; [ "$goos" = darwin ] && label=macos
  arch="$goarch"; [ "$goarch" = amd64 ] && arch=x64
  tmp="$(mktemp -d)"
  build "$goos" "$goarch" "$tmp/lanbridge"
  tar -C "$tmp" -czf "$OUT/lanbridge-$label-$arch.tar.gz" lanbridge
  rm -rf "$tmp"
done
ls -l "$OUT"
