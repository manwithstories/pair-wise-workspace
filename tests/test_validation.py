"""清单校验、损坏分类、合成确定性与边界窗口单测。"""

import json
from pathlib import Path

import numpy as np
import pytest

from relaymix.errors import ManifestError, MediaError
from relaymix.frame_export import iter_windows
from relaymix.manifest import build_manifest
from relaymix.pipeline import run_job
from relaymix.timeline import resample_linear
from relaymix.timebase import TimeBase, quantize_int16
from relaymix.verify import verify_root
from relaymix.wav_reader import read_block

import make_fixtures as fx


@pytest.fixture()
def silence_job(tmp_path: Path) -> Path:
    return fx.build_short_data(tmp_path / "silence", policy="silence")


# ---------------------------------------------------------------- 清单校验

def test_manifest_rejects_negative_and_zero(tmp_path: Path) -> None:
    base = fx.build_normal(tmp_path / "m")
    data = json.loads(base.read_text())
    data["output_end"] = 0
    with pytest.raises(ManifestError) as ei:
        build_manifest(data)
    assert ei.value.error_code == "negative_or_zero_length"


def test_manifest_rejects_zero_rate_and_unknown_track(tmp_path: Path) -> None:
    base = fx.build_normal(tmp_path / "m")
    data = json.loads(base.read_text())
    data["master_rate"] = 0
    with pytest.raises(ManifestError, match="master_rate"):
        build_manifest(data)

    data = json.loads(base.read_text())
    data["overlaps"][0]["track_a"] = "ghost"
    with pytest.raises(ManifestError) as ei:
        build_manifest(data)
    assert ei.value.error_code == "unknown_track"


def test_manifest_rejects_non_monotonic_and_out_of_range(tmp_path: Path) -> None:
    base = fx.build_normal(tmp_path / "m")
    data = json.loads(base.read_text())
    data["tracks"][0]["blocks"][1]["start_tick"] = 0
    with pytest.raises(ManifestError) as ei:
        build_manifest(data)
    assert ei.value.error_code == "non_monotonic_blocks"

    data = json.loads(base.read_text())
    data["tracks"][0]["blocks"][0]["length_ticks"] = 999999
    with pytest.raises(ManifestError) as ei:
        build_manifest(data)
    assert ei.value.error_code == "range_out_of_bounds"


def test_manifest_rejects_bad_channel_map(tmp_path: Path) -> None:
    base = fx.build_normal(tmp_path / "m")
    data = json.loads(base.read_text())
    data["tracks"][0]["channel_map"] = [0, 9]
    with pytest.raises(ManifestError) as ei:
        build_manifest(data)
    assert ei.value.error_code == "bad_channel_map"


# ---------------------------------------------------------------- 损坏分类

def test_truncated_header_classification(tmp_path: Path) -> None:
    fx.build_truncated_wav(tmp_path / "dmg")
    block = tmp_path / "dmg" / fx.WAV_DIR / "a1.wav"
    with pytest.raises(MediaError) as ei:
        read_block(block, "A1")
    err = ei.value.to_dict()
    assert err["error_code"] == "truncated_wav"
    assert err["source_block"] == "A1"
    assert isinstance(err["byte_offset"], int)


def test_short_data_classification(tmp_path: Path) -> None:
    fx.build_short_data(tmp_path / "dmg")
    block = tmp_path / "dmg" / fx.WAV_DIR / "b0.wav"
    with pytest.raises(MediaError) as ei:
        read_block(block, "B0")
    err = ei.value.to_dict()
    assert err["error_code"] == "data_length_mismatch"
    assert err["actual_frames"] < err["declared_frames"]


def test_illegal_channels_and_empty_block(tmp_path: Path) -> None:
    fx.build_illegal_channels(tmp_path / "illegal")
    with pytest.raises(MediaError) as ei:
        read_block(tmp_path / "illegal" / fx.WAV_DIR / "a0.wav", "A0")
    assert ei.value.to_dict()["error_code"] == "illegal_channels"

    fx.build_empty_block(tmp_path / "empty")
    with pytest.raises(MediaError) as ei:
        read_block(tmp_path / "empty" / fx.WAV_DIR / "a0.wav", "A0")
    assert ei.value.to_dict()["error_code"] == "empty_block"


def test_abort_policy_exits_three(tmp_path: Path) -> None:
    manifest = fx.build_truncated_wav(tmp_path / "job")
    with pytest.raises(MediaError) as ei:
        run_job(tmp_path / "root", manifest)
    assert ei.value.exit_code == 3
    assert ei.value.to_dict()["source_block"] == "A1"


def test_silence_policy_fills_and_marks(silence_job: Path, tmp_path: Path) -> None:
    summary = run_job(tmp_path / "silence_root", silence_job, is_resume=False)
    assert summary["status"] == "ok"
    filled = summary["filled_blocks"]
    assert len(filled) == 1 and filled[0]["source_block"] == "B0"
    result = verify_root(tmp_path / "silence_root")
    assert result["status"] == "ok"
    assert result["roundtrip_rate"] == 1.0


# ---------------------------------------------------------------- 去重/合成

def test_duplicate_delivery_is_idempotent(tmp_path: Path) -> None:
    manifest = fx.build_normal(tmp_path / "normal")
    summary = run_job(tmp_path / "root", manifest)
    assert summary["deduped_blocks"] == ["A1"]
    # 重放整作业：所有窗口已提交，committed_now 必须为空。
    again = run_job(tmp_path / "root", manifest)
    assert again["committed_now"] == []
    assert again["resumed_chunks"] == summary["total_chunks"]


def test_same_block_id_different_content_stops_track(tmp_path: Path) -> None:
    manifest_path = fx.build_normal(tmp_path / "src", duplicate=False)
    data = json.loads(manifest_path.read_text())
    # 同 block_id A1 指向内容不同的 a0.wav，且起点递增以通过清单校验。
    clone = dict(data["tracks"][0]["blocks"][1])
    clone["path"] = f"{fx.WAV_DIR}/a0.wav"
    clone["start_tick"] = 9000
    data["tracks"][0]["blocks"].append(clone)
    manifest_path.write_text(json.dumps(data))
    summary = run_job(tmp_path / "root", manifest_path)
    codes = [e["error_code"] for e in summary["track_errors"]]
    assert "block_content_conflict" in codes


def test_resample_and_quantize_are_deterministic() -> None:
    rng = np.random.default_rng(0)
    x = rng.standard_normal((100, 2))
    a = resample_linear(x, 250)
    b = resample_linear(x, 250)
    assert np.array_equal(a, b)
    q1 = quantize_int16(a)
    q2 = quantize_int16(a)
    assert np.array_equal(q1, q2) and q1.dtype == np.int16
    # round-half-even：+0.5 落到偶数。
    tb = TimeBase(48000)
    assert tb.to_source_frames(1, 24000) == 0  # 0.5 -> 0
    assert tb.to_source_frames(3, 24000) == 2  # 1.5 -> 2


# ---------------------------------------------------------------- 边界/窗口

def test_boundary_incomplete_window(tmp_path: Path) -> None:
    manifest = fx.build_boundary(tmp_path / "edge")
    run_job(tmp_path / "root", manifest)
    result = verify_root(tmp_path / "root")
    tail = result["incomplete_tail_window"]
    assert tail == {"chunk_index": 2, "frames": 200, "frame_size": 2400}
    assert result["segments"] == 3
    assert result["keyframe_index_hit_rate"] == 1.0


def test_window_iteration_covers_full_range() -> None:
    wins = list(iter_windows(0, 5000, 2400))
    assert [(w.start_tick, w.end_tick) for w in wins] == [
        (0, 2400), (2400, 4800), (4800, 5000)
    ]
    assert wins[-1].end_tick - wins[-1].start_tick == 200
