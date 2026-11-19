"""流水线编排：采集清单 -> 解析/时间修复 -> 时间线图 -> 索引/音频 -> 报告。

调度按 timeline 的拓扑依赖进行；每阶段经 scheduler 检查点保护，
输入摘要或处理参数变化时检查点指纹失配自动重跑。
"""
from __future__ import annotations

import time
from pathlib import Path

from . import audio as audio_mod
from . import index as index_mod
from . import probe as probe_mod
from . import report as report_mod
from . import sync as sync_mod
from .color import arbitrate_all
from .model import (ErrorCategory, ExitCode, Issue, Manifest, TrackKind,
                    TrackResult, TrackState, atomic_write_json, read_json,
                    resolve_within_root, sha256_file)
from .scheduler import Scheduler
from .timeline import build_timeline


class Pipeline:
    def __init__(self, manifest: Manifest, work_dir: str) -> None:
        self.manifest = manifest
        self.work_dir = work_dir
        Path(work_dir).mkdir(parents=True, exist_ok=True)
        (Path(work_dir) / "index").mkdir(exist_ok=True)
        (Path(work_dir) / "audio").mkdir(exist_ok=True)
        self.scheduler = Scheduler(manifest, work_dir)
        # 跨阶段共享：视频帧索引、音频结果、索引、跳转率、样本差、同步误差
        self.frame_index: dict[str, list] = {}
        self.audio_results: dict[str, audio_mod.AudioResult] = {}
        self.ra_indexes: dict[str, index_mod.RandomAccessIndex] = {}
        self.seek_rates: dict[str, float] = {}
        self.audio_deltas: dict[str, int] = {}
        self.sync_errors: dict[str, float] = {}
        self.audio_durations: dict[str, int] = {}
        self._cache: dict[str, bytes] = {}
        self.hard_input_error: ExitCode | None = None

    # ------------------------------------------------------------ 主入口
    def run(self) -> tuple[dict, ExitCode]:
        # 0) 清单语义校验（硬错误直接抛 INVALID_INPUT）
        probe_mod.validate_manifest(self.manifest)
        # 1) 色彩裁决（error 策略冲突会抛 INVALID_INPUT）
        try:
            self.color_verdicts = arbitrate_all(self.manifest)
        except Exception:
            self.hard_input_error = ExitCode.INVALID_INPUT
            raise
        color_conflicts = sum(
            len(v.conflicts) for v in self.color_verdicts.values())

        # 2) 拓扑调度各轨处理
        self.scheduler.run(self._stage)

        # 3) 时间线与指标
        timeline = build_timeline(self.manifest, self.frame_index,
                                  self.audio_durations)
        worst_sync = max(self.sync_errors.values(), default=0.0)
        metrics = report_mod.aggregate(
            self.manifest, self.scheduler.results, timeline,
            self.seek_rates, self.audio_deltas, worst_sync,
            color_conflicts)
        exit_code = report_mod.decide_exit_code(
            self.manifest, self.scheduler.results, metrics,
            self.hard_input_error)
        report = report_mod.build_report(
            self.manifest, self.scheduler.results, timeline, metrics,
            self.color_verdicts)
        atomic_write_json(str(Path(self.work_dir) / "report.json"), report)
        return report, exit_code

    # ------------------------------------------------------------ 单轨处理
    def _stage(self, sched: Scheduler, track, result: TrackResult) -> None:
        if track.kind is TrackKind.VIDEO:
            self._stage_video(sched, track, result)
        else:
            self._stage_audio(sched, track, result)

    def _abs(self, rel: str) -> str:
        return str(resolve_within_root(self.manifest.root, rel))

    def _stage_video(self, sched: Scheduler, track, result: TrackResult) -> None:
        abs_path = self._abs(track.path)
        fhash = sched.file_hash(track.track_id, abs_path)

        # --- 阶段 probe ---
        t0 = time.perf_counter()
        cp = sched.checkpoint_load(self.manifest, track.track_id,
                                   "probe", fhash)
        if cp is not None:
            probe_result = self._rehydrate_probe(track, cp)
        else:
            probe_result = probe_mod.probe_video(track, self.manifest.root)
            sched.checkpoint_save(self.manifest, track.track_id, "probe",
                                  fhash, self._dehydrate_probe(probe_result))
        result.latency_ms.append((time.perf_counter() - t0) * 1000.0)
        result.issues.extend(probe_result.issues)
        result.video = probe_result.video
        self.frame_index[track.track_id] = probe_result.frames

        # --- 阶段 index + seek_verify ---
        t0 = time.perf_counter()
        cp_idx = sched.checkpoint_load(self.manifest, track.track_id,
                                       "seek_verify", fhash)
        idx = index_mod.build_index(track, probe_result.frames,
                                    Path(abs_path).stat().st_size)
        if track.index_path:
            legacy = self._abs(track.index_path)
            if Path(legacy).is_file():
                result.issues.extend(
                    index_mod.validate_legacy_index(track, idx, legacy))
        if cp_idx is not None and not idx.stale:
            rate = float(cp_idx["seek_pass_rate"])
        else:
            index_mod.verify_seek_points(track, idx, abs_path, sample=12)
            rate = index_mod.seek_pass_rate(idx)
            sched.checkpoint_save(self.manifest, track.track_id,
                                  "seek_verify", fhash,
                                  {"seek_pass_rate": rate,
                                   "entry_count": len(idx.entries)})
        result.latency_ms.append((time.perf_counter() - t0) * 1000.0)
        self.ra_indexes[track.track_id] = idx
        self.seek_rates[track.track_id] = rate
        index_mod.write_index(
            idx, str(Path(self.work_dir) / "index" /
                     f"{track.track_id}.index.json"))
        result.outputs["keyframe_index"] = f"index/{track.track_id}.index.json"
        result.issues.extend(idx.issues)

    def _stage_audio(self, sched: Scheduler, track, result: TrackResult) -> None:
        abs_path = self._abs(track.path)
        fhash = sched.file_hash(track.track_id, abs_path)
        # 参考时长：取所依赖视频轨的时间线时长，用于边界补齐对齐。
        ref_duration = self._reference_duration(track)

        t0 = time.perf_counter()
        cp = sched.checkpoint_load(self.manifest, track.track_id,
                                   "audio", fhash)
        if cp is not None:
            # 检查点只缓存元数据；样本仍需重算以驱动同步实测，
            # 但归一参数与样本数可直接校验复用。
            ares = audio_mod.process_audio(track, self.manifest.root,
                                           reference_duration_us=ref_duration)
            if ares.info.output_frames != cp["output_frames"]:
                sched.store.invalidate_track(track.track_id)
                cp = None
        else:
            ares = audio_mod.process_audio(track, self.manifest.root,
                                           reference_duration_us=ref_duration)
            sched.checkpoint_save(self.manifest, track.track_id, "audio",
                                  fhash, {
                                      "output_frames": ares.info.output_frames,
                                      "target_rate": ares.info.target_rate,
                                      "channel_map": ares.info.channel_map,
                                      "padded": ares.info.padded_samples})
        result.latency_ms.append((time.perf_counter() - t0) * 1000.0)
        result.issues.extend(ares.issues)
        result.audio = ares.info
        self.audio_results[track.track_id] = ares
        self.audio_durations[track.track_id] = ares.info.duration_us
        self.audio_deltas[track.track_id] = ares.info.padded_samples

        out_wav = str(Path(self.work_dir) / "audio" /
                      f"{track.track_id}.wav")
        audio_mod.write_normalized(ares, out_wav)
        result.outputs["normalized_audio"] = f"audio/{track.track_id}.wav"

        # 同步实测：以参考视频轨为基准。
        ref_id = self.manifest.reference_video
        if ref_id and ref_id in self.frame_index:
            ref_track = self.manifest.tracks[ref_id]
            err = sync_mod.measure_sync_ms(
                ref_track, self.manifest.root, track,
                ares.samples, ares.info.target_rate)
            self.sync_errors[track.track_id] = err

    def _reference_duration(self, track) -> int | None:
        for dep in track.depends_on:
            dt = self.manifest.tracks.get(dep)
            if dt and dt.kind is TrackKind.VIDEO and dep in self.frame_index:
                frames = self.frame_index[dep]
                if frames:
                    frame_us = round(1_000_000 / dt.frame_rate)
                    return frames[-1].pts_us + frame_us
        return None

    # ------------------------------------------------------------ 检查点序列化
    def _dehydrate_probe(self, pr) -> dict:
        return {
            "video": pr.video.to_dict(),
            "frames": [f.to_dict() for f in pr.frames],
            "issues": [i.to_dict() for i in pr.issues],
            "truncated": pr.truncated,
        }

    def _rehydrate_probe(self, track, cp: dict):
        from .model import FrameRecord, VideoInfo
        from .probe import ProbeResult
        v = cp["video"]
        info = VideoInfo(**v)
        frames = [FrameRecord(**f) for f in cp["frames"]]
        issues = [self._issue_from_dict(d) for d in cp["issues"]]
        return ProbeResult(track=track, packets=[], frames=frames,
                           video=info, issues=issues,
                           truncated=cp["truncated"])

    @staticmethod
    def _issue_from_dict(d: dict) -> Issue:
        cat = ErrorCategory(d["category"])
        return Issue(category=cat, track=d["track"], message=d["message"],
                     byte_offset=d.get("byte_offset"),
                     pts_us=d.get("pts_us"), recovered=d.get("recovered"),
                     detail=d.get("detail", {}))
