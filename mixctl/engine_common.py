"""离线/流式引擎共享的渲染会话。

两条引擎共用同一份 Project、MixGraph 与 Automation/Resampler 构造方式，
保证差异只来自“一次喂多少数据”，而不来自算法路径。
"""

from __future__ import annotations

from pathlib import Path

import numpy as np
import soundfile as sf

from .automation import Automation, build_track_automation
from .errors import AudioIOError
from .graph import MixGraph
from .metrics import Metrics
from .project import Project, Track
from .resample import Resampler


def adapt_channels(block: np.ndarray, channels: int) -> np.ndarray:
    """把 (n, k) 音频适配为声明声道数：单声道复制、多声道取前 k 路。"""
    if block.ndim == 1:
        block = block[:, None]
    current = block.shape[1]
    if current == channels:
        return block.astype(np.float64, copy=False)
    if current == 1:
        return np.repeat(block, channels, axis=1).astype(np.float64)
    if channels == 1:
        return block[:, :1].astype(np.float64)
    return block[:, :channels].astype(np.float64)


class TrackRuntime:
    def __init__(self, track: Track, project: Project):
        self.track = track
        self.path = project.base_dir() / track.file
        self.resampler = Resampler(
            source_sr=self._read_sr(), target_sr=project.target_sr
        )
        self.automation = build_track_automation(
            track, sample_rate=project.target_sr, hop=project.hop
        )
        static_gain = 10.0 ** (track.gain_db / 20.0)
        self.static_gain = float(static_gain)
        self.metrics = Metrics(project.clip_threshold)
        self.total_target_samples = int(round(project.duration * project.target_sr))
        self.project_hop = project.hop
        self.cursor = 0

    def _read_sr(self) -> int:
        if not self.path.is_file():
            raise AudioIOError(f"音频文件缺失: {self.path}")
        try:
            info = sf.info(str(self.path))
        except Exception as exc:  # soundfile/Libsndfile 内部错误
            raise AudioIOError(f"WAV 无法识别或已损坏: {self.path} ({exc})") from exc
        return int(info.samplerate)

    def open(self) -> int:
        try:
            handle = sf.SoundFile(str(self.path), mode="r")
        except Exception as exc:
            raise AudioIOError(f"WAV 无法打开: {self.path} ({exc})") from exc
        if handle.channels != self.track.channels:
            handle.close()
            raise AudioIOError(
                f"{self.path}: 声道数 {handle.channels} 与工程声明 "
                f"{self.track.channels} 不符"
            )
        return handle

    def apply_gain(self, target_block: np.ndarray, start_sample: int) -> np.ndarray:
        output = target_block * self.static_gain
        if self.automation is not None:
            gains = self.automation.gains(start_sample, target_block.shape[0])
            output = output * gains[:, None]
        return output.astype(np.float64)

    def collect_metrics(self, processed: np.ndarray) -> None:
        self.metrics.update(processed)

    def finalize_auto_points(self) -> None:
        if self.automation is not None:
            points = int(np.ceil(self.total_target_samples / self.project_hop))
            self.metrics.add_auto_points(points)


class RenderSession:
    def __init__(self, project: Project, graph: MixGraph):
        self.project = project
        self.graph = graph
        self.target_sr = project.target_sr
        self.total_samples = int(round(project.duration * project.target_sr))
        self.channels = 2
        self.track_runtimes = {
            track.name: TrackRuntime(track, project) for track in project.tracks
        }
        self.master_metrics = Metrics(project.clip_threshold)

    def ratio_report(self, name: str) -> dict:
        runtime = self.track_runtimes[name]
        info = runtime.resampler.ratio
        return {
            "source_sr": info.source_sr,
            "target_sr": info.target_sr,
            "ratio": info.ratio_text,
        }

    def new_bus_buffers(self, frames: int) -> dict[str, np.ndarray]:
        return {
            name: np.zeros((frames, self.channels), dtype=np.float64)
            for name in self.graph.bus_outputs
        }

    def mix_block(
        self,
        target_blocks: dict[str, np.ndarray],
        start_sample: int,
        frames: int,
    ) -> np.ndarray:
        """处理一个目标采样率块，返回主总线信号（未钳制）。"""
        buses = self.new_bus_buffers(frames)
        for name, runtime in self.track_runtimes.items():
            raw = target_blocks[name]
            if raw.shape[0] < frames:
                pad = np.zeros(
                    (frames - raw.shape[0], raw.shape[1]), dtype=np.float64
                )
                raw = np.concatenate([raw, pad], axis=0)
            processed = runtime.apply_gain(raw, start_sample)
            runtime.collect_metrics(processed)
            if self.graph.is_audible(name):
                bus_name = self.graph.track_bus[name]
                buses[bus_name] += processed
        for bus_name in self.graph.order:
            output = self.graph.bus_outputs[bus_name]
            if output is not None:
                buses[output] += buses[bus_name]
        master = buses[self.graph.master]
        self.master_metrics.update(master)
        return master.astype(np.float64)
