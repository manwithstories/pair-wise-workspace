"""只读核验：独立重读全部产物，绝不改写检查点。

检查项：
    * 片段文件存在且字节 SHA-256 与索引 segment_hash 一致；
    * soundfile 往返解析：采样率、声道数、帧数与采样区间一致；
    * 时间戳单调递增且窗口连续，frame_index 与 chunk_index 映射一致；
    * 整数刻度索引查找：边界时间必命中，非关键帧刻度报告未命中原因；
    * 逐片段输入哈希与参数指纹可追溯；末尾不完整窗口单独标记。
输出固定字段名的 JSON 摘要字典，由 CLI 打成 JSON 行。
"""

from dataclasses import asdict
import hashlib
import json
import sqlite3
from pathlib import Path

import soundfile as sf

from .checkpoint import DB_NAME, RUN_META_NAME
from .errors import CheckpointError
from .frame_export import iter_windows, segment_name
from .manifest import build_manifest
from .timeline import parameter_fingerprint


def _open_readonly(root: Path) -> sqlite3.Connection:
    db = root / DB_NAME
    if not db.exists():
        raise CheckpointError(
            "checkpoint database missing", error_code="checkpoint_missing"
        )
    conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    conn.row_factory = sqlite3.Row
    return conn


def verify_root(root: str | Path) -> dict:
    root = Path(root)
    conn = _open_readonly(root)
    try:
        return _verify(root, conn)
    finally:
        conn.close()


def _verify(root: Path, conn: sqlite3.Connection) -> dict:
    meta = json.loads((root / RUN_META_NAME).read_text(encoding="utf-8"))
    manifest = build_manifest(meta["manifest"])
    expected_fp = parameter_fingerprint(manifest)
    rows = conn.execute("SELECT * FROM chunks ORDER BY chunk_index").fetchall()
    windows = list(
        iter_windows(manifest.output_start, manifest.output_end, manifest.frame_size)
    )

    segment_hashes: list[str] = []
    hash_checks: list[dict] = []
    roundtrip_ok = 0
    timestamp_errors: list[dict] = []
    index_misses: list[dict] = []
    provenance_errors: list[dict] = []
    incomplete_tail = None

    prev_end: int | None = None
    prev_start: int | None = None
    prev_index = -1
    for row in rows:
        idx = row["chunk_index"]
        seg_path = root / "segments" / row["segment_file"]
        if not seg_path.exists():
            hash_checks.append(
                {"chunk_index": idx, "ok": False, "reason": "segment_file_missing"}
            )
            continue
        actual = hashlib.sha256(seg_path.read_bytes()).hexdigest()
        ok = actual == row["segment_hash"] and len(actual) == 64
        hash_checks.append({"chunk_index": idx, "ok": ok, "reason": None if ok else "hash_mismatch"})
        segment_hashes.append(actual)

        # 往返解析核验（独立于编码路径，只用 soundfile）。
        data, sr = sf.read(str(seg_path), dtype="int16", always_2d=True)
        expected_frames = row["end_tick"] - row["start_tick"]
        rt_ok = (
            sr == manifest.master_rate
            and data.shape[1] == manifest.output_channels
            and data.shape[0] == expected_frames
            and row["frame_index"] == idx
            and row["params_fingerprint"] == expected_fp
            and row["algo_version"] == meta["algo_version"]
        )
        if rt_ok:
            roundtrip_ok += 1
        else:
            provenance_errors.append(
                {"chunk_index": idx, "reason": "roundtrip_or_fingerprint_mismatch",
                 "frames": data.shape[0], "expected_frames": expected_frames}
            )

        # 时间戳单调递增且窗口连续。
        if idx != prev_index + 1:
            timestamp_errors.append(
                {"chunk_index": idx, "reason": "chunk_index_gap",
                 "previous": prev_index}
            )
        if prev_end is not None and row["start_tick"] != prev_end:
            timestamp_errors.append(
                {"chunk_index": idx, "reason": "timestamp_non_contiguous",
                 "start_tick": row["start_tick"], "expected": prev_end}
            )
        if prev_start is not None and row["start_tick"] <= prev_start:
            timestamp_errors.append(
                {"chunk_index": idx, "reason": "timestamp_non_monotonic",
                 "start_tick": row["start_tick"], "previous_start": prev_start}
            )
        prev_start = row["start_tick"]
        prev_end = row["end_tick"]
        prev_index = idx
        if expected_frames < manifest.frame_size:
            incomplete_tail = {"chunk_index": idx, "frames": expected_frames,
                               "frame_size": manifest.frame_size}

    # 整数刻度索引：所有窗口边界必须命中；抽查刻度给出未命中原因。
    required_hits = 0
    got_hits = 0
    for win in windows:
        required_hits += 1
        found = conn.execute(
            "SELECT * FROM keyframes WHERE tick=?", (win.start_tick,)
        ).fetchone()
        if found is not None and found["chunk_index"] == win.index:
            got_hits += 1
        else:
            index_misses.append(
                {"tick": win.start_tick, "hit": False,
                 "reason": "boundary_tick_missing_from_index"}
            )
    # 额外探针：窗口内部刻度与输出末端 —— 预期未命中，记录原因（不计入命中率）。
    extra_probes: list[dict] = []
    for win in windows:
        mid = win.start_tick + min(1, win.end_tick - win.start_tick - 1)
        if mid != win.start_tick:
            extra_probes.append(
                {"tick": mid, "hit": False, "reason": "tick_not_a_keyframe"}
            )
    extra_probes.append(
        {"tick": manifest.output_end, "hit": False,
         "reason": "output_end_is_exclusive_boundary"}
    )

    total = max(len(rows), 1)
    finished = (root / ".finished").exists()
    summary = {
        "status": "ok"
        if not (timestamp_errors or index_misses or provenance_errors)
        and all(c["ok"] for c in hash_checks)
        else "failed",
        "finished": finished,
        "root": str(root),
        "master_rate": manifest.master_rate,
        "segments": len(rows),
        "expected_segments": len(windows),
        "segment_hashes": segment_hashes,
        "roundtrip_rate": round(roundtrip_ok / total, 6),
        "keyframe_index_hit_rate": round(got_hits / max(required_hits, 1), 6),
        "hash_checks": hash_checks,
        "timestamp_errors": timestamp_errors,
        "index_misses": index_misses,
        "expected_misses": extra_probes,
        "provenance_errors": provenance_errors,
        "incomplete_tail_window": incomplete_tail,
        "params_fingerprint": expected_fp,
        "input_fingerprint": meta["input_fingerprint"],
        "algo_version": meta["algo_version"],
    }
    return summary
