"""Audio decoding, resampling and clip rendering.

Boundaries: a clip's duration in microseconds is converted to an integer
sample count once, and the renderer must emit exactly that many samples.
Tests assert the count; the pipeline rejects a segment whose count differs.

Memory: renders are two-pass and streaming.  Pass one measures loudness and
peak, pass two applies the static gain and writes; peak memory is one second
of audio plus the loudness window, independent of clip length.
"""

from __future__ import annotations

from pathlib import Path
from typing import Iterator

import numpy as np

from .audio_io import file_sha256
from .audio_loudness import (
    CHUNK_SAMPLES,
    DEFAULT_CEILING_DBFS,
    LoudnessMeter,
    _stats_for,
)
from .audio_model import RenderResult
from .errors import AudioError
from .timebase import samples_for_duration

def _av():
    import av

    return av


def _stream_by_index(container, stream_index: int | None):
    streams = list(container.streams.audio)
    if not streams:
        raise AudioError("no audio stream", path=container.name)
    if stream_index is None:
        return streams[0]
    for stream in streams:
        if stream.index == stream_index:
            return stream
    raise AudioError(
        "audio stream index not found", path=container.name, stream_index=stream_index
    )


def iter_resampled(
    path: str | Path,
    *,
    stream_index: int | None,
    sample_rate: int,
    layout: Layout,
    frame_samples: int = CHUNK_SAMPLES,
) -> Iterator[np.ndarray]:
    """Yield planar float32 blocks at ``sample_rate`` / ``layout``.

    Decoding always starts at the stream head and the resampler is flushed at
    EOF, so the concatenation of all blocks is the full decoded track.  Clip
    boundaries are applied by the caller as absolute sample offsets; this
    keeps the mapping from microseconds to samples identical between the
    measurement pass and the write pass.
    """
    av = _av()
    resampler = av.AudioResampler(
        format="fltp",
        layout=av.AudioLayout(layout),
        rate=sample_rate,
    )

    def _emit(frame) -> np.ndarray:
        data = frame.to_ndarray()  # (channels, n) float32 for fltp
        return np.ascontiguousarray(data, dtype=np.float32)

    try:
        container = av.open(str(path))
    except Exception as exc:
        raise AudioError(
            "cannot open audio", path=str(path), reason=str(exc)[:200]
        ) from exc
    try:
        stream = _stream_by_index(container, stream_index)
        try:
            for frame in container.decode(stream):
                for out in resampler.resample(frame):
                    yield _emit(out)
        except Exception as exc:
            raise AudioError(
                "audio decode failed",
                path=str(path),
                stream_index=stream.index,
                reason=str(exc)[:200],
            ) from exc
        try:
            for out in resampler.resample(None):
                yield _emit(out)
        except Exception as exc:
            raise AudioError(
                "audio resampler flush failed", path=str(path), reason=str(exc)[:200]
            ) from exc
    finally:
        container.close()


def _slice_iter(
    blocks: Iterator[np.ndarray], start_sample: int, count: int
) -> Iterator[np.ndarray]:
    """Yield exactly ``count`` samples beginning at absolute ``start_sample``."""
    cursor = 0
    remaining = count
    for block in blocks:
        n = block.shape[1]
        block_start, block_end = cursor, cursor + n
        cursor = block_end
        if block_end <= start_sample:
            continue
        lo = max(0, start_sample - block_start)
        hi = min(n, start_sample + count - block_start)
        if hi <= lo:
            continue
        piece = block[:, lo:hi]
        if piece.shape[1] > remaining:
            piece = piece[:, :remaining]
        if piece.shape[1] == 0:
            continue
        remaining -= piece.shape[1]
        yield piece
        if remaining <= 0:
            return








def render_clip(
    source: str | Path,
    output: str | Path,
    *,
    stream_index: int | None,
    start_us: int,
    end_us: int,
    sample_rate: int,
    layout: Layout,
    target_lufs: float | None,
    ceiling_dbfs: float = DEFAULT_CEILING_DBFS,
    subtype: str = "PCM_16",
) -> RenderResult:
    """Render one clip to WAV with exact sample count and loudness control.

    ``samples_for_duration(end_us - start_us)`` is authoritative: if the
    decoder cannot supply that many samples (source shorter than the clip) the
    render fails with a locatable :class:`AudioError` rather than silently
    writing a short segment.
    """
    if end_us <= start_us:
        raise AudioError(
            "empty audio clip", start_us=start_us, end_us=end_us, path=str(source)
        )
    expected = samples_for_duration(end_us - start_us, sample_rate)
    if expected <= 0:
        raise AudioError(
            "clip is shorter than one sample",
            start_us=start_us,
            end_us=end_us,
            sample_rate=sample_rate,
        )
    start_sample = samples_for_duration(start_us, sample_rate)

    # Pass 1: measure.  Blocks are coalesced to ~1 s before filtering so the
    # FFT convolution is amortised; memory stays bounded by one second of
    # audio plus the loudness window.
    meter = LoudnessMeter(sample_rate, 1 if layout == "mono" else 2)
    available = 0
    coalesce = max(1, sample_rate)
    pending: list[np.ndarray] = []
    pending_samples = 0

    def _flush_measure() -> None:
        nonlocal pending, pending_samples
        if not pending:
            return
        meter.feed(np.concatenate(pending, axis=1) if len(pending) > 1 else pending[0])
        pending = []
        pending_samples = 0

    for piece in _slice_iter(
        iter_resampled(
            source,
            stream_index=stream_index,
            sample_rate=sample_rate,
            layout=layout,
        ),
        start_sample,
        expected,
    ):
        pending.append(piece)
        pending_samples += piece.shape[1]
        available += piece.shape[1]
        if pending_samples >= coalesce:
            _flush_measure()
    _flush_measure()
    if available < expected:
        raise AudioError(
            "source ended before the clip boundary",
            path=str(source),
            stream_index=stream_index,
            start_us=start_us,
            end_us=end_us,
            expected_samples=expected,
            available_samples=available,
            shortfall_samples=expected - available,
        )
    lufs, peak, gated, measurement = meter.result()
    stats = _stats_for(
        lufs, peak, gated, measurement,
        target_lufs=target_lufs, ceiling_dbfs=ceiling_dbfs,
    )
    gain = 10.0 ** (stats.gain_db / 20.0) if stats.gain_db else 1.0

    # Pass 2: write the gained signal, block by block.
    import soundfile as sf

    output = Path(output)
    output.parent.mkdir(parents=True, exist_ok=True)
    written = 0
    with sf.SoundFile(
        str(output),
        mode="w",
        samplerate=sample_rate,
        channels=1 if layout == "mono" else 2,
        subtype=subtype,
        format="WAV",
    ) as handle:
        for piece in _slice_iter(
            iter_resampled(
                source,
                stream_index=stream_index,
                sample_rate=sample_rate,
                layout=layout,
            ),
            start_sample,
            expected,
        ):
            gained = piece if gain == 1.0 else (piece * gain)
            handle.write(gained.T)
            written += gained.shape[1]
    if written != expected:
        raise AudioError(
            "rendered sample count mismatch",
            path=str(output),
            expected_samples=expected,
            written_samples=written,
            delta=written - expected,
        )
    return RenderResult(
        path=output,
        samples=written,
        sample_rate=sample_rate,
        layout=layout,
        sha256=file_sha256(output),
        loudness=stats,
    )





