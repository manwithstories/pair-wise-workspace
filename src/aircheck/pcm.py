"""Independent PCM verification of lossless audio copies.

Per-segment hashing proves the *compressed* payload survived.  For PCM audio
that claim can be checked one level deeper: decode both sides and compare the
samples themselves.  ``soundfile`` does this decoding independently of PyAV's
demuxer, so a bug that corrupts samples while keeping container bytes intact
would still be caught.

The check writes each side's audio tracks to temporary WAV files via PyAV, then
compares frames in bounded blocks with soundfile.  Files are only created for
assets where every audio stream is PCM (``pcm_*``); compressed audio is compared
at the payload level instead, where bit-exactness is the right claim.
"""

from __future__ import annotations

import tempfile
from dataclasses import dataclass, field
from pathlib import Path

import av
import numpy as np
import soundfile as sf
from av.audio.resampler import AudioResampler

from .probe import ProbeResult

#: Sample differences below this are treated as identical (float round-trip).
SAMPLE_TOLERANCE = 1e-6

#: Block size for streaming comparison; keeps memory at a few hundred KiB.
COMPARE_BLOCK_FRAMES = 8192


@dataclass
class PcmCheck:
    stream_index: int
    frames_compared: int = 0
    max_difference: float = 0.0
    identical: bool = True
    skipped: bool = False
    reason: str = ""

    def to_dict(self) -> dict[str, object]:
        return {
            "stream_index": self.stream_index,
            "frames_compared": self.frames_compared,
            "max_difference": self.max_difference,
            "identical": self.identical,
            "skipped": self.skipped,
            "reason": self.reason,
        }


@dataclass
class PcmReport:
    checks: list[PcmCheck] = field(default_factory=list)
    compared_streams: int = 0
    skipped_streams: int = 0

    @property
    def ok(self) -> bool:
        return all(check.identical for check in self.checks)

    def to_dict(self) -> dict[str, object]:
        return {
            "checks": [check.to_dict() for check in self.checks],
            "compared_streams": self.compared_streams,
            "skipped_streams": self.skipped_streams,
            "ok": self.ok,
        }


def _is_pcm(codec: str) -> bool:
    return codec.startswith("pcm_")


def _extract_stream(path: Path, stream_index: int, destination: Path) -> bool:
    """Decode one audio stream to a WAV file; return False if it is not PCM."""
    container = av.open(str(path))
    try:
        stream = next((s for s in container.streams if s.index == stream_index), None)
        if stream is None:
            return False
        output = av.open(str(destination), "w", format="wav")
        try:
            sample_rate = stream.codec_context.sample_rate
            layout = "stereo" if stream.codec_context.channels == 2 else "mono"
            target = output.add_stream("pcm_s16le", rate=sample_rate)
            target.layout = layout
            resampler = AudioResampler(format="s16", layout=layout, rate=sample_rate)
            frames = []
            for frame in container.decode(stream):
                frames.extend(resampler.resample(frame))
            frames.extend(resampler.resample(None))
            for frame in frames:
                frame.pts = None
                for packet in target.encode(frame):
                    output.mux(packet)
            for packet in target.encode(None):
                output.mux(packet)
        finally:
            output.close()
    finally:
        container.close()
    return True


def compare_pcm(
    source: str | Path,
    output: str | Path,
    source_probe: ProbeResult,
    output_probe: ProbeResult,
    *,
    max_streams: int = 4,
) -> PcmReport:
    """Decode matching PCM audio streams from both files and compare samples."""
    report = PcmReport()
    source_streams = {stream.index: stream for stream in source_probe.audio_streams}
    output_streams = {stream.index: stream for stream in output_probe.audio_streams}
    checked = 0
    with tempfile.TemporaryDirectory(prefix="aircheck-pcm-") as temporary:
        temp = Path(temporary)
        for index, source_stream in sorted(source_streams.items()):
            if checked >= max_streams:
                break
            check = PcmCheck(stream_index=index)
            output_stream = output_streams.get(index)
            if output_stream is None:
                check.skipped = True
                check.reason = "stream missing in output"
                report.checks.append(check)
                report.skipped_streams += 1
                continue
            if not _is_pcm(source_stream.codec) or not _is_pcm(output_stream.codec):
                check.skipped = True
                check.reason = (
                    f"non-PCM audio ({source_stream.codec} -> {output_stream.codec}); "
                    "payload hashing is the applicable check"
                )
                report.checks.append(check)
                report.skipped_streams += 1
                continue
            source_wav = temp / f"source_{index}.wav"
            output_wav = temp / f"output_{index}.wav"
            if not _extract_stream(Path(source), index, source_wav):
                check.skipped = True
                check.reason = "source stream could not be extracted"
                report.checks.append(check)
                report.skipped_streams += 1
                continue
            _extract_stream(Path(output), index, output_wav)
            _compare_wavs(source_wav, output_wav, check)
            checked += 1
            report.compared_streams += 1
            report.checks.append(check)
    return report


def _compare_wavs(source: Path, output: Path, check: PcmCheck) -> None:
    with sf.SoundFile(source) as left, sf.SoundFile(output) as right:
        if left.channels != right.channels or left.samplerate != right.samplerate:
            check.identical = False
            check.reason = (
                f"format changed: {left.channels}ch/{left.samplerate}Hz -> "
                f"{right.channels}ch/{right.samplerate}Hz"
            )
            return
        if len(left) != len(right):
            check.identical = False
            check.reason = f"frame count changed: {len(left)} -> {len(right)}"
            return
        while True:
            a = left.read(COMPARE_BLOCK_FRAMES, dtype="float32", always_2d=True)
            b = right.read(COMPARE_BLOCK_FRAMES, dtype="float32", always_2d=True)
            if len(a) == 0 and len(b) == 0:
                break
            if len(a) != len(b):
                check.identical = False
                check.reason = "block lengths diverged"
                return
            difference = float(np.max(np.abs(a - b))) if a.size else 0.0
            check.max_difference = max(check.max_difference, difference)
            check.frames_compared += len(a)
            if difference > SAMPLE_TOLERANCE:
                check.identical = False
                check.reason = f"sample difference {difference:.6f} exceeds tolerance"
                return


__all__ = [
    "COMPARE_BLOCK_FRAMES",
    "PcmCheck",
    "PcmReport",
    "SAMPLE_TOLERANCE",
    "compare_pcm",
]
