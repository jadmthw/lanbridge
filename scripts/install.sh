#!/bin/sh
# Installs LANBridge on macOS or Linux.
#   curl -fsSL https://github.com/__REPO__/releases/latest/download/install.sh | sh
# Downloading with curl (instead of a browser) avoids macOS's quarantine prompt.
set -eu
REPO="${LANBRIDGE_REPO:-__REPO__}"
case "$(uname -s)" in
  Darwin) os=macos ;;
  Linux) os=linux ;;
  *) echo "On Windows, download LANBridge-windows-*.exe from https://github.com/$REPO/releases/latest" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=x64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "Unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac
dir="${LANBRIDGE_DIR:-$HOME/.local/bin}"
mkdir -p "$dir"
curl -fsSL "https://github.com/$REPO/releases/latest/download/lanbridge-$os-$arch.tar.gz" | tar -xz -C "$dir" lanbridge
chmod +x "$dir/lanbridge"
echo "Installed LANBridge to $dir/lanbridge"
case ":$PATH:" in
  *":$dir:"*) echo "Start it with: lanbridge" ;;
  *) echo "Start it with: $dir/lanbridge   (or add $dir to your PATH)" ;;
esac
