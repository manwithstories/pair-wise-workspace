#!/bin/sh
# Generates the sample archive used by the acceptance run.
#
# Usage: scripts/make_fixtures.sh [OUT_DIR] [--include-damaged]
#
# The generator lives in the media-restore binary so fixtures and production
# code share one encoder path; this script only locates the binary and shells
# out to it.  Set ARCHIVE_FIXTURES to override the default output directory.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
binary="$root/build/media-restore"

if [ ! -x "$binary" ]; then
  echo "error: $binary not found; build first with: cmake -S . -B build && cmake --build build" >&2
  exit 1
fi

out=${ARCHIVE_FIXTURES:-/tmp/archive-fixtures}
include_damaged=0
for arg in "$@"; do
  case "$arg" in
    --include-damaged) include_damaged=1 ;;
    -*) echo "error: unknown option: $arg" >&2; exit 1 ;;
    *) out=$arg ;;
  esac
done

if [ "$include_damaged" -eq 1 ]; then
  exec "$binary" generate --out "$out" --include-damaged
fi
exec "$binary" generate --out "$out"
