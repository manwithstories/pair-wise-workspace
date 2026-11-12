#!/usr/bin/env bash
# Remove generated run artifacts and build products. Keeps source, configs, tests.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
echo "Removing out/ and target/"
rm -rf out target
echo "Done."
