"""Single-process task scheduling, retries and resumable execution."""

from __future__ import annotations

import json
import os
import time
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Callable

from .codec import decode, encode
from .metrics import evaluate
from .source import generate_reference, generate_sequence, resize_sequence
from .store import Store, StoreError


class Interrupted(RuntimeError):
    """Raised by the controlled --stop-after interruption."""


@dataclass
class ScanStats:
    skipped: int = 0
    completed: int = 0
    done: int = 0
    failed: int = 0
    pending: int = 0
    attempts: int = 0
    recovered: int = 0
    cleaned_temps: int = 0
    interrupted: bool = False
    elapsed_sec: float = 0.0

    def as_dict(self) -> dict[str, object]:
        return asdict(self)


def _task_error_code(exc: Exception) -> str:
    name = exc.__class__.__name__.upper()
    return "E_" + "".join(character for character in name if character.isalnum())


class Scheduler:
    def __init__(
        self,
        store: Store,
        progress: Callable[[dict[str, object]], None] | None = None,
    ):
        self.store = store
        self.progress = progress
        if store.path == ":memory:":
            self.temp_dir = Path(os.getcwd()) / ".abrlab-tmp"
        else:
            db_path = Path(store.path)
            self.temp_dir = db_path.parent / f".{db_path.name}.work"
        self.temp_dir.mkdir(parents=True, exist_ok=True)

    def cleanup_temps(self) -> int:
        removed = 0
        for path in self.temp_dir.glob("*.part"):
            try:
                path.unlink()
                removed += 1
            except OSError:
                continue
        return removed

    def _temp_path(self, key: str) -> Path:
        return self.temp_dir / f"{key}.part"

    def _process_one(self, row: object, stats: ScanStats) -> None:
        source = json.loads(row["source_json"])
        params = json.loads(row["params_json"])
        started = time.perf_counter()
        temp_path = self._temp_path(str(row["task_key"]))
        try:
            original = generate_sequence(source, str(params["chroma"]))
            payload = encode(original, int(params["qp"]), int(params["block_size"]), str(params["chroma"]))
            with open(temp_path, "wb") as handle:
                handle.write(payload)
                handle.flush()
                os.fsync(handle.fileno())
            reconstructed = decode(payload)
            reference = generate_reference(source, str(params["chroma"]))
            compared = resize_sequence(
                reconstructed, int(reference.shape[2]), int(reference.shape[1])
            )
            measured = evaluate(reference, compared)
            elapsed = time.perf_counter() - started
            duration = float(source["frames"]) / float(source["fps"])
            bitrate_kbps = len(payload) * 8.0 / duration / 1000.0
            self.store.complete_task(
                str(row["task_key"]), payload, round(bitrate_kbps, 6),
                measured["psnr_db"], measured["ssim"], elapsed
            )
            temp_path.unlink(missing_ok=True)
            stats.completed += 1
            stats.done += 1
            if self.progress:
                self.progress({
                    "task_key": row["task_key"],
                    "name": row["name"],
                    "bitrate_kbps": round(bitrate_kbps, 3),
                    "psnr_db": measured["psnr_db"],
                    "ssim": measured["ssim"],
                    "bytes": len(payload),
                    "elapsed_sec": round(elapsed, 4),
                })
        except Exception as exc:
            temp_path.unlink(missing_ok=True)
            elapsed = time.perf_counter() - started
            status = self.store.fail_task(
                str(row["task_key"]), _task_error_code(exc), str(exc), elapsed
            )
            if status == "failed":
                stats.failed += 1

    def run(self, stop_after: int | None = None) -> ScanStats:
        if stop_after is not None and stop_after < 0:
            raise ValueError("stop_after must not be negative")
        started = time.perf_counter()
        stats = ScanStats()
        counts = self.store.status_counts()
        stats.skipped = counts.get("done", 0)
        stats.done = stats.skipped
        stats.failed = counts.get("failed", 0)
        stats.recovered = self.store.recover_stale()
        stats.cleaned_temps = self.cleanup_temps()
        self.store.finalize_exhausted()
        tasks = self.store.resumable_tasks()
        try:
            for row in tasks:
                if stop_after is not None and stats.completed >= stop_after:
                    raise Interrupted()
                key = str(row["task_key"])
                while self.store.task_attempts(key) < 3:
                    attempts = self.store.claim_task(key)
                    if attempts is None:
                        break
                    stats.attempts += 1
                    self._process_one(row, stats)
                    if stop_after is not None and stats.completed >= stop_after:
                        raise Interrupted()
                self.store.finalize_exhausted()
        except Interrupted:
            stats.interrupted = True
        finally:
            current = self.store.status_counts()
            stats.done = current.get("done", 0)
            stats.failed = current.get("failed", 0)
            stats.pending = current.get("pending", 0) + current.get("running", 0)
            stats.elapsed_sec = round(time.perf_counter() - started, 4)
        return stats
