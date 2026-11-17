"""Feature lifecycle management across a frame sequence.

Owns detection/re-detection, per-frame matching, lost-frame counting
(occlusion handling) and track records. Motion-level outlier rejection
lives in `motion.py`; this module only guarantees per-frame point
correspondences with quality annotations.
"""

from __future__ import annotations

import numpy as np

from .features import detect_corners, track_points


class TrackResult:
    """Per-frame correspondences plus per-track lifetime records."""

    def __init__(self, n_frames):
        self.n_frames = n_frames
        self.ids: list[np.ndarray] = []
        self.xy: list[np.ndarray] = []
        self.conf: list[np.ndarray] = []
        self.resid: list[np.ndarray] = []
        self.tracks: dict[int, dict] = {}

    def add_frame(self, ids, xy, conf, resid):
        self.ids.append(ids)
        self.xy.append(xy)
        self.conf.append(conf)
        self.resid.append(resid)

    def matches(self, k: int):
        """Correspondences between frame k-1 and k -> (src, dst, ids)."""
        if k <= 0 or k >= self.n_frames:
            return (np.zeros((0, 2)),) * 2 + (np.zeros(0, dtype=int),)
        a = {i: p for i, p in zip(self.ids[k - 1], self.xy[k - 1])}
        b = {i: p for i, p in zip(self.ids[k], self.xy[k])}
        common = sorted(a.keys() & b.keys())
        if not common:
            return (np.zeros((0, 2)),) * 2 + (np.zeros(0, dtype=int),)
        src = np.array([a[i] for i in common])
        dst = np.array([b[i] for i in common])
        return src, dst, np.array(common)

    def active_count(self, k: int) -> int:
        return len(self.ids[k])


class FeatureTracker:
    def __init__(self, max_points: int = 150, min_points: int = 40,
                 max_lost: int = 3, redetect_below: int = 60,
                 conf_min: float = 0.02, resid_max: float = 40.0,
                 detect_min_dist: int = 8, levels: int = 3,
                 search_r: int = 4, patch_r: int = 5):
        self.max_points = max_points
        self.min_points = min_points
        self.max_lost = max_lost
        self.redetect_below = redetect_below
        self.conf_min = conf_min
        self.resid_max = resid_max
        self.detect_min_dist = detect_min_dist
        self.levels = levels
        self.search_r = search_r
        self.patch_r = patch_r

    def _spawn(self, frame, active, next_id, result, k):
        """Detect new corners away from existing points."""
        exclude = np.array([a["xy"] for a in active.values()]) \
            if active else np.zeros((0, 2))
        pts, scores = detect_corners(
            frame, max_points=self.max_points,
            min_dist=self.detect_min_dist, min_points=self.min_points)
        added = 0
        for p in pts:
            if len(active) >= self.max_points:
                break
            if len(exclude) and np.min(np.hypot(
                    *(exclude - p).T)) < self.detect_min_dist:
                continue
            active[next_id] = {"xy": p, "lost": 0, "born": k,
                               "conf": [], "resid": []}
            result.tracks[next_id] = {"born": k, "last": k, "reason": "end",
                                      "lost_frames": 0}
            next_id += 1
            added += 1
        return next_id, added

    def run(self, frames: np.ndarray) -> TrackResult:
        n = len(frames)
        result = TrackResult(n)
        active: dict[int, dict] = {}
        next_id = 0
        predict = np.zeros(2)  # constant-velocity motion prediction
        next_id, _ = self._spawn(frames[0], active, next_id, result, 0)
        self._record(result, active)
        for k in range(1, n):
            if active:
                ids = np.array(sorted(active.keys()))
                pts = np.array([active[i]["xy"] for i in ids])
                new, conf, resid, valid = track_points(
                    frames[k - 1], frames[k], pts, levels=self.levels,
                    search_r=self.search_r, patch_r=self.patch_r,
                    predict=predict)
                ok_mask = valid & (conf >= self.conf_min) & (resid <= self.resid_max)
                if ok_mask.sum() >= 8:  # update prediction from inliers
                    predict = np.median((new - pts)[ok_mask], axis=0)
                for j, tid in enumerate(ids):
                    tr = active[tid]
                    ok = (valid[j] and conf[j] >= self.conf_min
                          and resid[j] <= self.resid_max)
                    if ok:
                        tr["xy"] = new[j]
                        tr["lost"] = 0
                        tr["conf"].append(float(conf[j]))
                        tr["resid"].append(float(resid[j]))
                        result.tracks[tid]["last"] = k
                    else:
                        tr["lost"] += 1
                        result.tracks[tid]["lost_frames"] += 1
                dead = [t for t, a in active.items()
                        if a["lost"] > self.max_lost]
                for t in dead:
                    result.tracks[t]["reason"] = "lost"
                    del active[t]
            if len(active) < self.redetect_below:
                next_id, _ = self._spawn(frames[k], active, next_id,
                                         result, k)
            self._record(result, active)
        return result

    @staticmethod
    def _record(result, active):
        # Only points matched on THIS frame (lost == 0) are recorded;
        # a lost point keeps its stale position and must never enter a
        # correspondence (it would fake a zero-displacement measurement).
        visible = {i: a for i, a in active.items() if a["lost"] == 0}
        ids = np.array(sorted(visible.keys()), dtype=int)
        xy = np.array([visible[i]["xy"] for i in ids]) \
            if len(ids) else np.zeros((0, 2))
        conf = np.array([visible[i]["conf"][-1] if visible[i]["conf"] else 1.0
                         for i in ids]) if len(ids) else np.zeros(0)
        resid = np.array([visible[i]["resid"][-1] if visible[i]["resid"] else 0.0
                          for i in ids]) if len(ids) else np.zeros(0)
        result.add_frame(ids, xy, conf, resid)
