"""Bounded worker pool with signal handling, retries and orphan reclamation."""
from __future__ import annotations

import os
from pathlib import Path
import signal
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass

from ..codec.encoder import EncodeParams, decode, encode
from ..media.yuv import read_all, resize
from ..metrics.quality import aggregate
from ..store import db

MAX_ATTEMPTS = 3  # initial attempt + 2 retries
INTERRUPT_RUN_STATUSES = {"pending", "running"}


@dataclass
class SweepStats:
    tasks_total: int
    done: int
    resumed: int
    orphaned_reclaimed: int
    duplicates_collapsed: int
    failed: int
    interrupted: bool

    def as_dict(self) -> dict:
        return {
            "tasks_total": self.tasks_total,
            "done": self.done,
            "resumed": self.resumed,
            "orphaned_reclaimed": self.orphaned_reclaimed,
            "duplicates_collapsed": self.duplicates_collapsed,
            "failed": self.failed,
            "interrupted": self.interrupted,
        }


class _StopFlag:
    def __init__(self) -> None:
        self.event = threading.Event()

    def trigger(self, *_) -> None:
        self.event.set()

    def stopped(self) -> bool:
        return self.event.is_set()


def _bitstream_path(db_path: str, key: str) -> str:
    directory = os.path.join(os.path.dirname(os.path.abspath(db_path)), "streams")
    return os.path.join(directory, f"{key}.abr")


def run_task(conn_factory, task: dict, stop: _StopFlag) -> None:
    conn = conn_factory()
    try:
        width, height = task["width"], task["height"]
        source_frames = read_all(task["source"], _source_width(task), _source_height(task))
        duration = len(source_frames) / task["fps"]
        params = EncodeParams(width, height, task["fps"], task["qp"], task["gop"])
        out_path = _bitstream_path(task["_db_path"], task["key"])
        size = encode(source_frames, params, out_path)
        if stop.stopped():
            raise InterruptedError("stop requested after encode")
        decoded = decode(out_path, task["qp"])
        if len(decoded) != len(source_frames):
            raise RuntimeError(f"decoded {len(decoded)} frames, source has {len(source_frames)}")
        reference = [
            frame if (frame.width, frame.height) == (width, height) else resize(frame, width, height)
            for frame in source_frames
        ]
        metrics = aggregate(reference, decoded)
        result = {
            "bytes": size,
            "duration_s": round(duration, 6),
            "actual_kbps": round(size * 8 / duration / 1000.0, 4),
            "psnr_db": metrics["psnr_db"],
            "psnr_min_db": metrics["psnr_min_db"],
            "ssim": metrics["ssim"],
            "ssim_min": metrics["ssim_min"],
            "bitstream_path": out_path,
        }
        db.mark_done(conn, task["key"], result)
    except InterruptedError:
        db.requeue_failed_attempt(conn, task["key"], "interrupted")
    except Exception as exc:  # noqa: BLE001 - recorded as task failure
        message = f"{type(exc).__name__}: {exc}"
        if task["attempts"] + 1 >= MAX_ATTEMPTS:
            db.mark_failed(conn, task["key"], message)
        else:
            db.requeue_failed_attempt(conn, task["key"], message)
    finally:
        conn.close()


def _source_width(task: dict) -> int:
    return task.get("source_width") or task["width"]


def _source_height(task: dict) -> int:
    return task.get("source_height") or task["height"]


def _cleanup_temp_streams(db_path: str) -> int:
    directory = Path(_bitstream_path(db_path, "probe")).parent
    if not directory.is_dir():
        return 0
    removed = 0
    for tmp in directory.glob("*.tmp"):
        try:
            tmp.unlink()
            removed += 1
        except OSError:
            pass
    return removed


def run_sweep(conn, specs, duplicates: int, workers: int, db_path: str,
              source_width: int, source_height: int) -> SweepStats:
    stop = _StopFlag()
    previous = db.counts(conn)
    resumed_before = previous.get("done", 0) + previous.get("failed", 0)
    _cleanup_temp_streams(db_path)
    orphaned = db.reclaim_orphans(conn)
    db.upsert_pending(conn, specs)
    db.validate_schema(conn)

    handlers: dict[int, object] = {}
    for signum in (signal.SIGINT, signal.SIGTERM):
        handlers[signum] = signal.getsignal(signum)
        signal.signal(signum, stop.trigger)

    def conn_factory():
        child = db.connect(db_path)
        db.initialize(child)
        return child

    failed_seen = 0
    pool = ThreadPoolExecutor(max_workers=max(1, workers))
    in_flight: dict = {}
    try:
        while True:
            if stop.stopped():
                break
            in_flight = {future: key for future, key in in_flight.items() if not future.done()}
            if len(in_flight) >= max(1, workers):
                time.sleep(0.02)
                continue
            row = db.claim_next(conn)
            if row is None:
                break
            row["_db_path"] = db_path
            row["source_width"] = source_width
            row["source_height"] = source_height
            if row["attempts"] >= MAX_ATTEMPTS:
                db.mark_failed(conn, row["key"], "max attempts exceeded")
                continue
            future = pool.submit(run_task, conn_factory, row, stop)
            in_flight[future] = row["key"]
        if not stop.stopped():
            for future in list(in_flight):
                future.result()
    finally:
        # On interrupt do not wait for in-flight tasks: their rows stay
        # 'running' so the next invocation reclaims them as orphans.
        # The caller is expected to os._exit() promptly, because pool
        # threads are non-daemon and would otherwise block interpreter exit.
        pool.shutdown(wait=not stop.stopped(), cancel_futures=stop.stopped())
        for signum, original in handlers.items():
            signal.signal(signum, original)
        stats_now = db.counts(conn)
        failed_seen = stats_now.get("failed", 0)

    stats_now = db.counts(conn)
    return SweepStats(
        tasks_total=stats_now["total"],
        done=stats_now.get("done", 0),
        resumed=resumed_before,
        orphaned_reclaimed=orphaned,
        duplicates_collapsed=duplicates,
        failed=failed_seen,
        interrupted=stop.stopped(),
    )


