"""Deterministic fixture generator: paired v1/v2 exchange files.

The same model is emitted twice: v1 (flat integer frames) and v2 (nested
rational times at mixed rates, exercising the rounding-residual path).
Generated timelines intentionally contain same-track overlaps, cross-track
video overlaps, orphan transitions, zero-length clips and negative starts.
"""
from __future__ import annotations

import json
import random
from fractions import Fraction
from pathlib import Path

from .model import (
    AUDIO, GAP, TRANSITION, VIDEO,
    Clip, TimeRange, Timeline, Track, round_half_up,
)

FPS = Fraction(24)


def _distribute(total: int, weights: list[float]) -> list[int]:
    raw = [total * w for w in weights]
    counts = [int(x) for x in raw]
    for i in sorted(range(len(raw)), key=lambda i: raw[i] - counts[i],
                    reverse=True)[: total - sum(counts)]:
        counts[i] += 1
    return counts


def _sequential(rng: random.Random, name: str, count: int,
                overlap_p: float, transition_p: float = 0.0) -> tuple[list[Clip], int]:
    items: list[Clip] = []
    cursor, i = 0, 0
    prev_id: str | None = None
    while i < count:
        cid = f"{name.lower()}-{i:03d}"
        dur = rng.randint(24, 120)
        roll = rng.random()
        if prev_id and roll < overlap_p:
            start = cursor - rng.randint(6, 36)
        elif prev_id and transition_p and roll < overlap_p + transition_p:
            items.append(Clip(id=f"{name.lower()}-trans{i:03d}", kind=TRANSITION,
                              range=TimeRange(cursor, 12),
                              left=prev_id, right=cid))
            start = cursor + 12
        elif prev_id and roll < overlap_p + transition_p + 0.08:
            gap = rng.randint(6, 48)
            items.append(Clip(id=f"{name.lower()}-gap{i:03d}", kind=GAP,
                              range=TimeRange(cursor, gap)))
            start = cursor + gap
        else:
            start = cursor + rng.choice([0, 0, 0, 6, 12, 24])
        items.append(Clip(id=cid, range=TimeRange(start, dur)))
        cursor = start + dur
        prev_id = cid
        i += 1
    return items, cursor


def _broll(rng: random.Random, name: str, count: int, span: int) -> list[Clip]:
    items = []
    for i in range(count):
        dur = rng.randint(24, 72)
        start = rng.randint(0, max(0, span - dur))
        items.append(Clip(id=f"{name.lower()}-{i:03d}", range=TimeRange(start, dur)))
    return items


def _inject_defects(tl: Timeline, rng: random.Random) -> None:
    clips = [c for tr in tl.tracks for c in tr.clips()]
    n = len(clips)
    for c in rng.sample(clips, max(1, n // 66)):          # zero-length
        c.range = TimeRange(c.range.start, 0)
    alive = [c for c in clips if c.range.duration > 24]
    for c in rng.sample(alive, max(1, n // 100)):         # negative start
        shift = rng.randint(6, 24)
        c.range = TimeRange(-shift, c.range.duration + shift)
    for c in rng.sample(alive, max(1, n // 50)):          # disabled
        c.enabled = False
    video = [c for c in tl.tracks[1].clips() if c.enabled]
    for c in rng.sample(video, min(2, len(video))):       # muted video
        c.muted = True
    v1 = tl.tracks[0]
    v1.items.append(Clip(id="v1-orphan-trans", kind=TRANSITION,
                         range=TimeRange(96, 12),
                         left="ghost-left", right="ghost-right"))
    tail = max(c.range.end for c in v1.clips())
    v1.items.append(Clip(id="v1-tailgap", kind=GAP, range=TimeRange(tail, 48)))
    a1 = next(tr for tr in tl.tracks if tr.kind == AUDIO)
    atail = max(c.range.end for c in a1.clips())
    a1.items.append(Clip(id="a1-tailgap", kind=GAP, range=TimeRange(atail, 48)))


def build_model(seed: int, n_clips: int) -> Timeline:
    rng = random.Random(seed)
    spec = [("V1", VIDEO, 0.30), ("V2", VIDEO, 0.07), ("V3", VIDEO, 0.03),
            ("A1", AUDIO, 0.32), ("A2", AUDIO, 0.28)]
    counts = _distribute(n_clips, [w for _, _, w in spec])
    tracks: list[Track] = []
    span = 0
    for (name, kind, _), count in zip(spec, counts):
        if name == "V1":
            items, span = _sequential(rng, name, count, 0.10, 0.06)
        elif kind == VIDEO:
            items = _broll(rng, name, count, span)
        else:
            items, _ = _sequential(rng, name, count, 0.04)
        tracks.append(Track(name=name, kind=kind, items=items))
    tl = Timeline(name=f"case-seed{seed}", fps=FPS, tracks=tracks)
    _inject_defects(tl, rng)
    return tl


def to_v1_dict(tl: Timeline) -> dict:
    fps = tl.fps
    fps_json: object = fps.numerator if fps.denominator == 1 else f"{fps.numerator}/{fps.denominator}"
    tracks = []
    for tr in tl.tracks:
        items = []
        for c in sorted(tr.items, key=lambda c: (c.range.start, c.id)):
            item = {"id": c.id, "type": c.kind, "start": c.range.start,
                    "duration": c.range.duration,
                    "enabled": c.enabled, "muted": c.muted}
            if c.kind == TRANSITION:
                item["left"], item["right"] = c.left, c.right
            items.append(item)
        tracks.append({"name": tr.name, "kind": tr.kind, "items": items})
    return {"version": 1, "name": tl.name,
            "project": {"fps": fps_json}, "tracks": tracks}


_EXACT_RATES = (24, 30000)            # divide the 24 fps grid exactly
_LOOSE_RATES = (24, 25, 60, 30000)    # 25/60 may leave rounding residuals


def _rt(frames: int, rate: int) -> dict:
    return {"rate": rate, "value": round_half_up(Fraction(frames * rate, 24))}


def to_v2_dict(tl: Timeline, seed: int) -> dict:
    rng = random.Random(seed + 1)
    tracks = []
    for tr in tl.tracks:
        items = []
        for c in sorted(tr.items, key=lambda c: (c.range.start, c.id)):
            rates = _LOOSE_RATES if tr.name in ("V2", "V3") else _EXACT_RATES
            rate = rng.choice(rates)
            item = {"type": c.kind, "id": c.id,
                    "range": {"start": _rt(c.range.start, rate),
                              "duration": _rt(c.range.duration, rate)},
                    "enabled": c.enabled, "muted": c.muted}
            if c.kind == TRANSITION:
                item["left"], item["right"] = c.left, c.right
            items.append(item)
        tracks.append({"name": tr.name, "kind": tr.kind, "items": items})
    return {"version": 2, "name": tl.name,
            "project": {"fps": {"rate": tl.fps.denominator,
                                "value": tl.fps.numerator}},
            "tracks": tracks}


def generate(out_dir: str, seed: int, n_clips: int) -> dict:
    tl = build_model(seed, n_clips)
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    files = {}
    for tag, payload in (("v1", to_v1_dict(tl)), ("v2", to_v2_dict(tl, seed))):
        path = out / f"case_{tag}.json"
        path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n",
                        encoding="utf-8")
        files[tag] = str(path)
    return {"seed": seed, "clips": tl.clip_count(), "tracks": len(tl.tracks),
            "files": files}
