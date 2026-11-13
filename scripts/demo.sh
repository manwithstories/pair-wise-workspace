#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/gkpack-demo.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

# Keep every build step local and use the vendored dependency tree.
export GOPROXY=off
export GOSUMDB=off
export GOFLAGS=-mod=vendor
export GOPATH="${GOPATH:-$WORK/gopath}"
export GOMODCACHE="${GOMODCACHE:-$WORK/gomodcache}"
export GOCACHE="${GOCACHE:-$WORK/gocache}"

go build -o "$WORK/gkpack" ./cmd/gkpack
go run ./scripts/genfixture.go --out "$WORK/src-v1" --version v1
go run ./scripts/genfixture.go --out "$WORK/src-v2" --version v2

"$WORK/gkpack" build --src "$WORK/src-v1" --archive "$WORK/v1.gkpack" --version v1
"$WORK/gkpack" build --src "$WORK/src-v2" --archive "$WORK/v2.gkpack" --version v2
"$WORK/gkpack" delta --base "$WORK/v1.gkpack" --target "$WORK/v2.gkpack" --patch "$WORK/v1-v2.gkpatch"
"$WORK/gkpack" apply --base "$WORK/v1.gkpack" --patch "$WORK/v1-v2.gkpatch" --out "$WORK/upgraded"
"$WORK/gkpack" apply --base "$WORK/v2.gkpack" --patch "$WORK/v1-v2.gkpatch" --out "$WORK/rollback" --reverse

cp -R "$WORK/upgraded" "$WORK/corrupt"
for FILE in assets/tex/hero_02.ktx assets/audio/bgm_03.ogg; do
  printf '\0' | dd of="$WORK/corrupt/$FILE" bs=1 seek=1 count=1 conv=notrunc 2>/dev/null
done

set +e
VERIFY_OUT=$("$WORK/gkpack" verify --archive "$WORK/v2.gkpack" --tree "$WORK/corrupt" 2>"$WORK/verify.stderr")
VERIFY_STATUS=$?
set -e
if [[ $VERIFY_STATUS -eq 0 ]]; then
  echo "verify unexpectedly succeeded after corruption" >&2
  exit 1
fi
printf '注入位翻转 %s\n' "$VERIFY_OUT"
