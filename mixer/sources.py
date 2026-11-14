"""Stateful WAV track sources with resampling across blocks."""

from __future__ import annotations

import numpy as np
import soundfile as sf

from .errors import MixerIOError, RenderError
from .manifest import Project, Track
from .nodes import read_track_block
from .resample import SincResampler


IO_BLOCK = 8192


class TrackSource:
    def __init__(self, project: Project, track: Track) -> None:
        self.track = track
        self.project_rate = project.sample_rate
        self.timeline_frames = project.duration_frames
        self.position = 0
        self._eof = False
        self._parts: list[np.ndarray] = []
        self._pending = 0
        try:
            self.handle = sf.SoundFile(track.path, mode="r")
        except Exception as exc:
            raise MixerIOError(
                f"cannot open WAV for track {track.id!r}: {exc}",
                details={"track_id": track.id, "path": track.path},
            ) from exc
        channels = int(self.handle.channels)
        if channels not in (1, 2):
            self.handle.close()
            raise RenderError(
                "unsupported track channel count",
                details={"track_id": track.id, "channels": channels},
            )
        self.channels = channels
        self.resampler: SincResampler | None = None
        if int(self.handle.samplerate) != self.project_rate:
            self.resampler = SincResampler(int(self.handle.samplerate), self.project_rate, self.channels)
            self.resampler.set_target(self.timeline_frames)

    def read(self, start: int, length: int) -> np.ndarray:
        if start != self.position:
            raise RenderError("track source blocks must be requested sequentially")
        stop = min(start + length, self.timeline_frames)
        wanted = stop - start
        if wanted <= 0:
            return np.zeros((self.channels, 0), dtype=np.float32)
        if self.resampler is None:
            try:
                block = read_track_block(
                    self.handle,
                    start,
                    start + wanted,
                    self.channels,
                    track_id=self.track.id,
                )
            except RenderError:
                raise
            except Exception as exc:
                raise MixerIOError(
                    f"cannot read WAV for track {self.track.id!r}: {exc}",
                    details={"track_id": self.track.id, "path": self.track.path},
                ) from exc
        else:
            block = self._read_resampled(wanted)
        self.position += wanted
        return block

    def _read_resampled(self, wanted: int) -> np.ndarray:
        try:
            while self._pending < wanted and not self._eof:
                raw = self.handle.read(IO_BLOCK, dtype="float32", always_2d=True)
                if raw.shape[0] == 0:
                    self._eof = True
                    break
                raw_block = np.asarray(raw, dtype=np.float32).T
                if not np.all(np.isfinite(raw_block)):
                    raise RenderError("track contains NaN or Inf samples", details={"track_id": self.track.id})
                self._append(self.resampler.push(raw_block))
            while (
                self._pending < wanted
                and self._eof
                and self.resampler.produced < self.timeline_frames
            ):
                self._append(self.resampler.flush(wanted - self._pending))
        except (MixerIOError, RenderError):
            raise
        except Exception as exc:
            raise MixerIOError(
                f"cannot read WAV for track {self.track.id!r}: {exc}",
                details={"track_id": self.track.id, "path": self.track.path},
            ) from exc
        if not self._parts:
            if self._pending:
                return np.zeros((self.channels, wanted), dtype=np.float32)
            raise RenderError("resampler produced no samples for a requested block")
        combined = np.concatenate(self._parts, axis=1)
        self._parts = []
        available = combined.shape[1]
        block = combined[:, :wanted]
        if available < wanted:
            block = np.pad(block, ((0, 0), (0, wanted - available)))
        if available > wanted:
            self._parts = [combined[:, wanted:]]
            self._pending = available - wanted
        else:
            self._pending = 0
        return block

    def _append(self, block: np.ndarray) -> None:
        if block.shape[1]:
            self._parts.append(block)
            self._pending += block.shape[1]

    def close(self) -> None:
        self.handle.close()
