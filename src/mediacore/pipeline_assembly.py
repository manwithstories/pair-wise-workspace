"""Assemble per-clip segments into the final track.

Within a track clips are ordered by start time.  A licensed overlap is
replaced by the transition's composite output: the left clip contributes
everything before the blend, the blend follows, and the right clip contributes
everything after its head.  The assembled length therefore equals the union of
the clips' ranges — no gap, no double-counting.

All sample arithmetic goes through :func:`samples_for_duration` so a boundary
lands on the same sample regardless of how the segments were produced.
"""

from __future__ import annotations

from pathlib import Path

import numpy as np

from .audio_io import file_sha256, read_wav, write_wav
from .audio_model import AudioBuffer
from .errors import OutputError
from .timebase import samples_for_duration
from .timeline import Timeline

__all__ = ["assemble_output", "assemble_track", "safe_name"]


def assemble_output(
    timeline: Timeline,
    outcomes: dict,
    *,
    output_path: Path,
    sample_rate: int,
    layout: str,
) -> tuple[Path, str, int]:
    """Concatenate all tracks' assembled audio and write ``output.wav``."""
    track_ids = sorted({c.track for c in timeline.clips.values()})
    assembled: list[AudioBuffer] = []
    for track_id in track_ids:
        buffer = assemble_track(
            timeline, track_id, outcomes, sample_rate=sample_rate, layout=layout
        )
        if buffer.frames > 0:
            assembled.append(buffer)
    if not assembled:
        raise OutputError("nothing to assemble", path=str(output_path))
    combined = _concat(assembled)
    write_wav(output_path, combined)
    return output_path, file_sha256(output_path), combined.frames


def assemble_track(
    timeline: Timeline,
    track_id: str,
    outcomes: dict,
    *,
    sample_rate: int,
    layout: str,
) -> AudioBuffer:
    clips = sorted(
        (c for c in timeline.clips.values() if c.track == track_id),
        key=lambda c: (c.start_us, c.id),
    )
    transitions = {
        frozenset((t.left, t.right)): t for t in timeline.transitions.values()
    }
    pieces: list[np.ndarray] = []
    index = 0
    while index < len(clips):
        clip = clips[index]
        next_clip = clips[index + 1] if index + 1 < len(clips) else None
        if next_clip is not None:
            transition = transitions.get(frozenset((clip.id, next_clip.id)))
            overlap = (
                min(clip.end_us, next_clip.end_us)
                - max(clip.start_us, next_clip.start_us)
                if transition is not None
                else 0
            )
            if transition is not None and overlap > 0:
                lead = read_wav(outcomes[f"clip:{clip.id}"].path)
                keep = samples_for_duration(clip.duration_us - transition.duration_us, sample_rate)
                if keep > 0:
                    pieces.append(lead.samples[:, :keep])
                blend = read_wav(outcomes[transition.node_id].path)
                pieces.append(blend.samples)
                right = read_wav(outcomes[f"clip:{next_clip.id}"].path)
                drop = samples_for_duration(transition.duration_us, sample_rate)
                pieces.append(right.samples[:, drop:])
                index += 2
                continue
        buffer = read_wav(outcomes[f"clip:{clip.id}"].path)
        pieces.append(buffer.samples)
        index += 1
    channel_count = 1 if layout == "mono" else 2
    if not pieces:
        return AudioBuffer(
            samples=np.zeros((channel_count, 0), dtype=np.float32),
            sample_rate=sample_rate,
            layout=layout,  # type: ignore[arg-type]
        )
    data = np.concatenate(pieces, axis=1)
    return AudioBuffer(
        samples=np.ascontiguousarray(data, dtype=np.float32),
        sample_rate=sample_rate,
        layout=layout,  # type: ignore[arg-type]
    )


def _concat(buffers: list[AudioBuffer]) -> AudioBuffer:
    rate = buffers[0].sample_rate
    layout = buffers[0].layout
    for buf in buffers[1:]:
        if buf.sample_rate != rate or buf.layout != layout:
            raise OutputError(
                "assembled tracks differ in format",
                expected_rate=rate,
                got_rate=buf.sample_rate,
                expected_layout=layout,
                got_layout=buf.layout,
            )
    data = np.concatenate([b.samples for b in buffers], axis=1)
    return AudioBuffer(
        samples=np.ascontiguousarray(data, dtype=np.float32),
        sample_rate=rate,
        layout=layout,
    )


def safe_name(name: str) -> str:
    return "".join(ch if ch.isalnum() or ch in "-_." else "_" for ch in name)
