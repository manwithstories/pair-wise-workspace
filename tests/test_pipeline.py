"""End-to-end tests for the talkmap segmentation pipeline."""

from __future__ import annotations

import json
import sqlite3
import struct
import subprocess
import sys
import sysconfig
from pathlib import Path

import numpy as np
import pytest

from talkmap.config import Params
from talkmap.errors import AudioReadError, ParamError, QueryEmpty
from talkmap.framing import Framer, rms_to_dbfs
from talkmap.refine import run_pipeline
from talkmap.store import Store
from talkmap.synth import synth_fixtures
from talkmap.wavio import WavReader, write_pcm_wav

REPO = Path(__file__).resolve().parents[1]
TEST_TMP = REPO / ".test_tmp"


def _invoke(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, "-m", "talkmap", *args],
        cwd=REPO, capture_output=True, text=True, check=False)


@pytest.fixture(scope="module")
def fixtures():
    import shutil
    if TEST_TMP.exists():
        shutil.rmtree(TEST_TMP)
    TEST_TMP.mkdir(parents=True)
    out = TEST_TMP / "fixtures"
    result = synth_fixtures(str(out), seed=7, masters=4, minutes=1)
    return out, result


def test_synth_creates_four_masters_and_truth(fixtures):
    out, result = fixtures
    wavs = sorted(out.glob("*.wav"))
    assert len(wavs) == 4
    assert (out / "truth.json").exists()
    truth = json.loads((out / "truth.json").read_text())
    assert len(truth) == 4
    for entry in truth.values():
        assert entry["sample_rate"] == 48000
        assert entry["segments"]


def test_wav_reader_supports_all_bit_depths_and_channels(fixtures):
    out, _ = fixtures
    for wav in sorted(out.glob("*.wav")):
        with WavReader(wav) as reader:
            assert reader.info.sample_rate == 48000
            assert reader.info.bits_per_sample in (16, 24, 32)
            assert reader.info.channels in (1, 2)
            block = next(reader.iter_mono_blocks(48000))
            assert block.ndim == 1 and block.size > 0


def test_pipeline_segments_are_ordered_and_non_overlapping(fixtures):
    out, _ = fixtures
    params = Params()
    for wav in sorted(out.glob("*.wav")):
        with WavReader(wav) as reader:
            segments, total = run_pipeline(reader, params)
        assert segments
        for prev, cur in zip(segments, segments[1:]):
            assert prev.end_sample <= cur.start_sample
            assert cur.start_sample < cur.end_sample
        assert segments[-1].end_sample <= total
        assert segments[0].start_sample >= 0


def test_framer_does_not_zero_pad_tail():
    framer = Framer(48000, 20.0, 10.0)
    emitted = framer.push(np.zeros(framer.frame_samples + framer.hop_samples))
    leftover = framer.finish()
    assert len(emitted) == 2
    assert leftover == framer.hop_samples


def test_dbfs_round_trip():
    rms = np.asarray([1.0, 0.1, 0.0])
    db = rms_to_dbfs(rms)
    assert db[0] == pytest.approx(0.0, abs=1e-9)
    assert db[1] == pytest.approx(-20.0, abs=1e-9)
    assert db[2] < -200


def test_non_pcm_wav_exits_three(tmp_path):
    payload = (
        struct.pack("<4sI4s", b"RIFF", 60, b"WAVE")
        + struct.pack("<4sIHHIIHH", b"fmt ", 16, 3, 1, 48000, 96000, 2, 16)
        + struct.pack("<4sI", b"data", 24) + b"\0" * 24)
    path = TEST_TMP / "float.wav"
    path.write_bytes(payload)
    with pytest.raises(AudioReadError):
        WavReader(path)


def test_unknown_chunk_is_skipped(tmp_path):
    fmt = struct.pack("<4sIHHIIHH", b"fmt ", 16, 1, 1, 48000, 96000, 2, 16)
    junk = struct.pack("<4sI", b"JUNK", 4) + b"\1" * 4
    data = struct.pack("<4sI", b"data", 24) + b"\0" * 24
    path = TEST_TMP / "chunk.wav"
    path.write_bytes(
        struct.pack("<4sI4s", b"RIFF", 56, b"WAVE") + fmt + junk + data)
    with WavReader(path) as reader:
        assert reader.info.total_frames == 12


def test_truncated_data_chunk_is_clamped(tmp_path):
    fmt = struct.pack("<4sIHHIIHH", b"fmt ", 16, 1, 1, 48000, 96000, 2, 16)
    path = TEST_TMP / "trunc.wav"
    path.write_bytes(
        struct.pack("<4sI4s", b"RIFF", 40, b"WAVE") + fmt
        + struct.pack("<4sI", b"data", 999999) + b"\0" * 48)
    with pytest.warns(UserWarning):
        with WavReader(path) as reader:
            assert reader.info.truncated
            assert reader.info.total_frames == 24


def test_store_upsert_is_idempotent_and_supersedes(fixtures):
    out, _ = fixtures
    db = out.parent / "upsert.sqlite3"
    if db.exists():
        db.unlink()
    params = Params()
    with Store(db) as store:
        wav = next(out.glob("*.wav"))
        with WavReader(wav) as reader:
            segments, _ = run_pipeline(reader, params)
            file_id, inserted = store.upsert_analysis(
                wav, reader.info.sha256, params, reader.info, segments)
        assert inserted is True
        with WavReader(wav) as reader:
            same_id, inserted_again = store.upsert_analysis(
                wav, reader.info.sha256, params, reader.info, segments)
        assert inserted_again is False
        assert same_id == file_id
        changed = Params(min_silence_ms=200.0)
        with WavReader(wav) as reader:
            new_segments, _ = run_pipeline(reader, changed)
            new_id, inserted_new = store.upsert_analysis(
                wav, reader.info.sha256, changed, reader.info, new_segments)
        assert inserted_new is True and new_id != file_id
    rows = sqlite3.connect(db).execute(
        "SELECT status, count(*) FROM files GROUP BY status").fetchall()
    statuses = dict(rows)
    assert statuses.get("superseded") == 1
    assert statuses.get("active") == 1


def test_query_empty_raises_query_empty(fixtures):
    out, _ = fixtures
    db = out.parent / "query.sqlite3"
    params = Params()
    with Store(db) as store:
        for wav in sorted(out.glob("*.wav")):
            with WavReader(wav) as reader:
                segments, _ = run_pipeline(reader, params)
                store.upsert_analysis(wav, reader.info.sha256, params,
                                      reader.info, segments)
        assert store.query_segments(min_silence_ms=10**9) == []
    assert QueryEmpty.exit_code == 1



def test_steady_and_step_noise_produce_no_speech():
    rate = 48000
    rng = np.random.default_rng(11)
    duration = 30 * rate
    steady = (10 ** (-70 / 20)) * rng.standard_normal(duration)
    step = np.concatenate([
        (10 ** (-70 / 20)) * rng.standard_normal(duration // 2),
        (10 ** (-60 / 20)) * rng.standard_normal(duration - duration // 2),
    ])
    params = Params()
    for name, signal in (("steady", steady), ("step", step)):
        wav = TEST_TMP / f"{name}_noise.wav"
        write_pcm_wav(wav, signal, rate, 16)
        with WavReader(wav) as reader:
            segments, _ = run_pipeline(reader, params)
        assert segments == [], f"{name} noise must not yield speech segments"

def test_param_validation():
    with pytest.raises(ParamError):
        Params(hop_ms=50, frame_ms=20).validate()


def test_export_reuses_pipeline_and_writes_manifest(fixtures):
    out, _ = fixtures
    work = out.parent
    db = work / "export.sqlite3"
    if db.exists():
        db.unlink()
    export_dir = work / "exports"
    if export_dir.exists():
        for child in export_dir.iterdir():
            child.unlink()
    params = Params()
    total = 0
    with Store(db) as store:
        for wav in sorted(out.glob("*.wav")):
            with WavReader(wav) as reader:
                segments, _ = run_pipeline(reader, params)
                store.upsert_analysis(wav, reader.info.sha256, params,
                                      reader.info, segments)
                total += len(segments)
    proc = _invoke("export", "--db", str(db), "--out", str(export_dir),
                   "--pre-roll-ms", "40", "--post-roll-ms", "120")
    assert proc.returncode == 0
    wavs = list(export_dir.glob("*.wav"))
    assert len(wavs) == total
    manifest = [json.loads(line) for line in
                (export_dir / "manifest.jsonl").read_text().splitlines()]
    assert len(manifest) == total
    target = next(row for row in manifest if row["clip_start_sample"] > 0)
    clip_path = export_dir / target["file"]
    with WavReader(clip_path) as reader:
        assert reader.info.channels == 1 and reader.info.bits_per_sample == 16
        expected = target["clip_end_sample"] - target["clip_start_sample"]
        assert reader.info.total_frames == expected
    assert all(row["clip_start_sample"] >= 0 for row in manifest)


def test_cli_check_matches_truth(fixtures):
    out, _ = fixtures
    work = out.parent
    db = work / "cli_check.sqlite3"
    proc = _invoke("analyze", *map(str, sorted(out.glob("*.wav"))),
                   "--db", str(db))
    assert proc.returncode == 0
    check = _invoke("check", "--db", str(db), "--fixtures", str(out), "--json")
    assert check.returncode == 0
    payload = json.loads(check.stdout)
    assert payload["matched"] == payload["total"]
    assert payload["max_boundary_error_samples"] <= 480
    assert payload["silence_delta_samples"] <= 960
