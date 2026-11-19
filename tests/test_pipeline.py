"""端到端与单元测试：成功、损坏、边界样例。

夹具由 fixtures.generate_demo 在会话内生成，不引用外部文件。
"""
from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

from fielddeliver import audio as audio_mod
from fielddeliver import index as index_mod
from fielddeliver import probe as probe_mod
from fielddeliver import report as report_mod
from fielddeliver.color import arbitrate_all
from fielddeliver.fixtures import generate_demo
from fielddeliver.model import (ColorDecl, ColorStrategy, DEFAULT_LIMITS,
                                ErrorCategory, ExitCode, Manifest,
                                PaddingPolicy, PipelineError, TrackDecl,
                                TrackKind, VideoCodec)
from fielddeliver.pipeline import Pipeline
from fielddeliver.probe import normalize_timestamps, RawPacket
from fielddeliver.scheduler import Scheduler
from fielddeliver.timeline import topological_order


# ------------------------------------------------------------ 会话夹具

@pytest.fixture(scope="session")
def demo_out(tmp_path_factory):
    """只生成一次媒体文件，返回 demo 目录。"""
    out = tmp_path_factory.mktemp("demo")
    generate_demo(str(out), seed=37)
    return out


@pytest.fixture()
def demo_dir(demo_out):
    """每个测试从磁盘重载独立 manifest，避免跨测试原地修改污染。"""
    from fielddeliver.cli import load_manifest
    manifest = load_manifest(str(Path(demo_out) / "manifest.json"))
    return demo_out, manifest


# ------------------------------------------------------------ 清单校验

def test_demo_generates_multi_camera(demo_dir):
    out, manifest = demo_dir
    assert (Path(out) / "manifest.json").is_file()
    kinds = [t.kind for t in manifest.tracks.values()]
    assert kinds.count(TrackKind.VIDEO) == 3
    assert kinds.count(TrackKind.AUDIO) == 3
    codecs = {t.codec for t in manifest.tracks.values()
              if t.kind is TrackKind.VIDEO}
    assert VideoCodec.H264 in codecs and VideoCodec.H265 in codecs
    # 损坏样例确实带丢包/截断标注
    cam_c = manifest.tracks["cam_c"]
    assert any(not e.get("recoverable", True) for e in cam_c.known_losses)


def test_missing_required_field_exits_2(demo_dir):
    _, manifest = demo_dir
    manifest.tracks["cam_a"].frame_rate = None
    with pytest.raises(PipelineError) as ei:
        probe_mod.validate_manifest(manifest)
    assert ei.value.exit_code is ExitCode.INVALID_INPUT
    assert ei.value.track == "cam_a"


def test_path_outside_root_rejected(tmp_path):
    (tmp_path / "x.h264").write_bytes(b"")
    m = Manifest(project="p", root=str(tmp_path), tracks={
        "v": TrackDecl(track_id="v", kind=TrackKind.VIDEO,
                       path="../escape.h264", codec=VideoCodec.H264,
                       frame_rate=30)},
        color_default=ColorDecl(), limits=dict(DEFAULT_LIMITS),
        reference_video=None, global_frame_rate=30)
    with pytest.raises(PipelineError) as ei:
        probe_mod.validate_manifest(m)
    assert ei.value.category is ErrorCategory.PATH_OUTSIDE_ROOT


def test_dependency_cycle_detected(demo_dir):
    _, manifest = demo_dir
    manifest.tracks["cam_a"].depends_on = ["cam_b"]
    manifest.tracks["cam_b"].depends_on = ["cam_a"]
    with pytest.raises(PipelineError) as ei:
        topological_order(manifest)
    assert ei.value.category is ErrorCategory.DEPENDENCY_CYCLE


# ------------------------------------------------------------ 时间戳修复

def _track(wrap=None):
    return TrackDecl(track_id="c", kind=TrackKind.VIDEO, path="x",
                     codec=VideoCodec.H264, frame_rate=30.0,
                     pts_wrap_modulus=wrap)


def test_timestamp_wrap_single_report():
    tr = _track(wrap=64)
    raw = list(range(32)) + list(range(0, 28))
    pkts = [RawPacket(i * 100, 50, i % 10 == 0, r)
            for i, r in enumerate(raw)]
    frames, issues = normalize_timestamps(tr, pkts)
    assert [i.category for i in issues] == [
        ErrorCategory.TIMESTAMP_REPAIRED]
    assert frames[-1].raw_pts == 59
    assert all(frames[i].raw_pts < frames[i + 1].raw_pts
               for i in range(len(frames) - 1))


def test_timestamp_duplicate_smallloss_biggap():
    tr = _track()
    pkts = [RawPacket(i * 10, 10, i % 5 == 0, r)
            for i, r in enumerate([0, 1, 2, 2, 4, 50, 51])]
    frames, issues = normalize_timestamps(tr, pkts)
    cats = {i.category for i in issues}
    assert ErrorCategory.DUPLICATE_FRAME in cats
    assert ErrorCategory.PACKET_LOSS_RECOVERED in cats
    assert ErrorCategory.TIMESTAMP_GAP in cats
    # 大缺口保留显式 gap 记录，不伪造帧
    assert any(f.is_gap for f in frames)


def test_annexb_no_pts_sequential():
    tr = _track()
    pkts = [RawPacket(i * 10, 10, False, None) for i in range(5)]
    frames, issues = normalize_timestamps(tr, pkts)
    assert [f.raw_pts for f in frames] == [0, 1, 2, 3, 4]
    assert issues == []


# ------------------------------------------------------------ 视频探测与索引

def test_probe_three_cameras(demo_dir):
    out, manifest = demo_dir
    for tid in ("cam_a", "cam_b", "cam_c"):
        t = manifest.tracks[tid]
        result = probe_mod.probe_video(t, str(out))
        assert result.video.frame_count > 0
        assert result.video.keyframe_count >= 1
        assert result.video.width > 0 and result.video.height > 0


def test_cam_c_unrecoverable_detected(demo_dir):
    out, manifest = demo_dir
    t = manifest.tracks["cam_c"]
    result = probe_mod.probe_video(t, str(out))
    cats = {i.category for i in result.issues}
    assert (ErrorCategory.TRUNCATED_STREAM in cats
            or ErrorCategory.DECODE_FAILURE in cats)
    assert result.truncated is True


def test_random_seek_index_pass(demo_dir):
    out, manifest = demo_dir
    t = manifest.tracks["cam_a"]
    pr = probe_mod.probe_video(t, str(out))
    idx = index_mod.build_index(t, pr.frames, Path(t.path).stat().st_size)
    index_mod.verify_seek_points(t, idx, t.path, sample=6)
    assert index_mod.seek_pass_rate(idx) >= 0.99
    # 按时间定位返回其之前最近关键帧
    e = idx.entry_before(pr.frames[min(11, len(pr.frames) - 1)].pts_us)
    assert e is not None and e.pts_us >= 0


def test_legacy_index_out_of_range_invalidates(demo_dir):
    out, manifest = demo_dir
    t = manifest.tracks["cam_a"]
    pr = probe_mod.probe_video(t, str(out))
    idx = index_mod.build_index(t, pr.frames, Path(t.path).stat().st_size)
    bad = {"kind": index_mod.INDEX_KIND, "track_id": t.track_id,
           "file_size": idx.file_size,
           "entries": [{"index_no": 0, "byte_offset": 9_999_999_999,
                        "frame_index": 0, "pts_us": 0, "raw_pts": 0}]}
    legacy = Path(out) / "bad.index.json"
    legacy.write_text(json.dumps(bad))
    issues = index_mod.validate_legacy_index(t, idx, str(legacy))
    assert idx.stale is True
    assert all(i.category is ErrorCategory.INDEX_STALE for i in issues)


# ------------------------------------------------------------ 音频

def test_audio_resample_and_downmix(demo_dir):
    out, manifest = demo_dir
    t = manifest.tracks["aud_lav"]  # 44.1k mono -> 16k mono
    r = audio_mod.process_audio(t, str(out))
    assert r.info.target_rate == 16000
    assert r.samples.shape[1] == 1
    expected = round(2.0 * 16000)
    assert abs(r.info.output_frames - expected) <= 1


def test_empty_audio_skeleton(demo_dir):
    out, manifest = demo_dir
    t = manifest.tracks["aud_empty"]
    r = audio_mod.process_audio(t, str(out), reference_duration_us=2_000_000)
    cats = {i.category for i in r.issues}
    assert ErrorCategory.EMPTY_AUDIO in cats
    assert r.info.output_frames == 0          # 不按参考时长伪造
    assert r.info.padded_samples == 0


def test_audio_reject_policy(tmp_path):
    sf.write(str(tmp_path / "a.wav"),
             np.zeros((100, 1), np.float32), 48000)
    t = TrackDecl(track_id="a", kind=TrackKind.AUDIO, path="a.wav",
                  sample_rate=48000, channels=1,
                  padding_policy=PaddingPolicy.REJECT)
    with pytest.raises(PipelineError) as ei:
        audio_mod.process_audio(t, str(tmp_path),
                                reference_duration_us=1_000_000)
    assert ei.value.exit_code is ExitCode.INVALID_INPUT


# ------------------------------------------------------------ 色彩裁决

def test_color_arbitration_strategies(demo_dir):
    _, manifest = demo_dir
    verdicts = arbitrate_all(manifest)
    # cam_b 用 inherit 且与参考冲突 -> 采用继承源并记录冲突
    vb = verdicts["cam_b"]
    assert vb.merged()["primaries"] == "bt709"
    assert len(vb.conflicts) >= 1


def test_color_error_strategy_raises(demo_dir):
    _, manifest = demo_dir
    manifest.tracks["cam_b"].color.strategy = ColorStrategy.ERROR
    with pytest.raises(PipelineError) as ei:
        arbitrate_all(manifest)
    assert ei.value.exit_code is ExitCode.INVALID_INPUT


# ------------------------------------------------------------ 流水线端到端

def test_pipeline_exit_3_and_metrics(demo_dir):
    out, manifest = demo_dir
    work = Path(out) / "work"
    report, code = Pipeline(manifest, str(work)).run()
    assert code is ExitCode.UNRECOVERABLE          # 含 cam_c 不可恢复
    m = report["metrics"]
    # 成功样例目标
    assert m["healthy_recovery_rate"] >= 0.90
    assert m["random_seek_pass_rate"] >= 0.99
    assert m["sync_error_ms"] <= 20.0
    # P95 延迟不高于素材时长的 0.5 倍
    assert m["p95_latency_ms"] <= m["duration_us"] / 1000.0 * 0.5
    # 损坏样例报告实际恢复率与不可恢复数
    assert m["damaged_unrecovered"] >= 1
    assert 0.0 <= m["damaged_recovery_rate"] < 1.0
    # 六项验收指标齐备
    for key in ("sync_error_ms", "packet_recovery_rate", "p95_latency_ms",
                "random_seek_pass_rate", "audio_sample_delta",
                "color_conflicts"):
        assert key in m


def test_pipeline_writes_outputs(demo_dir):
    out, manifest = demo_dir
    work = Path(out) / "work2"
    Pipeline(manifest, str(work)).run()
    assert (work / "report.json").is_file()
    for tid in ("cam_a", "cam_b", "cam_c"):
        assert (work / "index" / f"{tid}.index.json").is_file()
    for tid in ("aud_master", "aud_lav"):
        assert (work / "audio" / f"{tid}.wav").is_file()


def test_checkpoint_resume_and_invalidation(demo_dir):
    out, manifest = demo_dir
    work = Path(out) / "work3"
    p1 = Pipeline(manifest, str(work)).run()
    cp_dir = work / "checkpoints"
    cps = list(cp_dir.glob("*.json"))
    assert cps, "首次运行应写出检查点"
    # 续跑：指纹一致，检查点命中（不抛错且报告可重建）
    p2_report, _ = Pipeline(manifest, str(work)).run()
    assert p2_report["metrics"]["track_count"] == 6
    # 参数变化：改清单源哈希 -> 检查点指纹失配，应仍能正确重跑
    manifest.source_hash = "changed-for-test"
    p3_report, code3 = Pipeline(manifest, str(work)).run()
    assert code3 is ExitCode.UNRECOVERABLE
    assert p3_report["metrics"]["track_count"] == 6


def test_upstream_failure_blocks_downstream(demo_dir, monkeypatch):
    out, manifest = demo_dir
    # 让 cam_b 在处理阶段失败，验证其下游 aud_empty 被跳过而非提交。
    def boom(track, root):
        if track.track_id == "cam_b":
            raise PipelineError(ErrorCategory.TRUNCATED_STREAM,
                                "注入的处理阶段失败", track="cam_b")
        return real_probe(track, root)

    real_probe = probe_mod.probe_video
    monkeypatch.setattr(probe_mod, "probe_video", boom)
    work = Path(out) / "work4"
    _, code = Pipeline(manifest, str(work)).run()
    report = json.loads((work / "report.json").read_text())
    states = {t: v["state"] for t, v in report["tracks"].items()}
    assert states["cam_b"] == "failed"
    assert states["aud_empty"] == "skipped"
    assert code is ExitCode.UNRECOVERABLE


# ------------------------------------------------------------ CLI

def test_cli_demo_and_verify(tmp_path):
    from fielddeliver.cli import main
    out = tmp_path / "demo_cli"
    rc = main(["demo", "--seed", "7", "--out", str(out)])
    assert rc == 0
    rc = main(["verify", "--manifest", str(out / "manifest.json"),
               "--work-dir", str(tmp_path / "cliwork")])
    assert rc == 3                     # 含不可恢复损坏样例


def test_cli_invalid_manifest_exit_2(tmp_path):
    from fielddeliver.cli import main
    bad = tmp_path / "bad.json"
    bad.write_text("{not valid json")
    rc = main(["verify", "--manifest", str(bad)])
    assert rc == 2


def test_cli_missing_file_exit_2(tmp_path):
    from fielddeliver.cli import main
    rc = main(["verify", "--manifest", str(tmp_path / "nope.json")])
    assert rc == 2


def test_internal_error_exit_1(demo_dir, monkeypatch):
    out, manifest = demo_dir
    def boom(track, root):
        raise RuntimeError("模拟未预期内部异常")
    monkeypatch.setattr(probe_mod, "probe_video", boom)
    work = Path(out) / "work_internal"
    _, code = Pipeline(manifest, str(work)).run()
    assert code is ExitCode.INTERNAL
