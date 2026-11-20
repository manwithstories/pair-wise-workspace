"""取消续跑与检查点恢复的端到端测试。"""

import dataclasses
import json
from pathlib import Path

import numpy as np

from mediasync.audio import write_audio
from mediasync.bitstream import Codec
from mediasync.checkpoint import canonical_json
from mediasync.digest import write_json
from mediasync.fixture import (
    RepresentationSpec,
    build_keyframe_index,
    encode_video,
    inject_corrupt_header,
)
from mediasync.pipeline import CancellationToken, JobConfig, Pipeline


def _make_workspace(root: Path, representations: int = 3, corrupt_every: int = 0):
    """生成完整工作区，返回损坏偏移（无损坏为 None）。"""
    corrupt_offset = None
    (root / "fixtures").mkdir(parents=True, exist_ok=True)
    for i in range(representations):
        codec = (Codec.H264, Codec.H265, Codec.AV1)[i % 3]
        spec = RepresentationSpec(name=f"rep{i}", codec=codec, frames=30, gop=15)
        data = encode_video(spec)
        entries = build_keyframe_index(data, codec, spec.timescale, spec.fps, spec.gop)
        if corrupt_every and i == 0:
            data, corrupt_offset = inject_corrupt_header(data, corrupt_every)
        (root / "fixtures" / f"rep{i}.{codec.value}").write_bytes(data)
        write_json(
            f"fixtures/rep{i}.index.json",
            root,
            [dataclasses.asdict(e) for e in entries],
        )
        write_json(
            f"fixtures/rep{i}.color.json",
            root,
            {"range": "limited", "matrix": "bt709",
             "transfer": "bt709", "primaries": "bt709"},
        )
    rng = np.random.RandomState(11)
    write_audio("fixtures/audio.wav", root, rng.randn(24000, 2), 24000)
    return corrupt_offset


def test_cancel_then_resume_reuses_confirmed_blocks(tmp_path):
    _make_workspace(tmp_path)
    cfg = JobConfig(workdir=str(tmp_path), block_records=1, interrupt_after=1)

    first = Pipeline(cfg).run(CancellationToken())
    assert first.cancelled is True
    store = Path(tmp_path) / "checkpoint" / "blocks"
    confirmed = list(store.glob("*.json"))
    tmp_left = list(store.glob("*.tmp"))
    assert len(confirmed) == 1 and tmp_left  # 中断留下未确认临时块

    # 续跑：新配置关闭中断注入，从已确认块之后继续。
    resume_cfg = JobConfig(workdir=str(tmp_path), block_records=1, resume=True)
    second = Pipeline(resume_cfg).run(CancellationToken())
    assert second.exit_code == 0
    assert second.segments >= 2
    assert second.aligned_representations == "3/3"
    assert not list(store.glob("*.tmp"))
    # 恢复耗时远低于素材时长目标（1000ms 素材，断言 < 500ms）。
    assert second.resume_ms < 500


def test_repeated_runs_produce_identical_manifest_and_index(tmp_path):
    _make_workspace(tmp_path)
    cfg = JobConfig(workdir=str(tmp_path), block_records=2)
    first = Pipeline(cfg).run()

    # 清空产物但保留输入，重跑应得到完全一致的摘要。
    for rel in ("manifest.json", "index.json"):
        (tmp_path / rel).unlink()
    second = Pipeline(JobConfig(workdir=str(tmp_path), block_records=2)).run()
    assert first.manifest_sha256 == second.manifest_sha256
    assert first.index_sha256 == second.index_sha256
    assert len(first.manifest_sha256) == 64


def test_corrupt_input_reported_with_byte_precision(tmp_path):
    offset = _make_workspace(tmp_path, corrupt_every=7)
    cfg = JobConfig(workdir=str(tmp_path), corrupt_offset=offset)
    result = Pipeline(cfg).run()

    assert result.exit_code == 3
    assert result.corrupt_offset_bytes == offset
    assert result.errors.any_fatal()


def test_checkpoint_summary_mismatch_quarantines_and_restarts(tmp_path):
    _make_workspace(tmp_path)
    cfg = JobConfig(workdir=str(tmp_path), block_records=1)
    Pipeline(cfg).run(CancellationToken())

    # 篡改已确认块模拟块边界损坏。
    block = next((tmp_path / "checkpoint" / "blocks").glob("*.json"))
    data = json.loads(block.read_bytes())
    data["records"].append({"tampered": True})
    block.write_bytes(canonical_json(data))

    resume = Pipeline(JobConfig(workdir=str(tmp_path), block_records=1, resume=True)).run()
    assert resume.exit_code == 0  # 隔离后从安全边界重算成功
    quarantine = tmp_path / "quarantine"
    assert quarantine.exists() and any(quarantine.iterdir())
    assert resume.manifest_sha256
