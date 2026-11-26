#!/usr/bin/env bash
# Reproduces the acceptance demo end to end on a fully local, dependency-free
# fixture. Exit codes: 0 metrics+recovery pass, 2 input error, 3 rollback or
# recovery failure, 4 metrics over threshold.
set -euo pipefail

cd "$(dirname "$0")/.."
export PKG_CONFIG_PATH="$(brew --prefix ffmpeg)/lib/pkgconfig"

go build -o bin/mediamix ./cmd/mediamix
exec ./bin/mediamix demo \
  --case all \
  --seed 41 \
  --seconds 12 \
  --loss 0.02 \
  --reorder 0.05 \
  --queue-mib 8 \
  --slow-consumer-ms 2 \
  --fault after-normalize
