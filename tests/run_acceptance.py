#!/usr/bin/env python3
"""End-to-end acceptance: fixtures, sniff, verify, pack, query, negatives."""

import json
import os
import shutil
import sqlite3
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FAILURES = []


def run(args, expect_code):
    proc = subprocess.run([sys.executable] + args, cwd=ROOT,
                          capture_output=True, text=True, timeout=60)
    check(proc.returncode == expect_code,
          "%s -> exit %d (want %d)\n%s%s" % (
              " ".join(args), proc.returncode, expect_code,
              proc.stdout, proc.stderr))
    return proc


def check(ok, label):
    print("%s %s" % ("PASS" if ok else "FAIL", label))
    if not ok:
        FAILURES.append(label)


def main():
    for name in ("samples", "reports", "var", "dist"):
        shutil.rmtree(os.path.join(ROOT, name), ignore_errors=True)

    proc = run(["tools/make_fixtures.py", "--out", "samples", "--seed", "7"], 0)
    check("generated 14 fixtures" in proc.stdout, "fixtures: 14 generated")
    check(len(os.listdir(os.path.join(ROOT, "samples"))) == 14,
          "fixtures: 14 files on disk")

    proc = run(["-m", "mediaqa.cli", "sniff", "samples",
                "--out", "reports/sniff.json", "--db", "var/mediaqa.sqlite3"],
               4)
    check("files=14 tracks=23 bad=2" in proc.stdout, "sniff: summary line")
    with open(os.path.join(ROOT, "reports/sniff.json")) as fh:
        doc = json.load(fh)
    check((doc["files"], doc["tracks"], doc["bad"]) == (14, 23, 2),
          "sniff: JSON totals")
    check(all(set(f) >= {"track_count", "duration_us", "codecs"}
              for f in doc["files_detail"]), "sniff: per-file codec summary")

    proc = run(["-m", "mediaqa.cli", "verify", "deliveries/ep01.spec.json",
                "--db", "var/mediaqa.sqlite3", "--out", "reports/ep01.json"],
               3)
    with open(os.path.join(ROOT, "reports/ep01.json")) as fh:
        doc = json.load(fh)
    check(doc["track_count"] == 4, "verify: track_count=4")
    check(doc["max_duration_delta_us"] == 33333, "verify: delta=33333us")
    check(doc["finding_count"] == 3 and doc["errors"] == 1
          and doc["warnings"] == 2, "verify: 3 findings (1 error, 2 warning)")
    check(doc["exit_code"] == 3, "verify: exit_code field")

    proc = run(["-m", "mediaqa.cli", "pack", "deliveries/ep01.spec.json",
                "--out", "dist/ep01", "--verify",
                "--db", "var/mediaqa.sqlite3"], 0)
    check("roundtrip=ok tracks=4 duration_delta_us=0" in proc.stdout,
          "pack: roundtrip ok, 4 tracks, delta 0")

    proc = run(["-m", "mediaqa.cli", "query", "--db", "var/mediaqa.sqlite3",
                "--severity", "error", "--limit", "20"], 0)
    check("ZERO_ALIGN" in proc.stdout and "total=" in proc.stdout,
          "query: error rows plus total")
    proc = run(["-m", "mediaqa.cli", "query", "--db", "var/mediaqa.sqlite3",
                "--rule-id", "UNKNOWN_BRAND", "--container", "mp4"], 0)
    check("total=1" in proc.stdout, "query: rule_id + container filter")

    bad_spec = os.path.join(ROOT, "var", "bad.spec.json")
    with open(bad_spec, "w") as fh:
        json.dump({"package_id": "bad",
                   "sources": [
                       {"path": "../samples/wav_recpen_44k_mono24.wav"}]}, fh)
    proc = run(["-m", "mediaqa.cli", "pack", "var/bad.spec.json",
                "--out", "dist/bad"], 5)
    check("PACK_INCOMPATIBLE" in proc.stdout, "pack: whitelist rejection")

    proc = run(["-m", "mediaqa.cli", "sniff", ".."], 1)
    check("escapes workspace" in proc.stderr, "cli: path escape rejected")

    run(["-m", "mediaqa.cli", "pack", "deliveries/ep01.spec.json",
         "--out", "dist/ep01", "--verify", "--db", "var/mediaqa.sqlite3"], 0)
    conn = sqlite3.connect(os.path.join(ROOT, "var", "mediaqa.sqlite3"))
    count = conn.execute("SELECT COUNT(*) FROM packages").fetchone()[0]
    conn.close()
    check(count == 1, "store: package idempotent by sha256")

    print("---")
    if FAILURES:
        print("ACCEPTANCE FAILED: %d check(s)" % len(FAILURES))
        return 1
    print("ACCEPTANCE OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
