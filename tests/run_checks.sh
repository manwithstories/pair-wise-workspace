#!/usr/bin/env bash
set -u

BIN=${1:?binary path required}
PYTHON=${2:?python path required}
GENERATOR=${3:?generator path required}
TMP=$(mktemp -d "${TMPDIR:-/tmp}/mp4pack-checks.XXXXXX")
trap 'rm -rf "$TMP"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

expect_status() {
    local expected=$1
    local actual=$2
    local label=$3
    [[ "$actual" -eq "$expected" ]] || fail "$label: expected exit $expected, got $actual"
}

"$PYTHON" "$GENERATOR" --out "$TMP/fixtures" >/dev/null || fail "fixture generation"
DRONE="$TMP/fixtures/drone_a"
OUT="$TMP/out"

"$BIN" index "$DRONE" --out "$OUT/index.json" >"$TMP/index.txt" 2>&1
expect_status 2 $? "index"
grep -qx 'segments_found=12' "$TMP/index.txt" || fail "index segments_found"
grep -qx 'segments_after_repack=11' "$TMP/index.txt" || fail "index segments_after_repack"
grep -qx 'missing_sequences=\[7\]' "$TMP/index.txt" || fail "index missing_sequences"
grep -qx 'seq_contiguous=false' "$TMP/index.txt" || fail "index seq_contiguous"

"$PYTHON" - "$OUT/index.json" <<'PY' || fail "index JSON assertions"
import json
import sys
with open(sys.argv[1], encoding="utf-8") as source:
    index = json.load(source)
assert index["segments_found"] == 12
assert index["segments_after_repack"] == 11
assert index["missing_sequences"] == [7]
assert index["seq_contiguous"] is False
assert index["duplicate_count"] == 1
assert index["out_of_order_count"] > 0
assert len(index["tracks"]) == 2
tracks = {track["track_id"]: track for track in index["tracks"]}
assert tracks[1]["tfdt_versions"] == [0, 1]
assert tracks[1]["trun_versions"] == [0]
assert tracks[2]["trun_versions"] == [1]
audio = [track for segment in index["segments"] for track in segment["tracks"] if track["track_id"] == 2]
assert all(track["composition_offset_min"] == -512 for track in audio)
assert any(track["tfdt_version"] == 0 and track["tfdt_raw"] < 100000 and
           track["base_decode_time"] > (1 << 32) for track in audio)
PY

"$BIN" validate --index "$OUT/index.json" --init "$DRONE/init.mp4" --out "$OUT/validate.json" >"$TMP/validate.txt" 2>&1
expect_status 2 $? "validate"
"$PYTHON" - "$OUT/validate.json" <<'PY' || fail "validate JSON assertions"
import json
import sys
with open(sys.argv[1], encoding="utf-8") as source:
    result = json.load(source)
assert result["compatible"] is True
assert result["verdict"] == "missing_segment"
assert result["missing_sequences"] == [7]
assert all(check["ok"] for check in result["checks"])
PY

"$BIN" validate --index "$OUT/index.json" --init "$TMP/fixtures/incompatible_init.mp4" --out "$OUT/incompatible.json" >"$TMP/incompatible.txt" 2>&1
expect_status 3 $? "incompatible validate"
"$PYTHON" - "$OUT/incompatible.json" <<'PY' || fail "incompatible JSON assertions"
import json
import sys
with open(sys.argv[1], encoding="utf-8") as source:
    result = json.load(source)
assert result["compatible"] is False
assert result["verdict"] == "init_incompatible"
assert any(not check["ok"] for check in result["checks"])
PY

"$BIN" repack --index "$OUT/index.json" --init "$DRONE/init.mp4" --out "$OUT/repacked.mp4" >"$TMP/repack.txt" 2>&1
expect_status 2 $? "repack"
[[ -s "$OUT/repacked.mp4" ]] || fail "repacked output missing"
[[ ! -e "$OUT/repacked.mp4.tmp" ]] || fail "repack left a partial file"

"$BIN" report --index "$OUT/index.json" --repacked "$OUT/repacked.mp4" --out "$OUT/report.json" >"$TMP/report.txt" 2>&1
expect_status 2 $? "report"
for expected in \
    'segments_found=12' \
    'segments_after_repack=11' \
    'missing_sequences=[7]' \
    'seq_contiguous=false' \
    'tracks=2' \
    'video.max_deviation_ms=0.000' \
    'audio.max_deviation_ms=0.333' \
    'verdict=missing_segment'; do
    grep -Fqx "$expected" "$TMP/report.txt" || fail "report line: $expected"
done

"$PYTHON" - "$OUT/repacked.mp4" "$OUT/report.json" <<'PY' || fail "repacked structure assertions"
import json
import struct
import sys

with open(sys.argv[1], "rb") as source:
    data = source.read()

def boxes(offset, end):
    while offset < end:
        assert end - offset >= 8
        size, kind = struct.unpack(">I4s", data[offset:offset + 8])
        assert size >= 8 and offset + size <= end
        yield kind, offset, size
        offset += size

roots = list(boxes(0, len(data)))
assert sum(kind == b"ftyp" for kind, _, _ in roots) == 1
assert sum(kind == b"moov" for kind, _, _ in roots) == 1
sequences = []
decode = {1: [], 2: []}
for kind, offset, size in roots:
    if kind != b"moof":
        continue
    children = list(boxes(offset + 8, offset + size))
    mfhd = next(item for item in children if item[0] == b"mfhd")
    sequences.append(struct.unpack(">I", data[mfhd[1] + 12:mfhd[1] + 16])[0])
    for child_kind, child_offset, child_size in children:
        if child_kind != b"traf":
            continue
        traf_children = list(boxes(child_offset + 8, child_offset + child_size))
        tfhd = next(item for item in traf_children if item[0] == b"tfhd")
        tfdt = next(item for item in traf_children if item[0] == b"tfdt")
        track_id = struct.unpack(">I", data[tfhd[1] + 12:tfhd[1] + 16])[0]
        version = data[tfdt[1] + 8]
        value = struct.unpack(">I", data[tfdt[1] + 12:tfdt[1] + 16])[0] if version == 0 else \
            struct.unpack(">Q", data[tfdt[1] + 12:tfdt[1] + 20])[0]
        decode[track_id].append(value)
assert sequences == list(range(1, 12))
for track_id, values in decode.items():
    assert len(values) == 11
    assert all(left < right for left, right in zip(values, values[1:]))
assert any(value > (1 << 32) for value in decode[1])

with open(sys.argv[2], encoding="utf-8") as source:
    report = json.load(source)
assert report["verdict"] == "missing_segment"
by_name = {track["name"]: track for track in report["deviations"]}
assert by_name["video"]["max_deviation_ms"] < 0.0005
assert abs(by_name["audio"]["max_deviation_ms"] - 1 / 3) < 0.0005
PY

"$BIN" index "$TMP/fixtures/drone_box64" --out "$OUT/box64.json" >"$TMP/box64.txt" 2>&1
expect_status 0 $? "64-bit/uuid index"
"$BIN" index "$TMP/fixtures/drone_hvc" --out "$OUT/hvc.json" >"$TMP/hvc.txt" 2>&1
expect_status 0 $? "hvcC index"
grep -q '"config_type": "hvcC"' "$OUT/hvc.json" || fail "hvcC config was not parsed"

for fixture in malformed malformed_size_zero malformed_uuid; do
    "$BIN" index "$TMP/fixtures/$fixture" --out "$OUT/$fixture.json" >"$TMP/$fixture.txt" 2>&1
    expect_status 4 $? "$fixture"
    grep -q 'byte offset' "$TMP/$fixture.txt" || fail "$fixture error lacks byte offset"
done

touch "$TMP/not-a-directory"
"$BIN" index "$DRONE" --out "$TMP/not-a-directory/index.json" >"$TMP/write.txt" 2>&1
expect_status 5 $? "write failure"

echo "all checks passed"
