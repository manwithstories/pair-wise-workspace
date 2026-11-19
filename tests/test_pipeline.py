"""Pipeline tests: success, corruption and boundary paths.

The sample generator is invoked in a tmp directory so tests are deterministic
and never touch the real input/output locations.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import numpy as np
import pytest

from remuxsync import annexb, index as index_mod
from remuxsync.cli import main as cli_main
from remuxsync.clock import (
    TICKS_PER_MS,
    WRAP_HALF,
    WRAP_MODULUS,
    fit_drift,
    unwrap_timestamps,
)
from remuxsync.errors import ErrorLog, InputError, Recovery, Stage
from remuxsync.packets import (
    FLAG_KEY,
    HEADER_SIZE,
    encode_packet,
    parse_stream,
)
from remuxsync.pipeline import run

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "make_samples.py"


def _make_samples(tmp_path: Path, seed: int = 41) -> Path:
    out = tmp_path / "samples"
    subprocess.run(
        [sys.executable, str(SCRIPT), "--out", str(out), "--seed", str(seed)],
        check=True, capture_output=True)
    return out


# --------------------------------------------------------------------------
# clock: unwrap and drift
# --------------------------------------------------------------------------

def test_unwrap_distinguishes_wrap_from_regression():
    seq = [WRAP_MODULUS - 100, WRAP_MODULUS - 10, 5, 20]
    out, wraps = unwrap_timestamps(seq)
    assert wraps == [2]                       # wrap detected at the rollover
    assert out[2] == WRAP_MODULUS + 5         # monotonic after unwrap
    assert out == sorted(out)

    # a small genuine backward step is not a wrap
    back, back_wraps = unwrap_timestamps([1000, 900, 1100])
    assert back_wraps == []
    assert back[1] == 900


def test_fit_drift_splits_on_persistent_step():
    xs = list(range(0, 200, 2))
    ys = [x + (100 * TICKS_PER_MS if x >= 100 else 0) for x in xs]
    model, breaks = fit_drift(xs, ys)
    assert breaks == [50]
    assert len(model.segments) == 2


# --------------------------------------------------------------------------
# packets: recovery paths
# --------------------------------------------------------------------------

def _write(path: Path, blob: bytes) -> None:
    path.write_bytes(blob)


def test_packet_loss_retransmission_and_duplicate(tmp_path):
    p = tmp_path / "t.pkt"
    blob = b"".join([
        encode_packet(0, 100, b"a", 0),
        encode_packet(2, 300, b"c", 0),                       # seq 1 lost
        encode_packet(1, 200, b"b", 0b100),                   # retransmitted
        encode_packet(3, 400, b"d", 0),
        encode_packet(3, 400, b"d", 0b100),                   # duplicate
    ])
    _write(p, blob)
    log = ErrorLog()
    ps = parse_stream(str(p), "t", log, 120)
    assert [q.seq for q in ps.packets] == [0, 1, 2, 3]
    assert not ps.unrecovered_seqs
    kinds = {r.recovery for r in log.records}
    assert Recovery.RETRANSMITTED.value in kinds
    assert Recovery.DEDUPLICATED.value in kinds


def test_reorder_within_window_and_late_unrecoverable(tmp_path):
    p = tmp_path / "tmp.pkt"
    # frames 1 and 2 swapped; each ~33 ms apart, inside the 120 ms window
    blob = b"".join([
        encode_packet(0, 0, b"0", 0),
        encode_packet(2, 2 * 3000, b"2", 0),
        encode_packet(1, 1 * 3000, b"1", 0),
        encode_packet(3, 3 * 3000, b"3", 0),
    ])
    _write(p, blob)
    log = ErrorLog()
    ps = parse_stream(str(p), "tmp", log, 120)
    assert [q.seq for q in ps.packets] == [0, 1, 2, 3]
    assert any(r.recovery == Recovery.REORDERED.value for r in log.records)

    # same data but a 33 ms window cannot hold the displaced packet
    log2 = ErrorLog()
    ps2 = parse_stream(str(p), "tmp", log2, 10)
    assert ps2.unrecovered_seqs
    assert any(r.recovery == Recovery.UNRECOVERABLE.value for r in log2.records)


def test_timestamp_wrap_recovered(tmp_path):
    p = tmp_path / "w.pkt"
    near = WRAP_MODULUS - 3000
    blob = b"".join([
        encode_packet(0, near, b"a", 0),
        encode_packet(1, (near + 3000) & (WRAP_MODULUS - 1), b"b", 0),
    ])
    _write(p, blob)
    log = ErrorLog()
    ps = parse_stream(str(p), "w", log, 120)
    assert ps.packets[1].ts_unwrapped > ps.packets[0].ts_unwrapped
    assert any(r.recovery == Recovery.UNWRAPPED.value for r in log.records)


def test_truncated_header_is_unrecoverable(tmp_path):
    p = tmp_path / "h.pkt"
    _write(p, encode_packet(0, 0, b"x", 0) + b"\x52\x58\x00")  # cut header
    log = ErrorLog()
    ps = parse_stream(str(p), "h", log, 120)
    assert ps.packets and any(
        r.recovery == Recovery.UNRECOVERABLE.value for r in log.records)


# --------------------------------------------------------------------------
# annexb scanner boundary defects
# --------------------------------------------------------------------------

def test_annexb_truncated_head_and_tail(tmp_path):
    p = tmp_path / "x.h264"
    good = b"\x00\x00\x00\x01\x65" + b"k" * 8       # IDR
    p.write_bytes(b"\xff\xff" + good + b"\x00\x00\x00\x01\x65ab")
    res = annexb.scan(str(p), "h264", tail_complete=False)
    msgs = " ".join(e.message for e in res.errors)
    assert "truncated head" in msgs
    assert "incomplete tail" in msgs
    assert res.errors[0].byte_offset == 0


def test_av1_invalid_leb_and_forbidden_bit(tmp_path):
    p = tmp_path / "x.av1"
    # forbidden bit set
    p.write_bytes(bytes([0x80]))
    res = annexb.scan(str(p), "av1")
    assert res.errors and "forbidden bit" in res.errors[0].message
    assert res.errors[0].byte_offset == 0


# --------------------------------------------------------------------------
# index validation and rebuild
# --------------------------------------------------------------------------

def test_index_quarantine_and_seek_verify():
    log = ErrorLog()
    key_offsets = {0, 100, 200}
    entries = [
        index_mod.IndexEntry(0, 0),
        index_mod.IndexEntry(1, 999),             # out of bounds
        index_mod.IndexEntry(0, 0),               # duplicate
        index_mod.IndexEntry(2, 50),              # not a keyframe
        index_mod.IndexEntry(3, 200),             # good
    ]
    valid = index_mod.validate_index(entries, 300, key_offsets, log, "t")
    assert [e.frame for e in valid] == [0, 3]
    assert len([r for r in log.records
                if r.recovery == Recovery.QUARANTINED.value]) == 3

    rebuilt = index_mod.rebuild_index(
        [type("U", (), {"offset": o})() for o in sorted(key_offsets)],
        {0: 0, 100: 5, 200: 10}, {0: 0, 100: 1, 200: 2})
    failures = index_mod.verify_seeks(rebuilt, key_offsets, log, "t")
    assert failures == 0
    assert rebuilt.seek(7).frame == 5            # nearest preceding keyframe


def test_empty_index_seek_failure():
    log = ErrorLog()
    failures = index_mod.verify_seeks(
        index_mod.RandomAccessIndex(), {0}, log, "t")
    assert failures == 1
    assert log.unrecovered


# --------------------------------------------------------------------------
# end-to-end pipeline and CLI exit codes
# --------------------------------------------------------------------------

def test_full_pipeline_success(tmp_path):
    samples = _make_samples(tmp_path)
    out = tmp_path / "out"
    result = run(str(samples), str(out), reorder_window_ms=120)
    s = result.summary
    assert s["sync_error_ms_p95"] <= 20
    assert s["packet_recovery_rate"] >= 0.90
    assert s["latency_ms_p95"] <= 500
    assert s["index_seek_failures"] == 0
    assert not result.errors.unrecovered
    # every error carries a stage; byte offsets present where applicable
    stages = {e.stage for e in result.errors.records}
    assert {st.value for st in Stage} & stages
    for v in result.videos:
        assert len(v.frames) == 120
        assert len(v.drift.segments) >= 2          # clock step segmented
    # repaired index files published and seek targets land on keyframes
    for v in result.videos:
        idx_file = out / f"{v.track}.{v.codec}.index.repaired.json"
        data = json.loads(idx_file.read_text())
        assert data["entries"]
    # input directory untouched (no outputs written inside it)
    assert not (samples / "out").exists()


def test_cli_exit_codes(tmp_path, capsys):
    samples = _make_samples(tmp_path)

    # exit 2: unreadable input
    assert cli_main(["repair", "--input", str(tmp_path / "nope"),
                     "--output", str(tmp_path / "o"), "--json"]) == 2
    capsys.readouterr()

    # exit 2: output nested inside input (refuse to overwrite sources)
    assert cli_main(["repair", "--input", str(samples),
                     "--output", str(samples / "nested"),
                     "--json"]) == 2
    capsys.readouterr()

    # exit 2: invalid window argument
    assert cli_main(["repair", "--input", str(samples),
                     "--output", str(tmp_path / "o2"),
                     "--reorder-window-ms", "0"]) == 2
    capsys.readouterr()

    # exit 0: healthy run
    ok = tmp_path / "ok"
    assert cli_main(["repair", "--input", str(samples),
                     "--output", str(ok),
                     "--reorder-window-ms", "120", "--json"]) == 0
    report = json.loads(capsys.readouterr().out)
    assert report["index_seek_failures"] == 0
    assert report["packet_recovery_rate"] >= 0.90


def test_corruption_forces_exit_one(tmp_path):
    samples = _make_samples(tmp_path)
    # drop a video packet file's middle packet entirely with no retransmission
    pkt = next(samples.glob("cam_studio*.pkt"))
    blob = pkt.read_bytes()
    # remove the packet whose header starts at HEADER_SIZE (seq 0 stays valid)
    _magic, flags, seq, length, ts = __import__(
        "remuxsync.packets", fromlist=["HEADER_STRUCT"]
    ).HEADER_STRUCT.unpack(blob[:HEADER_SIZE])
    pkt.write_bytes(blob[:HEADER_SIZE] + blob[HEADER_SIZE + HEADER_SIZE + length:])
    out = tmp_path / "out"
    code = cli_main(["repair", "--input", str(samples),
                     "--output", str(out), "--reorder-window-ms", "120",
                     "--json"])
    assert code == 1


def test_deterministic_samples_and_audio_offset(tmp_path):
    a = _make_samples(tmp_path / "run1", seed=7)
    b = _make_samples(tmp_path / "run2", seed=7)
    assert (a / "manifest.json").read_bytes() == (b / "manifest.json").read_bytes()
    result = run(str(a), str(tmp_path / "out"), 120)
    lav = next(t for t in result.audios if t.track == "audio_lav")
    # measured 60 ms capture offset within one 48 kHz sample (~0.02 ms)
    assert abs(lav.measured_offset_ticks - 60 * TICKS_PER_MS) <= 2
    assert lav.overrun_ticks > 0                   # retained, never clipped


def test_pyav_probe_parses_recovered_streams(tmp_path):
    from remuxsync.probe import probe_stream
    samples = _make_samples(tmp_path)
    out = tmp_path / "out"
    run(str(samples), str(out), 120)
    for codec in ("h264", "h265", "av1"):
        es = next(out.glob(f"*.{codec}.recovered.es"))
        info = probe_stream(str(es), codec)
        assert info["available"] is True
        assert info["parsed_packets"] > 0  # framing independently confirmed
