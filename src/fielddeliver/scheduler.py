"""按依赖图拓扑调度各轨处理阶段，并记录可恢复检查点。

检查点设计：
- 每个 (轨道, 阶段) 产出一个 JSON 检查点，含输入指纹（源文件哈希、
  清单 source_hash、处理参数指纹）；
- 续跑时指纹一致且校验通过才复用，否则失效重跑，避免复用错误结果；
- 并发受 max_concurrency 约束，临时缓存受 max_cache_bytes 约束。
"""
from __future__ import annotations

import concurrent.futures as cf
import hashlib
import json
import os
import time
from dataclasses import asdict
from pathlib import Path

from .model import (ErrorCategory, Issue, Manifest, PipelineError,
                    TrackKind, TrackResult, TrackState, atomic_write_json,
                    read_json, sha256_file)
from .timeline import blocked_by_failure, topological_order

CHECKPOINT_DIR = "checkpoints"

# 阶段名（顺序即单轨内的处理顺序）
VIDEO_STAGES = ("probe", "index", "seek_verify", "color")
AUDIO_STAGES = ("audio", "color")


def _fingerprint(manifest: Manifest, track_id: str, stage: str,
                 file_hash: str | None) -> str:
    tr = manifest.tracks[track_id]
    payload = {
        "source_hash": manifest.source_hash,
        "file_hash": file_hash,
        "stage": stage,
        "params": tr.to_dict(),
        "schema": 1,
    }
    return hashlib.sha256(
        json.dumps(payload, sort_keys=True, default=str).encode()
    ).hexdigest()


class CheckpointStore:
    def __init__(self, work_dir: str) -> None:
        self.dir = Path(work_dir) / CHECKPOINT_DIR
        self.dir.mkdir(parents=True, exist_ok=True)

    def _path(self, track_id: str, stage: str) -> Path:
        safe = track_id.replace(os.sep, "_")
        return self.dir / f"{safe}__{stage}.json"

    def save(self, manifest: Manifest, track_id: str, stage: str,
             file_hash: str | None, payload: dict) -> None:
        doc = {"fingerprint": _fingerprint(manifest, track_id, stage,
                                           file_hash),
               "track_id": track_id, "stage": stage,
               "saved_at": time.time(), "payload": payload}
        atomic_write_json(self._path(track_id, stage), doc)

    def load(self, manifest: Manifest, track_id: str, stage: str,
             file_hash: str | None) -> dict | None:
        """指纹匹配且文档完整才返回 payload，否则视为失效返回 None。"""
        path = self._path(track_id, stage)
        if not path.is_file():
            return None
        try:
            doc = read_json(path)
        except (OSError, ValueError):
            return None
        expect = _fingerprint(manifest, track_id, stage, file_hash)
        if doc.get("fingerprint") != expect:
            return None
        if doc.get("track_id") != track_id or doc.get("stage") != stage:
            return None
        return doc.get("payload")

    def invalidate_track(self, track_id: str) -> int:
        n = 0
        for p in self.dir.glob(f"{track_id.replace(os.sep, '_')}__*.json"):
            p.unlink()
            n += 1
        return n


class Scheduler:
    def __init__(self, manifest: Manifest, work_dir: str) -> None:
        self.manifest = manifest
        self.work_dir = work_dir
        self.store = CheckpointStore(work_dir)
        self.results: dict[str, TrackResult] = {}
        self._hashes: dict[str, str] = {}
        self._cache_bytes = 0

    # ------------------------------------------------------------ 工具
    def file_hash(self, track_id: str, abs_path: str) -> str:
        if track_id not in self._hashes:
            size = os.path.getsize(abs_path)
            if size > self.manifest.limits["max_cache_bytes"]:
                # 大文件只哈希一次且不缓存内容，哈希本身分块进行。
                self._hashes[track_id] = sha256_file(
                    abs_path, self.manifest.limits["chunk_bytes"])
            else:
                self._hashes[track_id] = sha256_file(abs_path)
        return self._hashes[track_id]

    def _acquire_cache(self, need: int) -> None:
        """限制并发缓存；超限时抛内部错误而非无限吃内存。"""
        cap = self.manifest.limits["max_cache_bytes"]
        if need > cap:
            raise PipelineError(
                ErrorCategory.INTERNAL,
                f"单任务缓存需求 {need}B 超过上限 {cap}B")
        # 简化的预算占用：调度层串行释放，超限即拒绝新任务。
        if self._cache_bytes + need > cap:
            raise PipelineError(
                ErrorCategory.INPUT_TOO_LARGE,
                f"并发缓存 {self._cache_bytes + need}B 超过上限 {cap}B")
        self._cache_bytes += need

    def _release_cache(self, size: int) -> None:
        self._cache_bytes = max(0, self._cache_bytes - size)

    # ------------------------------------------------------------ 调度
    def run(self, stage_fn) -> dict[str, TrackResult]:
        """stage_fn(track, result, checkpoint_load, checkpoint_save) 执行单轨。

        按拓扑序分批：同一批内无依赖关系的轨道并发执行，
        每批结束后下游可见上游状态；失败/跳过的轨道阻止其下游提交。
        """
        order = topological_order(self.manifest)
        remaining = set(order)
        done: set[str] = set()
        max_workers = max(1, self.manifest.limits["max_concurrency"])

        while remaining:
            ready = [
                t for t in order if t in remaining
                and all(d in done for d in self.manifest.tracks[t].depends_on)
            ]
            if not ready:
                raise PipelineError(ErrorCategory.DEPENDENCY_CYCLE,
                                    "调度卡死：存在无法解析的依赖")
            with cf.ThreadPoolExecutor(max_workers=max_workers) as pool:
                futures = {pool.submit(self._run_one, tid, stage_fn): tid
                           for tid in ready}
                for fut in cf.as_completed(futures):
                    tid = futures[fut]
                    result = fut.result()      # 单轨异常已在内部归类
                    self.results[tid] = result
                    done.add(tid)
                    remaining.discard(tid)
        return self.results

    def _run_one(self, track_id: str, stage_fn) -> TrackResult:
        tr = self.manifest.tracks[track_id]
        result = TrackResult(track_id=track_id, kind=tr.kind,
                             state=TrackState.RUNNING)
        blocker = blocked_by_failure(tr, self.results)
        if blocker is not None:
            result.state = TrackState.SKIPPED
            result.issues.append(Issue(
                ErrorCategory.UPSTREAM_FAILED, track_id,
                f"上游轨道 {blocker} 未成功，阻止本轨提交"))
            return result
        try:
            stage_fn(self, tr, result)
            if result.state is TrackState.RUNNING:
                result.state = TrackState.DONE
        except PipelineError as exc:
            result.state = TrackState.FAILED
            result.issues.append(Issue(
                exc.category, track_id, exc.message,
                byte_offset=exc.byte_offset))
        except Exception as exc:  # noqa: BLE001 - 内部错误兜底
            result.state = TrackState.FAILED
            result.issues.append(Issue(
                ErrorCategory.INTERNAL, track_id,
                f"未预期内部错误: {type(exc).__name__}: {exc}"))
        return result

    # ------------------------------------------------------------ 检查点 API
    def checkpoint_load(self, manifest: Manifest, track_id: str, stage: str,
                        file_hash: str | None) -> dict | None:
        return self.store.load(manifest, track_id, stage, file_hash)

    def checkpoint_save(self, manifest: Manifest, track_id: str, stage: str,
                        file_hash: str | None, payload: dict) -> None:
        self.store.save(manifest, track_id, stage, file_hash, payload)
        self.results.setdefault(
            track_id, TrackResult(track_id, manifest.tracks[track_id].kind,
                                  TrackState.RUNNING)).checkpoints.append(stage)

    def stage_latency(self, result: TrackResult) -> list[float]:
        return result.latency_ms

    def summarize_states(self) -> dict[str, int]:
        counts: dict[str, int] = {}
        for r in self.results.values():
            counts[r.state.value] = counts.get(r.state.value, 0) + 1
        return counts
