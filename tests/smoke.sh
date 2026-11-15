#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BIN="$ROOT/build/subtitle-audit"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/subtitle-audit.XXXXXX")
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

cd "$ROOT"
"$BIN" gen-fixtures --out "$TMP/fixtures" --count 12 --seed 20261116 >/dev/null
"$BIN" style-chain --script "$TMP/fixtures/ep12_zh.ass" --json "$TMP/chain.json" >/dev/null
"$BIN" tag-stats --script "$TMP/fixtures/ep12_zh.ass" --top 5 >/dev/null
"$BIN" render-probe --script "$TMP/fixtures/ep12_zh.ass" --frame 1250 \
    --out "$TMP/probe.json" --dump-alpha "$TMP/alpha" >/dev/null
"$BIN" batch --dir "$TMP/fixtures" --out "$TMP/batch.json" >/dev/null

python3 - "$TMP" <<'PY'
import glob
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
chain = json.loads((root / "chain.json").read_text())
probe = json.loads((root / "probe.json").read_text())
batch = json.loads((root / "batch.json").read_text())
assert chain["events"] == 48 and chain["styles"] == 9
assert chain["max_chain_depth"] == 4 and chain["cycles"] == 0
assert chain["chains"]["ep12_zh"] == ["ep12_zh", "ep12", "season1", "template"]
assert probe["visible"] == 41
assert 1050 <= probe["max_width"] <= 1150
assert 90 <= probe["max_height"] <= 105
assert len(glob.glob(str(root / "alpha" / "*.ppm"))) == 41
assert [item["file"].split("/")[-1] for item in batch["files_detail"]] == [
    f"ep{i:02d}_zh.ass" for i in range(1, 13)
]
assert all(item["tags"] == 300 for item in batch["files_detail"])
print("smoke: ok", chain["max_chain_depth"], probe["max_width"], probe["max_height"])
PY
