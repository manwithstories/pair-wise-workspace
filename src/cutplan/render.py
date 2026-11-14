from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
from pathlib import Path
from typing import Any

from PIL import Image, ImageDraw

from .errors import CutplanError
from .model import Timeline


@dataclass(frozen=True, slots=True)
class FramePlan:
    index: int
    active: tuple[tuple[str, str], ...]

    @property
    def gap(self) -> bool:
        return not self.active

    def as_dict(self) -> dict[str, Any]:
        return {"frame": self.index, "active": [list(item) for item in self.active], "gap": self.gap}


def build_frame_plan(timeline: Timeline) -> list[FramePlan]:
    tracks = sorted(timeline.tracks, key=lambda item: item.id)
    ordered = [
        (track.id, clip)
        for track in tracks
        for clip in sorted(track.clips, key=lambda item: item.id)
    ]
    frames: list[FramePlan] = []
    for frame in range(timeline.output_duration_frames):
        active = tuple(
            (track_id, clip.id)
            for track_id, clip in ordered
            if clip.duration > 0 and clip.start <= frame < clip.end
        )
        frames.append(FramePlan(frame, active))
    return frames


def _plan_digest(frames: list[FramePlan]) -> str:
    payload = json.dumps(
        [frame.as_dict() for frame in frames],
        ensure_ascii=True,
        sort_keys=True,
        separators=(",", ":"),
    )
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def _color(clip_id: str) -> tuple[int, int, int]:
    digest = hashlib.sha256(clip_id.encode("utf-8")).digest()
    return (48 + digest[0] % 176, 48 + digest[1] % 176, 48 + digest[2] % 176)


def _draw_frame(frame: FramePlan, track_ids: list[str], width: int, height: int) -> Image.Image:
    image = Image.new("RGB", (width, height), (14, 16, 22))
    draw = ImageDraw.Draw(image)
    if not track_ids:
        return image
    rows = min(len(track_ids), max(1, height))
    row_height = max(1, height // rows)
    positions = {track_id: index for index, track_id in enumerate(track_ids)}
    for track_id, clip_id in frame.active:
        row = positions.get(track_id)
        if row is None or row >= rows:
            continue
        top = row * row_height
        bottom = height - 1 if row == rows - 1 else min(height - 1, top + row_height - 1)
        draw.rectangle((0, top, width - 1, bottom), fill=_color(clip_id))
        draw.line((0, top, width - 1, top), fill=(232, 236, 244))
    return image


def render_timeline(
    timeline: Timeline,
    out_dir: str | Path,
    *,
    width: int = 64,
    height: int = 36,
) -> dict[str, Any]:
    if not isinstance(width, int) or isinstance(width, bool) or width <= 0:
        raise CutplanError("E_RENDER_SIZE", "width must be a positive integer")
    if not isinstance(height, int) or isinstance(height, bool) or height <= 0:
        raise CutplanError("E_RENDER_SIZE", "height must be a positive integer")
    destination = Path(out_dir)
    destination.mkdir(parents=True, exist_ok=True)
    for stale in destination.glob("frame_*.png"):
        stale.unlink()
    frames = build_frame_plan(timeline)
    track_ids = sorted(track.id for track in timeline.tracks)
    hashes: list[str] = []
    for frame in frames:
        image = _draw_frame(frame, track_ids, width, height)
        path = destination / f"frame_{frame.index:06d}.png"
        image.save(path, format="PNG", compress_level=9)
        hashes.append(hashlib.sha256(path.read_bytes()).hexdigest())
    first_hash = hashes[0] if hashes else hashlib.sha256(b"").hexdigest()
    digest = hashlib.sha256("".join(hashes).encode("ascii")).hexdigest()
    plan_hash = _plan_digest(frames)
    gap_frames = sum(1 for frame in frames if frame.gap)
    if len(hashes) != timeline.output_duration_frames:
        raise CutplanError("E_RENDER_SELF_CHECK", "frame count mismatch", exit_code=5)
    return {
        "ok": True,
        "frame_count": len(frames),
        "width": width,
        "height": height,
        "gap_frames": gap_frames,
        "first_hash": first_hash,
        "digest": digest,
        "plan_digest": plan_hash,
        "output_dir": str(destination),
    }


render = render_timeline
