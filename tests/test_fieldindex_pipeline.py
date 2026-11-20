"""端到端测试：夹具生成、流水线、索引续建、SHA-256 复核与 CLI 退出码。"""
from __future__ import annotations

import json
from fractions import Fraction
from pathlib import Path

import pytest

from fieldindex.cli import EXIT_EVIDENCE, EXIT_OK, EXIT_USAGE, main as cli_main
from fieldindex.fixtures import build_fixtures
from fieldindex.index import IndexError_, SegmentIndex, canonical_digest
from fieldindex.media import probe_media
from fieldindex.model import SourceRef
from fieldindex.pipeline import Pipeline, PipelineConfig
from fieldindex.repair import RepairConfig


@pytest.fixture(scope="module")
def bundle(tmp_path_factory):
    workdir = tmp_path_factory.mktemp("fx")
    return build_fixtures(
        workdir, seed=42, frames=1200, gop=100,
        corrupt_tail=True, partial_index=True,
    )


def _run(bundle, index_path: Path, resume: bool = False):
    sub = bundle.subtitles.read_bytes()
    tc = bundle.timecodes.read_bytes()
    media = probe_media(bundle.media)
    cfg = PipelineConfig(
        frame_rate=bundle.rate, gop=bundle.gop,
        drift_limit=Fraction(1, 4),
        subtitle_source=bundle.subtitles.name,
        timecode_source=bundle.timecodes.name,
    )
    blobs = {bundle.subtitles.name: sub, bundle.timecodes.name: tc}
    return Pipeline(
        index_path, cfg, RepairConfig(drift_limit=cfg.drift_limit)
    ).build(sub, tc, blobs, media, resume=resume)


def test_end_to_end_acceptance_metrics(bundle):
    result = _run(bundle, bundle.workdir / "a.jsonl")
    assert len(result.segments) == bundle.segment_count == 12
    assert float(result.roundtrip_rate) >= 0.99
    assert float(result.keyframe_hit_rate) == 1.0
    assert result.verified == (12, 12)
    assert result.recovered >= 2 and len(result.errors) >= 2


def test_rerun_produces_identical_boundaries_and_hashes(bundle):
    first = _run(bundle, bundle.workdir / "b1.jsonl")
    second = _run(bundle, bundle.workdir / "b2.jsonl")
    h1 = [(s.segment_id, s.start, s.end, s.sha256) for s in first.segments]
    h2 = [(s.segment_id, s.start, s.end, s.sha256) for s in second.segments]
    assert h1 == h2


def test_partial_index_resume_rebuilds_only_tail(bundle):
    path = bundle.workdir / "resume.jsonl"
    first = _run(bundle, path)
    raw = path.read_text().splitlines(keepends=True)
    cut = next(i for i, line in enumerate(raw)
               if '"segment_id": 7' in line)
    path.write_text("".join(raw[:cut]))
    resumed = _run(bundle, path, resume=True)
    assert len(resumed.segments) == 12
    assert resumed.verified == (12, 12)
    assert [s.sha256 for s in resumed.segments] == \
           [s.sha256 for s in first.segments]


def test_corrupt_tail_line_does_not_poison_index(bundle):
    path = bundle.workdir / "tail.jsonl"
    _run(bundle, path)
    raw = path.read_text().splitlines(keepends=True)
    path.write_text("".join(raw[:-1]) + raw[-1][:25])
    reader = SegmentIndex(path)
    reusable = reader.load_existing()
    # 末段不完整 -> 不可复用，且未验证条目不可访问
    assert 11 not in reusable
    with pytest.raises(IndexError_):
        reader.random_access(11)


def test_version_mismatch_forces_full_rebuild(bundle):
    path = bundle.workdir / "ver.jsonl"
    _run(bundle, path)
    lines = path.read_text().splitlines()
    header = json.loads(lines[0])
    header["version"] = 999
    path.write_text(json.dumps(header) + "\n" + "\n".join(lines[1:]) + "\n")
    with pytest.raises(IndexError_):
        SegmentIndex(path).load_existing()
    # 流水线捕获版本异常并整体重建
    result = _run(bundle, path, resume=True)
    assert result.verified == (12, 12)


def test_tampered_source_fails_sha256_verification(bundle):
    path = bundle.workdir / "tamper.jsonl"
    _run(bundle, path)
    reader = SegmentIndex(path)
    reader.load_existing()
    bad_blobs = {
        bundle.subtitles.name: b"Z" + bundle.subtitles.read_bytes()[1:],
        bundle.timecodes.name: bundle.timecodes.read_bytes(),
    }
    ok, total, failed = reader.verify(bad_blobs)
    assert failed and ok < total
    with pytest.raises(IndexError_):
        reader.random_access(failed[0])


def test_canonical_digest_is_order_independent():
    blobs = {"a": b"hello world", "b": b"second blob"}
    refs = [SourceRef("a", 0, 5), SourceRef("b", 2, 7)]
    h1 = canonical_digest(blobs, refs, 3, Fraction(1), Fraction(2))
    h2 = canonical_digest(blobs, list(reversed(refs)), 3, Fraction(1), Fraction(2))
    assert h1 == h2 and len(h1) == 64


def test_cli_process_and_verify_exit_codes(bundle, capsys):
    base = [
        "--workdir", str(bundle.workdir),
        "--subtitles", bundle.subtitles.name,
        "--timecodes", bundle.timecodes.name,
        "--media", bundle.media.name,
        "--index", "cli_index.jsonl",
        "--rate", "24", "--gop", "100",
    ]
    code = cli_main(["process", *base])
    assert code == EXIT_OK
    out = capsys.readouterr().out
    assert "roundtrip_rate=0.99" in out and "sha256_verified=12/12" in out
    verify_code = cli_main([
        "verify", "--workdir", str(bundle.workdir),
        "--index", "cli_index.jsonl",
        "--inputs", bundle.subtitles.name, bundle.timecodes.name,
    ])
    assert verify_code == EXIT_OK
    # 路径逃逸 -> 退出码 2
    escape = cli_main([
        "verify", "--workdir", str(bundle.workdir),
        "--index", "../escape.jsonl",
        "--inputs", bundle.subtitles.name,
    ])
    assert escape == EXIT_USAGE
    # 证据阈值抬高 -> 退出码 1
    fail = cli_main(["process", *base, "--index", "strict.jsonl",
                     "--min-roundtrip", "0.9999"])
    assert fail == EXIT_EVIDENCE


def test_cli_missing_file_is_usage_error(bundle):
    code = cli_main([
        "process", "--workdir", str(bundle.workdir),
        "--subtitles", "missing.srt",
        "--timecodes", bundle.timecodes.name,
        "--media", bundle.media.name, "--index", "x.jsonl",
    ])
    assert code == EXIT_USAGE
