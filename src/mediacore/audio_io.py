"""WAV IO, hashing and transition blending.

WAV is written through libsndfile with a fixed subtype and no timestamp
metadata, so two renders of the same input are byte-identical (asserted in
tests and relied on by the resume digest).
"""

from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Literal

import numpy as np

from .audio_loudness import measure_loudness
from .audio_model import AudioBuffer, RenderResult
from .errors import AudioError
from .timebase import samples_for_duration

def write_wav(path: str | Path, buffer: AudioBuffer, *, subtype: str = "PCM_16") -> Path:
    """Write a WAV deterministically (no timestamps, fixed layout)."""
    import soundfile as sf

    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    data = buffer.samples.T
    sf.write(str(path), data, buffer.sample_rate, subtype=subtype, format="WAV")
    return path


def read_wav(path: str | Path) -> AudioBuffer:
    import soundfile as sf

    data, sample_rate = sf.read(str(path), dtype="float32", always_2d=True)
    samples = np.ascontiguousarray(data.T)
    layout: Layout = "mono" if samples.shape[0] == 1 else "stereo"
    return AudioBuffer(samples=samples, sample_rate=int(sample_rate), layout=layout)


def file_sha256(path: str | Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as fh:
        while block := fh.read(1 << 20):
            digest.update(block)
    return digest.hexdigest()


def crossfade(
    left: AudioBuffer,
    right: AudioBuffer,
    *,
    curve: Literal["linear", "equal_power"] = "linear",
) -> AudioBuffer:
    """Blend two equal-length buffers over their full length."""
    if left.frames != right.frames:
        raise AudioError(
            "crossfade operands differ in length",
            left_samples=left.frames,
            right_samples=right.frames,
        )
    if left.sample_rate != right.sample_rate or left.layout != right.layout:
        raise AudioError(
            "crossfade operands differ in format",
            left_rate=left.sample_rate,
            right_rate=right.sample_rate,
            left_layout=left.layout,
            right_layout=right.layout,
        )
    n = left.frames
    t = np.linspace(0.0, 1.0, n, endpoint=False, dtype=np.float32)
    if curve == "equal_power":
        fade_in = np.sin(t * np.pi / 2.0)
        fade_out = np.cos(t * np.pi / 2.0)
    else:
        fade_in = t
        fade_out = 1.0 - t
    mixed = left.samples * fade_out + right.samples * fade_in
    return AudioBuffer(
        samples=np.ascontiguousarray(mixed, dtype=np.float32),
        sample_rate=left.sample_rate,
        layout=left.layout,
    )


def render_blend(
    left_path: str | Path,
    right_path: str | Path,
    output: str | Path,
    *,
    duration_us: int,
    sample_rate: int,
    curve: Literal["linear", "equal_power"] = "linear",
    subtype: str = "PCM_16",
) -> RenderResult:
    """Render a transition as a composite of its two rendered clips.

    Both operands are read at ``sample_rate``; the blend takes the **last**
    ``duration_us`` of the left clip and the **first** ``duration_us`` of the
    right one.  The output length is exactly
    ``samples_for_duration(duration_us)`` so the assembly step can splice it
    in without a single sample of drift.
    """
    left = read_wav(left_path)
    right = read_wav(right_path)
    for name, buf in (("left", left), ("right", right)):
        if buf.sample_rate != sample_rate:
            raise AudioError(
                "blend operand sample rate mismatch",
                operand=name,
                path=str(left_path if name == "left" else right_path),
                expected=sample_rate,
                got=buf.sample_rate,
            )
    count = samples_for_duration(duration_us, sample_rate)
    if count <= 0:
        raise AudioError("transition shorter than one sample", duration_us=duration_us)
    if left.frames < count or right.frames < count:
        raise AudioError(
            "clip shorter than its transition",
            transition_samples=count,
            left_samples=left.frames,
            right_samples=right.frames,
        )
    tail = AudioBuffer(left.samples[:, -count:], sample_rate, left.layout)
    head = AudioBuffer(right.samples[:, :count], sample_rate, right.layout)
    if tail.layout != head.layout:
        raise AudioError(
            "blend operands differ in channel layout",
            left_layout=tail.layout,
            right_layout=head.layout,
        )
    mixed = crossfade(tail, head, curve=curve)
    write_wav(output, mixed, subtype=subtype)
    # Loudness of a blend is reported but not re-normalised: the operands
    # were already normalised to the target, and normalising the transition
    # again would create an audible level step at the splice points.
    stats = measure_loudness(mixed)
    return RenderResult(
        path=Path(output),
        samples=mixed.frames,
        sample_rate=sample_rate,
        layout=mixed.layout,
        sha256=file_sha256(output),
        loudness=stats,
    )

