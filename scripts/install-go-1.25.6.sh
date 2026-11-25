#!/usr/bin/env bash
# Install the exact Go release used by this project on macOS.
set -euo pipefail

arch=$(uname -m)
case "$arch" in
  arm64) goarch=arm64 ;;
  x86_64) goarch=amd64 ;;
  *)
    echo "unsupported architecture: $arch" >&2
    exit 2
    ;;
esac

pkg="$TMPDIR/go1.25.6-$goarch.pkg"
curl -fL "https://go.dev/dl/go1.25.6.darwin-$goarch.pkg" -o "$pkg"
sudo installer -pkg "$pkg" -target /
rm "$pkg"
echo "Installed Go 1.25.6 for $goarch"
