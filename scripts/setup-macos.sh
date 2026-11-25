#!/usr/bin/env bash
# Install the required local toolchain and dependencies on macOS Apple Silicon.
set -euo pipefail

GO_PKG="/tmp/go1.25.6.pkg"
curl -fL https://go.dev/dl/go1.25.6.darwin-arm64.pkg -o "$GO_PKG"
sudo installer -pkg "$GO_PKG" -target /
rm -f "$GO_PKG"

export PATH="/usr/local/go/bin:$PATH"
if [[ -x /opt/homebrew/bin/brew ]]; then
  eval "$(/opt/homebrew/bin/brew shellenv)"
fi

brew install ffmpeg
go mod tidy
go mod edit -toolchain=go1.25.6
go version
