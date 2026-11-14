"""Dual output writer for the deterministic index and CFR proxy."""

from __future__ import annotations

import json
import os
from fractions import Fraction
from pathlib import Path
from typing import Any, TextIO

import av

from frameaudit.errors import ProcessingError
from frameaudit.model import RepairedFrame
from frameaudit.stats import plane_hash


MICROSECOND_TIME_BASE = Fraction(1, 1_000_000)


class IndexWriter:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.temp_path = self.path.with_name(self.path.name + ".tmp")
        if self.temp_path.exists():
            self.temp_path.unlink()
        self._stream: TextIO = self.temp_path.open("w", encoding="utf-8", newline="\n")
        self.rows = 0

    def write(self, frame: RepairedFrame, hash_value: str | None) -> None:
        row = {
            "decode_idx": frame.decode_idx,
            "present_idx": frame.present_idx,
            "pts_raw_us": frame.pts_raw_us,
            "pts_fixed_us": frame.pts_fixed_us,
            "pict_type": frame.pict_type,
            "keyframe": frame.keyframe,
            "plane_hash": hash_value,
        }
        self._stream.write(json.dumps(row, ensure_ascii=True, separators=(",", ":")) + "\n")
        self.rows += 1

    def commit(self) -> None:
        self._stream.flush()
        os.fsync(self._stream.fileno())
        self._stream.close()
        os.replace(self.temp_path, self.path)

    def abort(self) -> None:
        try:
            self._stream.close()
        finally:
            self.temp_path.unlink(missing_ok=True)


class ProxyWriter:
    def __init__(self, path: str | Path, width: int, height: int, clock_us: int) -> None:
        if width <= 0 or height <= 0:
            raise ProcessingError("invalid proxy dimensions")
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.temp_path = self.path.with_name(self.path.name + ".tmp.mp4")
        if self.temp_path.exists():
            self.temp_path.unlink()
        self.width = width
        self.height = height
        self.clock_us = clock_us
        self.frames = 0
        self._container: av.OutputContainer | None = None
        try:
            self._container = av.open(str(self.temp_path), mode="w", format="mp4")
            rate = Fraction(1_000_000, max(1, clock_us))
            self._stream = self._container.add_stream("libx264", rate=rate)
            self._stream.width = width
            self._stream.height = height
            self._stream.pix_fmt = "yuv420p"
            self._stream.codec_context.time_base = MICROSECOND_TIME_BASE
            self._stream.options = {
                "preset": "ultrafast",
                "crf": "0",
                "g": "48",
                "bf": "0",
                "x264-params": "keyint=48:min-keyint=48:scenecut=0",
            }
        except Exception as exc:
            if self._container is not None:
                try:
                    self._container.close()
                except Exception:
                    pass
            self.temp_path.unlink(missing_ok=True)
            raise ProcessingError(f"cannot create proxy: {exc}") from exc

    def _prepare(self, frame: Any, index: int) -> av.VideoFrame:
        try:
            prepared = frame.reformat(
                width=self.width,
                height=self.height,
                format="yuv420p",
            )
        except Exception:
            prepared = av.VideoFrame(self.width, self.height, "yuv420p")
        prepared.pts = index * self.clock_us
        prepared.time_base = MICROSECOND_TIME_BASE
        return prepared

    def write(self, frame: RepairedFrame) -> str:
        prepared = self._prepare(frame.frame, frame.present_idx)
        digest = plane_hash(prepared)
        if digest is None:
            raise ProcessingError("cannot hash prepared proxy frame")
        try:
            for packet in self._stream.encode(prepared):
                self._container.mux(packet)
        except Exception as exc:
            raise ProcessingError(f"cannot encode proxy frame {self.frames}: {exc}") from exc
        self.frames += 1
        return digest

    def commit(self) -> None:
        try:
            for packet in self._stream.encode(None):
                self._container.mux(packet)
            self._container.close()
            os.replace(self.temp_path, self.path)
        except Exception as exc:
            raise ProcessingError(f"cannot finalize proxy: {exc}") from exc

    def abort(self) -> None:
        try:
            self._container.close()
        except Exception:
            pass
        self.temp_path.unlink(missing_ok=True)


class DualWriter:
    """Write index and proxy from exactly the same repaired frame stream."""

    def __init__(
        self,
        index_path: str | Path,
        proxy_path: str | Path,
        width: int,
        height: int,
        clock_us: int,
    ) -> None:
        self.index = IndexWriter(index_path)
        try:
            self.proxy = ProxyWriter(proxy_path, width, height, clock_us)
        except Exception:
            self.index.abort()
            raise
        self._closed = False

    def write(self, frame: RepairedFrame) -> None:
        actual_hash = self.proxy.write(frame)
        self.index.write(frame, actual_hash)

    def commit(self) -> None:
        if self.proxy.frames == 0:
            raise ProcessingError("no frames available for proxy output")
        try:
            self.proxy.commit()
            self.index.commit()
            self._closed = True
        except Exception:
            self.proxy.abort()
            self.index.abort()
            self.proxy.path.unlink(missing_ok=True)
            self._closed = True
            raise

    def abort(self) -> None:
        if self._closed:
            return
        self.proxy.abort()
        self.index.abort()
        self._closed = True

