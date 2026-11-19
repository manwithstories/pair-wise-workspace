"""Mapping: bind camera video tracks and an optional WAV companion track.

The plan is validated up front: unknown source tracks, overlapping
mappings (two outputs claiming one input, or reused output names) and
sample-rate conversions that would overflow the nanosecond base are all
rejected before any frame moves.
"""
from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from pathlib import Path

from .model import INT64_MAX, NS_PER_SECOND, FieldPackError, Stage, TrackInfo


def _err(code: str, msg: str) -> FieldPackError:
    return FieldPackError(code=code, message=msg, stage=Stage.TRANSFORM)


def audio_duration_ns(frames: int, sample_rate: int) -> int:
    """frames * 1e9 / sample_rate with explicit int64 overflow rejection."""
    if sample_rate <= 0:
        raise _err("invalid-sample-rate", f"sample rate {sample_rate} not positive")
    num = frames * NS_PER_SECOND
    if num > INT64_MAX:
        raise _err(
            "duration-overflow",
            f"{frames} frames @ {sample_rate} Hz overflow the ns time base",
        )
    return num // sample_rate


@dataclass
class AudioTrack:
    path: str
    sample_rate: int
    channels: int
    frames: int
    duration_ns: int
    sha256: str  # digest of the PCM payload, joins the validation flow

    def summary(self) -> dict:
        return {
            "kind": "audio",
            "path": self.path,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "frames": self.frames,
            "duration_ns": self.duration_ns,
            "sha256": self.sha256,
        }


def summarise_audio(path: str | Path) -> AudioTrack:
    """Read a WAV companion track and digest its PCM into the map plan."""
    import soundfile as sf  # lazy: optional dependency surface

    path = Path(path)
    if not path.is_file():
        raise _err("audio-not-found", f"audio file {path} does not exist")
    with sf.SoundFile(str(path)) as snd:
        rate, channels, frames = snd.samplerate, snd.channels, snd.frames
        digest = hashlib.sha256()
        while True:
            block = snd.read(65536, dtype="float32", always_2d=True)
            if block.size == 0:
                break
            digest.update(block.tobytes())
    return AudioTrack(
        path=str(path),
        sample_rate=rate,
        channels=channels,
        frames=frames,
        duration_ns=audio_duration_ns(frames, rate),
        sha256=digest.hexdigest(),
    )


@dataclass
class VideoMapping:
    output: str
    device_id: int

    def summary(self, info: TrackInfo) -> dict:
        return {
            "kind": "video",
            "output": self.output,
            "device_id": self.device_id,
            "pixfmt": info.pixfmt,
            "width": info.width,
            "height": info.height,
            "records": info.record_count,
        }


@dataclass
class MapPlan:
    videos: list[VideoMapping] = field(default_factory=list)
    audio: AudioTrack | None = None

    def device_for(self, device_id: int) -> VideoMapping | None:
        for vm in self.videos:
            if vm.device_id == device_id:
                return vm
        return None

    def summary(self, tracks: dict[int, TrackInfo]) -> dict:
        return {
            "videos": [vm.summary(tracks[vm.device_id]) for vm in self.videos],
            "audio": self.audio.summary() if self.audio else None,
        }


def build_plan(
    video_specs: list[tuple[str, int]],
    audio_path: str | Path | None,
    tracks: dict[int, TrackInfo],
) -> MapPlan:
    """Validate CLI mapping specs against probed source tracks."""
    seen_devices: set[int] = set()
    seen_outputs: set[str] = set()
    videos: list[VideoMapping] = []
    for output, device_id in video_specs:
        if device_id not in tracks:
            raise _err("unknown-track", f"device {device_id} not present in source")
        if device_id in seen_devices:
            raise _err("overlap-mapping", f"device {device_id} mapped more than once")
        if output in seen_outputs:
            raise _err("overlap-mapping", f"output track name {output!r} reused")
        seen_devices.add(device_id)
        seen_outputs.add(output)
        videos.append(VideoMapping(output=output, device_id=device_id))
    audio = summarise_audio(audio_path) if audio_path else None
    return MapPlan(videos=videos, audio=audio)
