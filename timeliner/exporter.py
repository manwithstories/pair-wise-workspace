"""Sharded parallel export with resume; one compositor instance per worker."""

import json
import multiprocessing
import os
import shutil

from .compositor import Compositor
from .digest import FRAME_FMT, frame_digest
from .model import load_timeline

PROGRESS_DIR = ".progress"


def _shard(frames, workers):
    """Split a sorted frame list into contiguous shards."""
    shards = []
    n = len(frames)
    if n == 0:
        return shards
    workers = min(workers, n)
    base, extra = divmod(n, workers)
    pos = 0
    for k in range(workers):
        size = base + (1 if k < extra else 0)
        if size:
            shards.append(frames[pos:pos + size])
        pos += size
    return shards


def _render_shard(args):
    """Worker: build one Compositor and render its whole shard."""
    (timeline_path, assets_dir, out_dir, fps, frames,
     no_frames, progress_path) = args
    timeline = load_timeline(timeline_path)
    comp = Compositor(timeline, assets_dir)
    frames_dir = os.path.join(out_dir, "frames")
    results = {}
    prog = open(progress_path, "a", encoding="utf-8") if progress_path else None
    try:
        for frame in frames:
            rgb = comp.render_frame(frame, fps)
            digest = frame_digest(rgb)
            if not no_frames:
                tmp = os.path.join(frames_dir, FRAME_FMT % frame + ".tmp")
                with open(tmp, "wb") as fh:
                    fh.write(rgb)
                os.replace(tmp, os.path.join(frames_dir, FRAME_FMT % frame))
            results[frame] = digest
            if prog is not None:
                prog.write(json.dumps({"frame": frame, "digest": digest}) + "\n")
                prog.flush()
    finally:
        if prog is not None:
            prog.close()
    return results, comp.blend_calls


def _load_progress(out_dir):
    """Read per-shard progress journals left by an interrupted run."""
    done = {}
    prog_dir = os.path.join(out_dir, PROGRESS_DIR)
    if not os.path.isdir(prog_dir):
        return done
    for name in sorted(os.listdir(prog_dir)):
        if not name.endswith(".jsonl"):
            continue
        with open(os.path.join(prog_dir, name), "r", encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    rec = json.loads(line)
                except json.JSONDecodeError:
                    continue  # tolerate a torn final line after Ctrl-C
                done[int(rec["frame"])] = rec["digest"]
    return done


def _adopt_frame_files(out_dir, n_frames, frame_bytes):
    """Resume from already-written frame files (size-checked and re-hashed)."""
    done = {}
    frames_dir = os.path.join(out_dir, "frames")
    for i in range(n_frames):
        path = os.path.join(frames_dir, FRAME_FMT % i)
        try:
            if os.path.getsize(path) != frame_bytes:
                continue
            with open(path, "rb") as fh:
                done[i] = frame_digest(fh.read())
        except OSError:
            continue
    return done


def run_export(timeline_path, assets_dir, out_dir, fps, n_frames, workers,
               resume=False, no_frames=False):
    """Render frames, returning {digests, blend_calls, resumed_from}."""
    os.makedirs(os.path.join(out_dir, "frames"), exist_ok=True)
    timeline = load_timeline(timeline_path)
    frame_bytes = timeline.width * timeline.height * 3
    done = {}
    if resume:
        done.update(_load_progress(out_dir))
        if not no_frames:
            done.update(_adopt_frame_files(out_dir, n_frames, frame_bytes))
    resumed_from = len(done)
    remaining = [i for i in range(n_frames) if i not in done]
    prog_dir = os.path.join(out_dir, PROGRESS_DIR)
    shards = _shard(remaining, max(1, workers))
    blend_calls = 0
    if workers > 1 and len(shards) > 1:
        os.makedirs(prog_dir, exist_ok=True)
        tasks = [
            (timeline_path, assets_dir, out_dir, fps, shard, no_frames,
             os.path.join(prog_dir, "shard_%03d.jsonl" % k))
            for k, shard in enumerate(shards)
        ]
        ctx = multiprocessing.get_context("spawn")
        pool = ctx.Pool(processes=workers)
        try:
            for results, calls in pool.imap(_render_shard, tasks):
                done.update(results)
                blend_calls += calls
        except BaseException:
            pool.terminate()
            pool.join()
            raise
        pool.close()
        pool.join()
    else:
        for k, shard in enumerate(shards):
            progress_path = None
            if no_frames:
                os.makedirs(prog_dir, exist_ok=True)
                progress_path = os.path.join(prog_dir, "shard_%03d.jsonl" % k)
            results, calls = _render_shard(
                (timeline_path, assets_dir, out_dir, fps, shard,
                 no_frames, progress_path))
            done.update(results)
            blend_calls += calls
    shutil.rmtree(prog_dir, ignore_errors=True)
    digests = [done[i] for i in range(n_frames)]
    return {"digests": digests, "blend_calls": blend_calls,
            "resumed_from": resumed_from}
