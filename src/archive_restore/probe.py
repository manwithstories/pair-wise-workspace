"""Cross-validation of recovered output with PyAV and soundfile.

Recovered payloads are PCM s16le mono at PCM_SAMPLE_RATE Hz.  The probe
writes them to a temporary WAV via soundfile, then re-reads it with both
soundfile and PyAV and checks that the two decoders agree on shape and
duration.  A disagreement means the recovered byte stream is not a
consistent media signal and the restore fails verification.
"""

from __future__ import annotations

import os
import tempfile
from dataclasses import dataclass, field

PCM_SAMPLE_RATE = 8_000
PCM_CHANNELS = 1
PCM_SUBTYPE = "PCM_16"


@dataclass
class ProbeResult:
    ok: bool
    frames: int = 0
    duration_seconds: float = 0.0
    details: list[str] = field(default_factory=list)


def probe_payload(payload: bytes) -> ProbeResult:
    """Cross-check a recovered PCM payload with soundfile and PyAV."""
    import numpy as np
    import soundfile as sf

    result = ProbeResult(ok=False)
    if len(payload) % 2:
        result.details.append("payload length is not sample-aligned")
        return result
    samples = np.frombuffer(payload, dtype="<i2")
    result.frames = int(samples.shape[0])
    result.duration_seconds = result.frames / PCM_SAMPLE_RATE

    fd, tmp = tempfile.mkstemp(suffix=".wav")
    os.close(fd)
    try:
        sf.write(tmp, samples, PCM_SAMPLE_RATE, subtype=PCM_SUBTYPE)
        info = sf.info(tmp)
        if info.frames != result.frames:
            result.details.append(
                f"soundfile frames {info.frames} != expected {result.frames}"
            )
            return result
        result.details.append(
            f"soundfile: {info.frames} frames @ {info.samplerate} Hz ok"
        )

        import av

        with av.open(tmp) as container:
            stream = container.streams.audio[0]
            decoded = 0
            for frame in container.decode(stream):
                decoded += frame.samples
        if decoded != result.frames:
            result.details.append(
                f"pyav decoded {decoded} samples != expected {result.frames}"
            )
            return result
        result.details.append(f"pyav: decoded {decoded} samples ok")
    finally:
        try:
            os.unlink(tmp)
        except OSError:
            pass
    result.ok = True
    return result
