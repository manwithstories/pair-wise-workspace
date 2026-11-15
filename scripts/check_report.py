#!/usr/bin/env python3
import json
import re
import sys

path = sys.argv[1] if len(sys.argv) > 1 else "out/report.json"
with open(path, encoding="utf-8") as f:
    report = json.load(f)
assert report["cues_total"] == 120, report["cues_total"]
assert 300 <= report["lines_total"] <= 380, report["lines_total"]
assert 600.0 <= float(report["max_line_width_px"]) <= 640.0, report["max_line_width_px"]
assert report["cues_ok"] + report["overflow_total"] == report["cues_total"]
assert re.fullmatch(r"[0-9a-f]{16}", report["report_hash"]), report["report_hash"]
assert len(report["cues"]) == report["cues_total"]
assert all("lines" in cue and "bbox" in cue for cue in report["cues"])
print("report check: OK", {k: report[k] for k in ("cues_total", "lines_total",
      "max_line_width_px", "report_hash")})
