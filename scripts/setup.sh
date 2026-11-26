#!/usr/bin/env bash
# Local environment setup for mediamix on macOS (Apple Silicon and Intel).
# Installs the native FFmpeg 9 toolchain and pkg-config via Homebrew, then
# points CGO at FFmpeg's pkg-config files.
set -euo pipefail

if ! command -v brew >/dev/null 2>&1; then
  echo "Homebrew is required: https://brew.sh" >&2
  exit 2
fi

brew install ffmpeg pkg-config

export PKG_CONFIG_PATH="$(brew --prefix ffmpeg)/lib/pkgconfig"
echo "PKG_CONFIG_PATH=${PKG_CONFIG_PATH}"

go mod tidy
go build ./...
echo "setup complete"
