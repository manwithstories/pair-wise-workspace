"""Normalization: sample rate, channel mapping and colour metadata.

Each source is normalized once into a lossless intermediate that carries the
shared clock, so later stages can slice it at exact sample and frame
boundaries:

* audio -> 48 kHz stereo PCM WAV with an exact sample count,
* video -> FFV1/Matroska at the target frame rate and frame size.

Colour metadata conflicts are resolved by an explicit per-track policy, and the
decision plus its sources are recorded for the manifest.
"""

from __future__ import annotations

import os
import struct
import tempfile
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Iterable

import av
import numpy as np
import soundfile as sf

from . import wavio

from .errors import MediaError, MediaLocation, Severity, Stage
from .model import Rate, format_rational
from .probe import ProbeResult, StreamInfo

TARGET_SAMPLE_RATE = 48_000
TARGET_LAYOUT = "stereo"
TARGET_CHANNELS = 2
AUDIO_BLOCK = 1024

COLOR_NAMES = {
    "color_primaries": {
        1: "bt709", 2: "unspecified", 4: "bt470m", 5: "bt470bg",
        6: "smpte170m", 7: "smpte240m", 8: "film", 9: "bt2020",
        10: "smpte428", 11: "smpte431", 12: "smpte432",
    },
    "color_trc": {
        1: "bt709", 2: "unspecified", 4: "gamma22", 5: "gamma28",
        6: "smpte170m", 7: "smpte240m", 8: "linear", 9: "log",
        10: "log_sqrt", 11: "iec61966_2_4", 12: "bt1361",
        13: "iec61966_2_1", 14: "bt2020_10", 15: "bt2020_12",
        16: "smpte2084", 17: "smpte428",
    },
    "colorspace": {
        0: "rgb", 1: "bt709", 2: "unspecified", 4: "fcc", 5: "bt470bg",
        6: "smpte170m", 7: "smpte240m", 8: "ycgco", 9: "bt2020nc", 10: "bt2020c",
    },
    "color_range": {0: "unspecified", 1: "tv", 2: "pc"},
}


def color_name(field_name: str, value: int | None) -> str:
    if value is None:
        return "unspecified"
    return COLOR_NAMES.get(field_name, {}).get(value, f"unknown:{value}")


def color_code(field_name: str, value: Any) -> int | None:
    """Accept an int code or a symbolic name; raise on anything else."""
    if value is None or isinstance(value, int):
        return value
    for code, label in COLOR_NAMES.get(field_name, {}).items():
        if label == value:
            return code
    raise ValueError(f"unknown {field_name} value {value!r}")


@dataclass(frozen=True)
class ColorMetadata:
    primaries: int | None = None
    trc: int | None = None
    colorspace: int | None = None
    range: int | None = None

    @classmethod
    def from_stream(cls, stream: StreamInfo) -> "ColorMetadata":
        return cls(
            primaries=stream.color_primaries,
            trc=stream.color_trc,
            colorspace=stream.colorspace,
            range=stream.color_range,
        )

    def as_dict(self) -> dict[str, Any]:
        return {
            "primaries": color_name("color_primaries", self.primaries),
            "trc": color_name("color_trc", self.trc),
            "colorspace": color_name("colorspace", self.colorspace),
            "range": color_name("color_range", self.range),
        }

    def is_unspecified(self) -> bool:
        return all(v in (None, 2) for v in (self.primaries, self.trc, self.colorspace, self.range))

    def signature(self) -> tuple:
        return (self.primaries, self.trc, self.colorspace, self.range)


@dataclass(frozen=True)
class ColorPolicy:
    """Per-track colour decision policy.

    ``first``   - the first source (by track id) that specifies values wins,
    ``track``   - prefer the named source,
    ``explicit``- use the supplied values verbatim.
    """

    mode: str = "first"
    track: str | None = None
    values: ColorMetadata | None = None

    @classmethod
    def parse(cls, raw: Any) -> "ColorPolicy":
        if raw is None:
            return cls()
        if isinstance(raw, str):
            if raw not in ("first", "track", "explicit"):
                raise ValueError(f"unknown color policy mode {raw!r}")
            return cls(mode=raw)
        if not isinstance(raw, dict):
            raise ValueError("color policy must be a string or object")
        mode = str(raw.get("mode", "first"))
        if mode not in ("first", "track", "explicit"):
            raise ValueError(f"unknown color policy mode {mode!r}")
        values = raw.get("values")
        parsed = None
        if values is not None:
            parsed = ColorMetadata(
                primaries=color_code("color_primaries", values.get("color_primaries")),
                trc=color_code("color_trc", values.get("color_trc")),
                colorspace=color_code("colorspace", values.get("colorspace")),
                range=color_code("color_range", values.get("color_range")),
            )
        return cls(mode=mode, track=raw.get("track"), values=parsed)


@dataclass
class ColorDecision:
    output: ColorMetadata
    sources: dict[str, ColorMetadata]
    policy: ColorPolicy
    conflicts: list[str] = field(default_factory=list)
    decided_by: str = "default"

    def to_dict(self) -> dict[str, Any]:
        return {
            "policy": {
                "mode": self.policy.mode,
                "track": self.policy.track,
                "values": self.policy.values.as_dict() if self.policy.values else None,
            },
            "output": self.output.as_dict(),
            "sources": {name: meta.as_dict() for name, meta in sorted(self.sources.items())},
            "conflicts": list(self.conflicts),
            "decided_by": self.decided_by,
        }


def resolve_color(
    track_id: str, sources: dict[str, ColorMetadata], policy: ColorPolicy
) -> ColorDecision:
    """Resolve colour metadata for one track and record the reasoning."""
    usable = {name: meta for name, meta in sources.items() if not meta.is_unspecified()}
    conflicts: list[str] = []
    if len({meta.signature() for meta in usable.values()}) > 1:
        conflicts.append(
            "sources disagree: "
            + ", ".join(f"{name}={meta.as_dict()}" for name, meta in sorted(usable.items()))
        )
    if policy.mode == "explicit" and policy.values is not None:
        output, decided_by = policy.values, "policy:explicit"
    elif policy.mode == "track" and policy.track and policy.track in usable:
        output, decided_by = usable[policy.track], f"policy:track:{policy.track}"
    elif usable:
        first_name = sorted(usable)[0]
        output, decided_by = usable[first_name], f"policy:first:{first_name}"
    else:
        output, decided_by = ColorMetadata(1, 1, 1, 1), "fallback:bt709"
    _ = track_id
    return ColorDecision(
        output=output,
        sources=dict(sorted(sources.items())),
        policy=policy,
        conflicts=conflicts,
        decided_by=decided_by,
    )


@dataclass(frozen=True)
class VideoSpec:
    width: int
    height: int
    rate: Rate

    @property
    def frame_duration(self) -> Fraction:
        return self.rate.frame_duration


@dataclass(frozen=True)
class AudioSpec:
    sample_rate: int = TARGET_SAMPLE_RATE
    layout: str = TARGET_LAYOUT
    channels: int = TARGET_CHANNELS


@dataclass(frozen=True)
class NormalizedVideo:
    path: str
    spec: VideoSpec
    frames: int
    color: ColorDecision | None = None


@dataclass(frozen=True)
class NormalizedAudio:
    path: str
    spec: AudioSpec
    samples: int


@dataclass
class NormalizeOutcome:
    video: NormalizedVideo | None = None
    audio: NormalizedAudio | None = None
    errors: list[MediaError] = field(default_factory=list)


def atomic_temp(directory: str, suffix: str) -> str:
    os.makedirs(directory, exist_ok=True)
    handle, path = tempfile.mkstemp(prefix=".part-", suffix=suffix, dir=directory)
    os.close(handle)
    return path


def safe_remove(path: str) -> None:
    try:
        os.unlink(path)
    except OSError:
        pass


def write_silent_wav(path: str, samples: int, spec: AudioSpec = AudioSpec()) -> None:
    """Write an exact-length silence WAV (used for empty timeline regions)."""
    data = np.zeros((samples, spec.channels), dtype=np.float32)
    wavio.write_wav(path, data, spec.sample_rate, subtype="PCM_16")


def normalize_audio_from_frames(
    frames: Iterable[av.AudioFrame],
    source_rate: int,
    source_layout: str,
    out_dir: str,
    track_id: str,
    spec: AudioSpec = AudioSpec(),
) -> tuple[NormalizedAudio | None, MediaError | None]:
    """Resample a decoded audio frame sequence to ``spec`` and write a WAV."""
    resampler = av.AudioResampler(format="fltp", layout=spec.layout, rate=spec.sample_rate)
    chunks: list[np.ndarray] = []
    try:
        for frame in frames:
            if frame.sample_rate != source_rate:
                frame.sample_rate = source_rate
            for out in resampler.resample(frame):
                chunks.append(out.to_ndarray())
        for out in resampler.resample(None):
            chunks.append(out.to_ndarray())
    except (av.error.FFmpegError, ValueError, OSError) as exc:
        return None, MediaError(
            code="resample_failed",
            stage=Stage.NORMALIZE,
            message=f"resampling {source_layout!r}@{source_rate} failed: {exc}",
            location=MediaLocation(track=track_id, sample_start=0),
            severity=Severity.ERROR,
            context={"layout": source_layout, "sample_rate": source_rate},
        )
    if not chunks:
        return None, MediaError(
            code="empty_audio",
            stage=Stage.NORMALIZE,
            message="resampler produced zero samples",
            location=MediaLocation(track=track_id, sample_start=0, sample_end=0),
            severity=Severity.ERROR,
        )
    data = np.concatenate(chunks, axis=1)
    path = os.path.join(out_dir, f"{track_id}.audio.wav")
    try:
        wavio.write_wav(path, np.ascontiguousarray(data.T), spec.sample_rate, subtype="PCM_16")
    except OSError as exc:
        return None, MediaError(
            code="audio_write_failed",
            stage=Stage.NORMALIZE,
            message=f"cannot write normalized audio: {exc}",
            location=MediaLocation(track=track_id),
            severity=Severity.ERROR,
        )
    return NormalizedAudio(path=path, spec=spec, samples=int(data.shape[1])), None


def normalize_audio(
    probe: ProbeResult, out_dir: str, track_id: str, spec: AudioSpec = AudioSpec()
) -> tuple[NormalizedAudio | None, MediaError | None]:
    """Decode and normalize the first audio stream of ``probe``."""
    stream = probe.audio()
    if stream is None:
        return None, None
    container = av.open(probe.path)
    try:
        frames = [f for f in container.decode(audio=0) if isinstance(f, av.AudioFrame)]
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        return None, MediaError(
            code="audio_decode_failed",
            stage=Stage.NORMALIZE,
            message=f"cannot decode audio: {exc}",
            location=MediaLocation(track=track_id, sample_start=0, stream_index=stream.index),
            severity=Severity.ERROR,
        )
    finally:
        container.close()
    audio, error = normalize_audio_from_frames(
        frames,
        stream.sample_rate or TARGET_SAMPLE_RATE,
        stream.layout or TARGET_LAYOUT,
        out_dir,
        track_id,
        spec,
    )
    if error is not None or stream.declared_samples() is None:
        return audio, error
    declared = stream.declared_samples()
    source_rate = stream.sample_rate or TARGET_SAMPLE_RATE
    target_declared = int(round(Fraction(declared) * spec.sample_rate / source_rate))
    if audio is not None and target_declared and audio.samples > target_declared:
        return _trim_audio(audio, target_declared, out_dir, track_id, spec), None
    return audio, error


def _trim_audio(
    audio: NormalizedAudio, samples: int, out_dir: str, track_id: str, spec: AudioSpec
) -> NormalizedAudio:
    """Trim encoder padding down to the declared sample count."""
    with sf.SoundFile(audio.path) as handle:
        data = handle.read(frames=samples, dtype="float32", always_2d=True)
    path = os.path.join(out_dir, f"{track_id}.audio.wav")
    wavio.write_wav(path, data, spec.sample_rate, subtype="PCM_16")
    return NormalizedAudio(path=path, spec=spec, samples=samples)


def read_wav_span(path: str, start: int, end: int) -> np.ndarray:
    """Read ``[start, end)`` samples as float32 shaped ``(channels, n)``."""
    with sf.SoundFile(path) as handle:
        handle.seek(start)
        data = handle.read(frames=max(0, end - start), dtype="float32", always_2d=True)
    return np.ascontiguousarray(data.T)


def audio_sample_count(path: str) -> int:
    return int(sf.info(path).frames)


def wav_header_frames(path: str) -> int:
    """Read the sample count from a WAV header without decoding the payload."""
    with open(path, "rb") as handle:
        header = handle.read(4096)
    if len(header) < 44 or header[:4] != b"RIFF" or header[8:12] != b"WAVE":
        raise ValueError(f"{path} is not a RIFF/WAVE file")
    channels, rate = struct.unpack("<HI", header[22:28])
    bits = struct.unpack("<H", header[34:36])[0]
    block = max(1, channels * (bits // 8))
    data_index = header.find(b"data", 12)
    if data_index < 0 or len(header) < data_index + 8:
        raise ValueError(f"{path} has no data chunk")
    declared = struct.unpack("<I", header[data_index + 4 : data_index + 8])[0]
    available = os.path.getsize(path) - (data_index + 8)
    return min(declared, available) // block


def describe_duration(samples: int, rate: int = TARGET_SAMPLE_RATE) -> str:
    return format_rational(Fraction(samples, rate))


def apply_color_metadata(codec_context: Any, meta: ColorMetadata) -> None:
    """Copy the resolved colour metadata onto an encoder context."""
    if meta.primaries is not None:
        codec_context.color_primaries = meta.primaries
    if meta.trc is not None:
        codec_context.color_trc = meta.trc
    if meta.colorspace is not None:
        codec_context.colorspace = meta.colorspace
    if meta.range is not None:
        codec_context.color_range = meta.range


def normalize_video(
    probe: ProbeResult,
    out_dir: str,
    track_id: str,
    spec: VideoSpec,
    color: ColorDecision | None,
) -> tuple[NormalizedVideo | None, MediaError | None]:
    """Transcode the source video to lossless FFV1 at ``spec``."""
    stream = probe.video()
    if stream is None:
        return None, None
    path = os.path.join(out_dir, f"{track_id}.video.mkv")
    temp = atomic_temp(out_dir, ".mkv")
    count = 0
    try:
        container = av.open(temp, "w", format="matroska", options={"fflags": "+bitexact"})
        try:
            out_stream = container.add_stream("ffv1", rate=spec.rate.value)
            out_stream.width = spec.width
            out_stream.height = spec.height
            out_stream.pix_fmt = "yuv420p"
            out_stream.time_base = spec.frame_duration
            out_stream.options = {
                "level": "3",
                "slices": "4",
                "threads": "1",
                "slicecrc": "1",
            }
            codec_context = out_stream.codec_context
            if color is not None:
                apply_color_metadata(codec_context, color.output)
            limit = stream.declared_frames() or 0
            source = av.open(probe.path)
            try:
                for frame in source.decode(video=0):
                    if limit and count >= limit:
                        break
                    converted = frame.reformat(
                        width=spec.width, height=spec.height, format="yuv420p"
                    )
                    converted.pts = count
                    converted.time_base = spec.frame_duration
                    for packet in out_stream.encode(converted):
                        container.mux(packet)
                    count += 1
            finally:
                source.close()
            for packet in out_stream.encode():
                container.mux(packet)
        finally:
            container.close()
        os.replace(temp, path)
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        safe_remove(temp)
        return None, MediaError(
            code="video_normalize_failed",
            stage=Stage.NORMALIZE,
            message=f"cannot normalize video: {exc}",
            location=MediaLocation(track=track_id, stream_index=stream.index),
            severity=Severity.ERROR,
        )
    return NormalizedVideo(path=path, spec=spec, frames=count, color=color), None


def normalize_track(
    probe: ProbeResult,
    out_dir: str,
    track_id: str,
    video_spec: VideoSpec | None,
    color: ColorDecision | None,
) -> NormalizeOutcome:
    """Normalize both streams of one source file."""
    outcome = NormalizeOutcome()
    if video_spec is not None and probe.video() is not None:
        video, error = normalize_video(probe, out_dir, track_id, video_spec, color)
        if error is not None:
            outcome.errors.append(error)
        else:
            outcome.video = video
    if probe.audio() is not None:
        audio, error = normalize_audio(probe, out_dir, track_id)
        if error is not None:
            outcome.errors.append(error)
        else:
            outcome.audio = audio
    return outcome
