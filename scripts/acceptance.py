"""End-to-end acceptance: regenerate fixtures, run align twice, verify
idempotency, quarantine exit code and the greppable summary lines."""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def run_align(env) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, "-m", "etl.cli", "align",
         "--config", "config/mapping.yaml"],
        cwd=ROOT, env=env, capture_output=True, text=True,
    )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--seed", type=int, default=7)
    args = parser.parse_args()

    sys.path.insert(0, os.path.join(ROOT, "scripts"))
    import gen_fixtures
    gen_fixtures.generate(ROOT, args.seed)

    run_dir = os.path.join(ROOT, "run")
    shutil.rmtree(run_dir, ignore_errors=True)

    env = dict(os.environ)
    env["PYTHONPATH"] = os.path.join(ROOT, "src") + os.pathsep + env.get(
        "PYTHONPATH", "")

    first = run_align(env)
    report_path = os.path.join(run_dir, "report.json")
    if not os.path.exists(report_path):
        print("ACCEPTANCE FAILED: first align produced no report",
              file=sys.stderr)
        print(first.stdout, file=sys.stderr)
        print(first.stderr, file=sys.stderr)
        return 1
    with open(report_path, encoding="utf-8") as fh:
        checksum1 = json.load(fh)["checksum"]

    second = run_align(env)
    with open(report_path, encoding="utf-8") as fh:
        checksum2 = json.load(fh)["checksum"]

    lines = second.stdout.strip().splitlines()
    for line in lines:
        if line.startswith(("SCHEMA", "FIELDS", "CONVERT", "LOAD")):
            print(line)
    equal = checksum1 == checksum2
    print(f"IDEMPOTENT checksum_pass1={checksum1} pass2={checksum2} "
          f"equal={str(equal).lower()}")
    for line in lines:
        if line.startswith("QUARANTINE"):
            print(line)
    print(f"EXIT {second.returncode}")

    checks = [
        ("align exit code 2 on both runs",
         first.returncode == 2 and second.returncode == 2),
        ("idempotent checksum", equal),
        ("quarantine file exists",
         os.path.exists(os.path.join(run_dir, "quarantine.csv"))),
    ]
    failed = [name for name, ok in checks if not ok]
    if failed:
        print("ACCEPTANCE FAILED: " + "; ".join(failed), file=sys.stderr)
        if second.stderr:
            print(second.stderr, file=sys.stderr)
        return 1
    print("ACCEPTANCE OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
