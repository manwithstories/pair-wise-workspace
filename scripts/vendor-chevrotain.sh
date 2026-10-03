#!/usr/bin/env bash
# Re-vendor chevrotain source and regenerate its declaration surface.
#
# chevrotain@11.1.0 ships its TypeScript source under src/. We copy it into
# vendor/chevrotain/src so the query parser is built on a version-locked,
# checked-in toolkit, then:
#   1. prepend `// @ts-nocheck` to each vendored file, because upstream does not
#      compile under this project's strict flags. Runtime behaviour is unchanged;
#      only the compiler contract is relaxed for this subtree.
#   2. emit vendor/chevrotain/dts with relaxed settings so the strict project in
#      src/ can consume a clean typed surface.
set -euo pipefail
cd "$(dirname "$0")/.."
VER=11.1.0

rm -rf vendor/chevrotain/src vendor/chevrotain/dts
mkdir -p vendor/chevrotain
cp -R "node_modules/chevrotain/src" vendor/chevrotain/src
cp node_modules/chevrotain/LICENSE.txt vendor/chevrotain/LICENSE.txt

find vendor/chevrotain/src -name '*.ts' -print0 | while IFS= read -r -d '' f; do
  if ! head -1 "$f" | grep -q '@ts-nocheck'; then
    printf '// @ts-nocheck (vendored chevrotain %s)\n%s' "$VER" "$(cat "$f")" > "$f"
  fi
done

npx tsc -p tsconfig.vendor-dts.json
echo "vendored chevrotain $VER -> vendor/chevrotain/src (+ dts)"
