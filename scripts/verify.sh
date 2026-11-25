#!/bin/sh
# scripts/verify.sh — the delivery acceptance gate.
#
# Runs the exact command sequence from the spec and asserts the demo's output
# stays inside the delivery thresholds. Exits non-zero on any deviation so CI
# and a broadcast engineer's laptop produce the same answer.
set -eu

cd "$(dirname "$0")/.."

GO=${GO:-go1.25.6}
SEED=${SEED:-73}
BUDGET_MIB=${BUDGET_MIB:-32}
SLOW_CONSUMER=${SLOW_CONSUMER:-25ms}

say() { printf '%s\n' "$*" >&2; }

say "== go version"
"$GO" version

say "== mod tidy"
# The spec's acceptance chain runs tidy before build. Go treats a toolchain
# line equal to the go directive as redundant and removes it; the repository
# ships with it because the delivery contract requires it, and tidy normalises
# it away here so the subsequent build is not blocked by -mod=readonly.
"$GO" mod tidy

say "== build"
"$GO" build ./...

say "== test"
"$GO" test ./...

say "== demo"
out=$(mktemp)
trap 'rm -f "$out"' EXIT
"$GO" run ./cmd/mediapipe demo --seed "$SEED" --budget-mib "$BUDGET_MIB" \
    --slow-consumer "$SLOW_CONSUMER" | tee "$out"

say "== thresholds"
awk -v budget_bytes=$((BUDGET_MIB * 1024 * 1024)) '
function field(line, name,   n, a, i, kv) {
    n = split(line, a, " ")
    for (i = 1; i <= n; i++) {
        if (index(a[i], name "=") == 1) {
            return substr(a[i], length(name) + 2)
        }
    }
    return ""
}
BEGIN { failed = 0 }
/^case=/ {
    name = field($0, "case")
    delta = field($0, "delta_pct") + 0
    mismatch = field($0, "payload_hash_mismatch") + 0
    crc = field($0, "crc_errors") + 0
    skew = field($0, "manifest_skew_ns") + 0
    peak = field($0, "buffer_peak_bytes") + 0
    exitc = field($0, "exit") + 0

    if (name == "success" || name == "late") {
        if (delta > 0.5 || delta < -0.5) {
            printf "FAIL %s: delta_pct %.4f outside ±0.5%%\n", name, delta > "/dev/stderr"
            failed = 1
        }
        if (mismatch != 0) {
            printf "FAIL %s: payload_hash_mismatch %d\n", name, mismatch > "/dev/stderr"
            failed = 1
        }
        if (crc != 0) {
            printf "FAIL %s: crc_errors %d\n", name, crc > "/dev/stderr"
            failed = 1
        }
        if (skew > 1000000) {
            printf "FAIL %s: manifest_skew_ns %d > 1000000\n", name, skew > "/dev/stderr"
            failed = 1
        }
        if (peak > budget_bytes || peak > 33554432) {
            printf "FAIL %s: buffer_peak_bytes %d exceeds budget\n", name, peak > "/dev/stderr"
            failed = 1
        }
        if (exitc != 0) {
            printf "FAIL %s: exit %d\n", name, exitc > "/dev/stderr"
            failed = 1
        }
    }
    if (name == "truncated" && exitc != 2) {
        printf "FAIL truncated: exit %d, want 2\n", exitc > "/dev/stderr"
        failed = 1
    }
    if (name == "beyond-horizon" && exitc != 3) {
        printf "FAIL beyond-horizon: exit %d, want 3\n", exitc > "/dev/stderr"
        failed = 1
    }
    if (name == "interrupted" && exitc != 4) {
        printf "FAIL interrupted: exit %d, want 4\n", exitc > "/dev/stderr"
        failed = 1
    }
    if (name == "unsupported" && exitc != 3) {
        printf "FAIL unsupported: exit %d, want 3\n", exitc > "/dev/stderr"
        failed = 1
    }
    if (name == "resume") {
        if (field($0, "resume_digest_equal") != "true") {
            printf "FAIL resume: resume_digest_equal is not true\n" > "/dev/stderr"
            failed = 1
        }
        if (exitc != 0) {
            printf "FAIL resume: exit %d\n", exitc > "/dev/stderr"
            failed = 1
        }
    }
}
END {
    if (!failed) {
        printf "all thresholds met\n" > "/dev/stderr"
    }
    exit failed
}' "$out"

say "== replay stability"
out2=$(mktemp)
trap 'rm -f "$out" "$out2"' EXIT
"$GO" run ./cmd/mediapipe demo --seed "$SEED" --budget-mib "$BUDGET_MIB" \
    --slow-consumer "$SLOW_CONSUMER" > "$out2"
if ! diff -q "$out" "$out2" >/dev/null; then
    say "FAIL: two identical runs produced different reports"
    diff "$out" "$out2" >&2 || true
    exit 1
fi
say "replay digests identical across runs"

say "OK"
