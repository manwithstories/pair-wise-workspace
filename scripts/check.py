#!/usr/bin/env python3
"""Verify an on-disk summary (s.json) against the expected subset (e.json)
plus structural invariants that must hold for every cue."""
import argparse
import json
import sys

TOL = 64  # 1px in 26.6


def fail(msg):
    print("FAIL %s" % msg)
    return 1


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-s", "--summary", required=True)
    ap.add_argument("-e", "--expected", required=True)
    args = ap.parse_args()
    with open(args.summary, encoding="utf-8") as f:
        s = json.load(f)
    with open(args.expected, encoding="utf-8") as f:
        e = json.load(f)

    errors = 0
    stats = s["stats"]
    for key in ("cues", "overflow_count", "missing_glyph"):
        if stats[key] != e[key]:
            errors += fail("stats.%s: got %s want %s" % (key, stats[key], e[key]))

    cues = {c["cue_id"]: c for c in s["cues"]}
    if len(cues) != len(s["cues"]):
        errors += fail("duplicate cue_id in summary")

    # golden cues
    for g in e["golden"]:
        c = cues.get(g["cue_id"])
        if c is None:
            errors += fail("golden %s missing" % g["cue_id"])
            continue
        if g["line_count"] is not None and c["line_count"] != g["line_count"]:
            errors += fail("%s line_count %s != %s" %
                           (g["cue_id"], c["line_count"], g["line_count"]))
        if g["breaks"] is not None and c["breaks"] != g["breaks"]:
            errors += fail("%s breaks %s != %s" %
                           (g["cue_id"], c["breaks"], g["breaks"]))
        if g["width"] is not None:
            w = c["bbox"]["x_max"] - c["bbox"]["x_min"]
            if abs(w - g["width"]) > TOL:
                errors += fail("%s width %s != %s" % (g["cue_id"], w, g["width"]))
        if c["overflow"] != g["overflow"]:
            errors += fail("%s overflow %s != %s" %
                           (g["cue_id"], c["overflow"], g["overflow"]))
        if c["missing_glyph"] != g["missing_glyph"]:
            errors += fail("%s missing_glyph mismatch" % g["cue_id"])

    # structural invariants for every cue
    for c in s["cues"]:
        cid = c["cue_id"]
        if c["breaks"] != sorted(c["breaks"]):
            errors += fail("%s breaks not sorted" % cid)
        if c["line_count"] != len(c["lines"]):
            errors += fail("%s line_count != len(lines)" % cid)
        if c["line_count"] == 0:
            if c["breaks"]:
                errors += fail("%s empty cue with breaks" % cid)
        elif c["line_count"] != len(c["breaks"]) + 1:
            errors += fail("%s line_count != breaks+1" % cid)
        if c["overflow"] != (c["overflow_px"] > 0 or len(c["rules"]) > 0):
            errors += fail("%s overflow flag inconsistent" % cid)
        if c["lines"]:
            x0 = min(l["bbox"]["x_min"] for l in c["lines"])
            y0 = min(l["bbox"]["y_min"] for l in c["lines"])
            x1 = max(l["bbox"]["x_max"] for l in c["lines"])
            y1 = max(l["bbox"]["y_max"] for l in c["lines"])
            bb = c["bbox"]
            if (bb["x_min"], bb["y_min"], bb["x_max"], bb["y_max"]) != \
               (x0, y0, x1, y1):
                errors += fail("%s bbox != union of line bboxes" % cid)
    ov = {o["cue_id"] for o in s["overflow_list"]}
    want = {c["cue_id"] for c in s["cues"] if c["overflow"]}
    if ov != want:
        errors += fail("overflow_list inconsistent with cues")

    if errors:
        print("check failed: %d error(s)" % errors)
        return 1
    print("check ok: cues=%d glyphs=%d breaks=%d overflow=%d missing=%d" %
          (stats["cues"], stats["glyphs"], stats["breaks"],
           stats["overflow_count"], stats["missing_glyph"]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
