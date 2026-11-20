"""多表示层对齐的端到端测试：3/3 对齐、偏差拒绝与未对齐原因。"""

import dataclasses
from fractions import Fraction
from pathlib import Path

import numpy as np

from mediasync.audio import write_audio
from mediasync.bitstream import Codec
from mediasync.digest import write_json
from mediasync.errors import ErrorBook, ErrorCode
from mediasync.fixture import (
    RepresentationSpec,
    build_keyframe_index,
    encode_video,
)
from mediasync.pipeline import JobConfig, Pipeline
from mediasync.segment import Representation, plan_cuts


def _make_workspace(root: Path, representations: int = 3):
    (root / "fixtures").mkdir(parents=True, exist_ok=True)
    for i in range(representations):
        codec = (Codec.H264, Codec.H265, Codec.AV1)[i % 3]
        spec = RepresentationSpec(name=f"rep{i}", codec=codec, frames=30, gop=15)
        data = encode_video(spec)
        entries = build_keyframe_index(data, codec, spec.timescale, spec.fps, spec.gop)
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
    write_audio("fixtures/audio.wav", root, np.random.RandomState(3).randn(24000, 2), 24000)


def test_three_representations_fully_aligned(tmp_path):
    _make_workspace(tmp_path)
    result = Pipeline(
        JobConfig(workdir=str(tmp_path), segment_ms=500, max_deviation_ms=25)
    ).run()

    assert result.exit_code == 0
    assert result.aligned_representations == "3/3"
    assert result.segments >= 2
    manifest = (tmp_path / "manifest.json").read_text()
    assert "fully_aligned" in manifest and "true" in manifest


def test_plan_rejects_when_representation_lacks_aligned_keyframe():
    book = ErrorBook()
    # rep2 的关键帧偏离 500ms 网格 200ms，超出 25ms 容差。
    reps = [
        Representation("rep0", [Fraction(0), Fraction(500), Fraction(1000)]),
        Representation("rep1", [Fraction(0), Fraction(500), Fraction(1000)]),
        Representation("rep2", [Fraction(0), Fraction(700), Fraction(1000)]),
    ]
    plan = plan_cuts(reps, segment_ms=500, max_deviation_ms=25, errors=book)
    assert not plan.fully_aligned
    assert book.has(ErrorCode.ALIGNMENT_IMPOSSIBLE)
    assert "rep2" in plan.rejected[0].reason


def test_pipeline_exit_code_three_on_misalignment(tmp_path):
    import json

    _make_workspace(tmp_path)
    # 把 rep1 第二项索引偏移 +1，使其不再指向关键帧，扫描阶段判定输入损坏。
    index_path = tmp_path / "fixtures" / "rep1.index.json"
    entries = json.loads(index_path.read_text())
    entries[1] = {"offset": entries[1]["offset"] + 1, "tick": entries[1]["tick"]}
    index_path.write_text(json.dumps(entries, sort_keys=True))

    result = Pipeline(
        JobConfig(workdir=str(tmp_path), segment_ms=500, max_deviation_ms=25)
    ).run()
    assert result.exit_code == 3
    assert result.errors.any_fatal()


def test_segment_boundaries_are_rational_and_contiguous(tmp_path):
    _make_workspace(tmp_path)
    result = Pipeline(JobConfig(workdir=str(tmp_path))).run()
    assert result.exit_code == 0
    import json

    manifest = json.loads((tmp_path / "manifest.json").read_text())
    segments = manifest["segments"]
    for prev, nxt in zip(segments, segments[1:]):
        assert prev["end_ms"] == nxt["start_ms"]
        assert set(prev["cuts"]) == {"rep0", "rep1", "rep2"}
