#!/usr/bin/env bash
# Reproduce the two acceptance demo scenarios from the project directory.
# Requires Go 1.25.6 on PATH. Purely local; no network service is used.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> go mod download"
go mod download

echo "==> build -> /tmp/avpipe"
go build -o /tmp/avpipe ./cmd/avpipe

echo
echo "==> damaged run (loss + burst + truncation + corruption)"
/tmp/avpipe demo --frames 600 --seed 7 --loss 0.05 --burst 2 \
	--truncate 3 --corrupt-every 97
damaged_rc=$?
echo "exit=$damaged_rc"

echo
echo "==> clean run"
/tmp/avpipe demo --frames 600 --seed 7 --loss 0 --case clean
clean_rc=$?
echo "exit=$clean_rc"

if [[ $damaged_rc -ne 0 || $clean_rc -ne 0 ]]; then
	echo "DEMO FAILED" >&2
	exit 1
fi
echo "DEMO OK"
