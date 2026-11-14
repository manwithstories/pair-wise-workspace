#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
./build/subqc gen-fixtures --out fixtures --events 240 --seed 20261114

mkdir -p out
set +e
./build/subqc scan --input fixtures --width 1920 --height 1080 \
  --font-dir fixtures/fonts --json out/report.json --text | tee out/report.txt
STATUS=${PIPESTATUS[0]}
set -e

python3 - "$STATUS" <<'PY'
import json
import sys

status = int(sys.argv[1])
report = json.load(open("out/report.json", encoding="utf-8"))
summary = report["summary"]

assert status == 1, status
assert summary["files"] == 12, summary
assert summary["events"] == 240, summary
assert summary["styles"] == 38, summary
assert summary["margin_violations"] == 3, summary
assert report["inline_tag_counts"]["fs"] == 96, report["inline_tag_counts"]
assert report["inline_tag_counts"]["r"] == 41, report["inline_tag_counts"]
assert report["inline_tag_counts"]["t"] == 17, report["inline_tag_counts"]
assert report["unknown_tag_counts"]["zzz"] == 19, report["unknown_tag_counts"]
assert len(report["style_chains"]) == 38
assert len(report["render"]) == 240
assert all(isinstance(row["chain"], list) for row in report["style_chains"])

required = {"event", "style", "x0", "y0", "x1", "y1", "w", "h", "glyphs", "crc32"}
assert all(required <= set(row) for row in report["render"])
assert all(row["rendered"] and row["w"] >= 100 and row["h"] >= 100 for row in report["render"])
assert {s["style"] for s in report["render"][0]["segments"]} == {"Default"}
assert "Hero" in {s["style"] for s in report["render"][12]["segments"]}
assert any("->" in line for line in open("out/report.txt", encoding="utf-8"))
print("acceptance OK")
PY

./build/subqc scan --input fixtures --width 1920 --height 1080 \
  --font-dir fixtures/fonts --json out/report-replay.json >/dev/null || test $? -eq 1
python3 - <<'PY'
import json

first = json.load(open("out/report.json", encoding="utf-8"))["render"]
second = json.load(open("out/report-replay.json", encoding="utf-8"))["render"]
keys = ("event", "x0", "y0", "x1", "y1", "w", "h", "glyphs", "crc32")
assert [{k: row[k] for k in keys} for row in first] == [{k: row[k] for k in keys} for row in second]
print("deterministic replay OK")
PY
