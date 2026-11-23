#!/usr/bin/env bash
#
# run-demo.sh — build mediascan and run it with its real exit code.
#
# Why this wrapper exists: `go run` normalizes any non-zero child exit code
# to 1 (it only prints "exit status N"), so the documented semantic codes
# (2/3/4/5) cannot be observed through `go run`. This script builds the
# binary once and execs it, propagating the exact exit code. All arguments
# are forwarded unchanged to the mediascan demo subcommand.
#
# Usage:
#   scripts/run-demo.sh demo --seed 42 --tracks 4 --frames 1200 \
#       --corrupt-at 731 --checkpoint ./tmp/checkpoint.json --cancel-after 500
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin_dir="${TMPDIR:-/tmp}/mediascan-bin"
mkdir -p "${bin_dir}"
bin="${bin_dir}/mediascan"

( cd "${repo_root}" && go build -o "${bin}" ./cmd/mediascan )

# Forward all arguments and replace the shell with the binary so its exact
# exit status becomes the script's exit status.
exec "${bin}" "$@"
