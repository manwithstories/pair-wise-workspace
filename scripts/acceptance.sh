#!/usr/bin/env bash
# Run the full local acceptance flow: tests, simulated interrupt, resume, verify.
set -euo pipefail

ROOT="${1:-/tmp/bcast-run}"
export PATH="/usr/local/go/bin:$PATH"
if [[ -x /opt/homebrew/bin/brew ]]; then
  eval "$(/opt/homebrew/bin/brew shellenv)"
fi

go version
go build -o /tmp/bcast ./cmd/bcast
go test ./...

if [[ -e "$ROOT" ]]; then
  case "$ROOT" in
    /tmp/*) rm -rf "$ROOT" ;;
    *)
      echo "refusing to remove non-/tmp path: $ROOT" >&2
      exit 2
      ;;
  esac
fi

set +e
/tmp/bcast demo --root "$ROOT" --seed 41 --selfcheck --interrupt-after 2
demo_code=$?
set -e
if [[ "$demo_code" -ne 75 ]]; then
  echo "demo exit=$demo_code, want 75" >&2
  exit 1
fi

/tmp/bcast resume --root "$ROOT"
/tmp/bcast verify --root "$ROOT" --full-scan
