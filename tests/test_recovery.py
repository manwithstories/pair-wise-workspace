"""检查点中断、恢复指纹校验、只读核验与 CLI 退出码测试。

中断场景（os._exit）一律在独立子进程中演示，避免终止 pytest 工作进程；
子进程使用最小化环境变量，防止父进程残留的崩溃注入泄漏。
"""

import json
import os
import sqlite3
import subprocess
import sys
from pathlib import Path

import pytest

from relaymix.checkpoint import DB_NAME, CheckpointStore, ChunkRecord
from relaymix.errors import CheckpointError
from relaymix.frame_export import segment_name
from relaymix.pipeline import run_job
from relaymix.verify import verify_root

import make_fixtures as fx

REPO_ROOT = Path(__file__).resolve().parents[1]


@pytest.fixture()
def normal_job(tmp_path: Path) -> Path:
    return fx.build_normal(tmp_path / "normal")


def _clean_env(**extra: str) -> dict[str, str]:
    env = {"PATH": os.environ.get("PATH", ""),
           "PYTHONPATH": str(REPO_ROOT / "src")}
    env.update(extra)
    return env


def _run_crashed(root: Path, manifest: Path, **extra: str) -> subprocess.CompletedProcess:
    code = (
        "from relaymix.pipeline import run_job; "
        f"run_job({str(root)!r}, {str(manifest)!r})"
    )
    return subprocess.run(
        [sys.executable, "-c", code], env=_clean_env(**extra), cwd=REPO_ROOT,
        capture_output=True, text=True,
    )


# ---------------------------------------------------------------- 检查点中断

def test_crash_after_commits_leaves_consistent_index(
    normal_job: Path, tmp_path: Path
) -> None:
    root = tmp_path / "root"
    proc = _run_crashed(root, normal_job, RELAYMIX_CRASH_AFTER="2")
    assert proc.returncode == 130
    store = CheckpointStore(root)
    assert store.committed_indices() == {0, 1}
    store.close()


def test_crash_during_write_leaves_no_committed_index(
    normal_job: Path, tmp_path: Path
) -> None:
    root2 = tmp_path / "root2"
    proc = _run_crashed(
        root2, normal_job,
        RELAYMIX_CRASH_CHUNK="1", RELAYMIX_CRASH_PHASE="write",
    )
    assert proc.returncode == 130
    # 块 1 的临时产物可能残留，但索引中不存在块 1。
    store = CheckpointStore(root2)
    assert store.committed_indices() == {0}
    store.close()

    summary = run_job(root2, normal_job, is_resume=True)
    assert summary["committed_now"] == [1, 2, 3, 4, 5]
    assert summary["resumed_chunks"] == 1
    result = verify_root(root2)
    assert result["status"] == "ok"
    assert result["segments"] == 6


def test_crash_during_index_commit_resumes_safely(
    normal_job: Path, tmp_path: Path
) -> None:
    root = tmp_path / "root"
    proc = _run_crashed(
        root, normal_job,
        RELAYMIX_CRASH_CHUNK="2", RELAYMIX_CRASH_PHASE="index",
    )
    assert proc.returncode == 130
    store = CheckpointStore(root)
    assert store.committed_indices() == {0, 1}
    store.close()

    summary = run_job(root, normal_job, is_resume=True)
    assert summary["finished"] is True
    assert len(summary["committed_now"]) == 4
    result = verify_root(root)
    assert result["status"] == "ok"


# ---------------------------------------------------------------- 指纹恢复

def test_resume_rejects_changed_fingerprint(
    normal_job: Path, tmp_path: Path
) -> None:
    root = tmp_path / "root"
    proc = _run_crashed(root, normal_job, RELAYMIX_CRASH_AFTER="1")
    assert proc.returncode == 130
    data = json.loads(normal_job.read_text())
    data["frame_size"] = 4800  # 参数变化 -> 指纹变化
    changed = normal_job.parent / "manifest_changed.json"
    changed.write_text(json.dumps(data))
    with pytest.raises(CheckpointError) as ei:
        run_job(root, changed, is_resume=True)
    err = ei.value.to_dict()
    assert err["error_code"] == "params_fingerprint_mismatch"
    assert "recompute_from_tick" in err


def test_resume_rejects_changed_input(normal_job: Path, tmp_path: Path) -> None:
    root = tmp_path / "root"
    proc = _run_crashed(root, normal_job, RELAYMIX_CRASH_AFTER="1")
    assert proc.returncode == 130
    wav = normal_job.parent / fx.WAV_DIR / "a0.wav"
    original = wav.read_bytes()
    wav.write_bytes(original + b"\x00\x00")  # 内容哈希变化
    try:
        with pytest.raises(CheckpointError) as ei:
            run_job(root, normal_job, is_resume=True)
        assert ei.value.to_dict()["error_code"] == "input_fingerprint_mismatch"
    finally:
        wav.write_bytes(original)


def test_commit_is_idempotent_under_duplicate_call(tmp_path: Path) -> None:
    store = CheckpointStore(tmp_path / "cp")
    rec = ChunkRecord(
        chunk_index=0, start_tick=0, end_tick=10, frame_index=0,
        segment_file=segment_name(0), segment_hash="a" * 64,
        input_hash="b" * 64, params_fingerprint="c" * 64,
        algo_version="relaymix-algo-1.0.0",
    )
    p1 = store.commit_chunk(rec, b"abc")
    p2 = store.commit_chunk(rec, b"abc")
    assert p1 == p2 and store.committed_indices() == {0}
    store.close()


# ---------------------------------------------------------------- verify 只读

def test_verify_does_not_mutate_checkpoint(normal_job: Path, tmp_path: Path) -> None:
    root = tmp_path / "root"
    run_job(root, normal_job)
    db = root / DB_NAME
    before = db.read_bytes()
    result = verify_root(root)
    after = db.read_bytes()
    assert before == after  # 只读核验不触发数据库写入
    assert result["status"] == "ok"
    assert all(len(h) == 64 for h in result["segment_hashes"])
    assert result["roundtrip_rate"] == 1.0
    assert result["keyframe_index_hit_rate"] == 1.0


def test_verify_readonly_open_survives_when_locked(
    normal_job: Path, tmp_path: Path
) -> None:
    root = tmp_path / "root"
    run_job(root, normal_job)
    # 以读写连接占住数据库，只读核验仍可打开。
    holder = sqlite3.connect(root / DB_NAME)
    try:
        result = verify_root(root)
    finally:
        holder.close()
    assert result["status"] == "ok"


def test_deterministic_replay_produces_identical_hashes(
    normal_job: Path, tmp_path: Path
) -> None:
    r1 = tmp_path / "r1"
    r2 = tmp_path / "r2"
    run_job(r1, normal_job)
    run_job(r2, normal_job)
    assert verify_root(r1)["segment_hashes"] == verify_root(r2)["segment_hashes"]


# ---------------------------------------------------------------- CLI 退出码

def test_cli_exit_codes_and_json_fields(tmp_path: Path) -> None:
    from relaymix.cli import build_parser, main

    manifest = fx.build_truncated_wav(tmp_path / "dmg")
    parser = build_parser()
    args = parser.parse_args(["render", "--root", str(tmp_path / "root"),
                              "--manifest", str(manifest)])
    assert main(args) == 3

    good = fx.build_normal(tmp_path / "ok")
    args = parser.parse_args(["render", "--root", str(tmp_path / "okroot"),
                              "--manifest", str(good)])
    assert main(args) == 0
    args = parser.parse_args(["verify", "--root", str(tmp_path / "okroot"),
                              "--json"])
    assert main(args) == 0
