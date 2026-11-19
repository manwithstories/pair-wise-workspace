"""分阶段流水线编排：解析 -> 索引修复 -> 时间轴 -> 切片/静音/指纹 -> 传输 -> 验证。

每个阶段记录输入摘要、参数、错误与产物校验值；阶段产物落检查点，
重跑时从最近阶段边界恢复，已验证结果保留；所有序列化键排序，
固定随机种子，保证重复运行输出一致。单个坏片段被隔离，不中断整批。
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field

from . import bitstream, fingerprint, keyframe_index, segmenter, silence
from .timeline import ErrorKind, MediaError, Stage, sha256_hex
from . import transport as transport_mod


@dataclass(slots=True)
class StageRecord:
    name: str
    input_summary: dict
    params: dict
    errors: list[MediaError] = field(default_factory=list)
    output_checksum: str | None = None
    output: dict = field(default_factory=dict)

    def to_dict(self) -> dict:
        return {
            "name": self.name,
            "input_summary": self.input_summary,
            "params": self.params,
            "errors": [e.to_dict() for e in self.errors],
            "output_checksum": self.output_checksum,
            "output": self.output,
        }


@dataclass(slots=True)
class PipelineConfig:
    codec: str
    workdir: str
    target_segment_us: int = 2_000_000
    max_segment_bytes: int = 64_000
    loss_burst: float = 0.08
    reorder_prob: float = 0.06
    duplicate_prob: float = 0.04
    truncate_at: int | None = None
    hamming_threshold: int = 8
    silence_threshold_db: float = -45.0
    seed: int = 12789
    mtu: int = transport_mod.DEFAULT_MTU
    # 传输评估的字节规模：弱网作用于完整流，截断只影响码流解析侧，
    # 为 None 时回退到实际文件字节数。
    transport_bytes: int | None = None

    def to_dict(self) -> dict:
        return {k: getattr(self, k) for k in self.__slots__}


def _checkpoint_path(workdir: str) -> str:
    return os.path.join(workdir, "pipeline.checkpoint.json")


def _write_checkpoint(workdir: str, state: dict) -> None:
    path = _checkpoint_path(workdir)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(state, fh, ensure_ascii=False, indent=2, sort_keys=True)


def _read_checkpoint(workdir: str) -> dict | None:
    path = _checkpoint_path(workdir)
    if not os.path.exists(path):
        return None
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


class Pipeline:
    def __init__(self, config: PipelineConfig) -> None:
        self.config = config
        self.records: list[StageRecord] = []
        self.errors: list[MediaError] = []
        os.makedirs(config.workdir, exist_ok=True)

    def _record(self, name: str, inputs: dict, params: dict,
                output: dict, errors: list[MediaError]) -> StageRecord:
        # 产物校验值覆盖输出 JSON，便于 verify 复核阶段一致性
        payload = json.dumps(output, ensure_ascii=False,
                             sort_keys=True).encode("utf-8")
        rec = StageRecord(name, inputs, params, list(errors),
                          sha256_hex(payload), output)
        self.records.append(rec)
        self.errors.extend(errors)
        return rec

    def run(self, video_path: str, audio_path: str | None,
            index_path: str | None, *, resume: bool = False) -> dict:
        state = _read_checkpoint(self.config.workdir) if resume else None
        if state is not None and state.get("config") == self.config.to_dict():
            return state["result"]
        result = self._execute(video_path, audio_path, index_path)
        _write_checkpoint(self.config.workdir, {
            "config": self.config.to_dict(),
            "records": [r.to_dict() for r in self.records],
            "result": result,
        })
        return result

    def _execute(self, video_path, audio_path, index_path) -> dict:
        cfg = self.config
        raw = open(video_path, "rb").read()
        video_checksum = sha256_hex(raw)
        stream_truncated = cfg.truncate_at is not None
        scan = bitstream.scan_bytes(raw, cfg.codec, stream_truncated)
        self._record(Stage.PARSE, {
            "video_path": video_path, "bytes": len(raw),
            "sha256": video_checksum,
        }, {"truncated": stream_truncated}, {
            "units": [u.to_dict() for u in scan.units],
            "total_bytes": scan.total_bytes,
        }, scan.errors)

        index_doc = self._stage_index(scan, index_path)
        timeline_pts = self._stage_timeline(scan)
        plan = self._stage_segment(scan, index_doc, timeline_pts)
        silence_result = self._stage_silence(audio_path)
        dedup = self._stage_fingerprint(raw, plan)
        transport_result = self._stage_transport(scan, timeline_pts)

        result = {
            "codec": cfg.codec,
            "video": {"path": video_path, "bytes": len(raw),
                      "sha256": video_checksum},
            "stages": [r.to_dict() for r in self.records],
            "segments": plan.to_dict(),
            "silence": silence_result.to_dict() if silence_result else None,
            "dedup": dedup.to_dict(),
            "transport": transport_result.metrics(),
            "error_offsets": sorted({e.offset for e in self.errors
                                     if e.offset >= 0}),
            "error_count": len(self.errors),
        }
        return result

    def _stage_index(self, scan, index_path) -> keyframe_index.IndexDocument:
        if index_path and os.path.exists(index_path):
            doc = keyframe_index.IndexDocument.load(index_path)
            keyframe_index.validate_index(doc, scan)
        else:
            doc = keyframe_index.build_index_from_scan(scan)
        keyframe_index.rebuild_index(doc, scan)
        self._record(Stage.INDEX, {"units": len(scan.units)},
                     self.config.to_dict(), doc.to_dict(), doc.errors)
        return doc

    def _stage_timeline(self, scan) -> dict[int, int]:
        from .timeline import TimestampNormalizer
        normalizer = TimestampNormalizer(tick_rate=90_000)
        pts_map: dict[int, int] = {}
        ordinal = 0
        for unit in scan.healthy_units():
            # 裸流无容器时间戳：以序号 * 固定帧时长合成滴答，关键帧锚定
            tick = ordinal * 3600  # 90kHz 下 3600 tick = 40ms (25fps)
            point = normalizer.feed(ordinal, tick, unit.start_offset,
                                    keyframe=unit.keyframe)
            if point is not None:
                pts_map[unit.start_offset] = point.pts_us
            ordinal += 1
        self._record(Stage.TIMELINE, {"healthy_units": ordinal},
                     {"tick_rate": 90_000, "frame_tick": 3600},
                     {"points": len(normalizer.points)}, normalizer.errors)
        return pts_map

    def _stage_segment(self, scan, index_doc, pts_map):
        offsets = [e.offset for e in index_doc.entries]
        plan = segmenter.segment_stream(
            scan, offsets,
            target_duration_us=self.config.target_segment_us,
            max_bytes=self.config.max_segment_bytes,
            pts_by_offset=pts_map)
        self._record(Stage.SEGMENT, {"keyframe_entries": len(offsets)},
                     {"target_us": self.config.target_segment_us,
                      "max_bytes": self.config.max_segment_bytes},
                     plan.to_dict(), plan.errors)
        return plan

    def _stage_silence(self, audio_path):
        if not audio_path or not os.path.exists(audio_path):
            return None
        result = silence.detect_silence(
            audio_path, threshold_db=self.config.silence_threshold_db)
        self._record(Stage.SILENCE, {"audio_path": audio_path},
                     {"threshold_db": self.config.silence_threshold_db},
                     result.to_dict(), result.errors)
        return result

    def _stage_fingerprint(self, raw, plan):
        fps = []
        errors: list[MediaError] = []
        for seg in plan.segments:
            try:
                payload = raw[seg.start_offset:seg.end_offset]
                fps.append(fingerprint.compute_fingerprint(
                    self.config.codec, payload, seg.index,
                    seg.start_offset, seg.end_offset))
            except Exception as exc:  # 单个坏片段不得中断整批
                errors.append(MediaError(
                    Stage.FINGERPRINT, ErrorKind.CORRUPT_UNIT,
                    seg.start_offset,
                    f"片段 {seg.index} 指纹计算失败已隔离: {exc}",
                    recovered=False))
        dedup = fingerprint.deduplicate(
            fps, hamming_threshold=self.config.hamming_threshold)
        dedup.errors.extend(errors)
        self._record(Stage.FINGERPRINT, {"segments": len(plan.segments)},
                     {"hamming_threshold": self.config.hamming_threshold},
                     dedup.to_dict(), dedup.errors)
        return dedup

    def _stage_transport(self, scan, pts_map):
        total = self.config.transport_bytes
        if total is None or total <= 0:
            total = scan.total_bytes
        result = transport_mod.simulate(
            total,
            loss_burst=self.config.loss_burst,
            reorder_prob=self.config.reorder_prob,
            duplicate_prob=self.config.duplicate_prob,
            mtu=self.config.mtu, seed=self.config.seed,
            pts_by_offset=pts_map)
        self._record(Stage.TRANSPORT,
                     {"file_bytes": scan.total_bytes,
                      "transport_bytes": total},
                     {"loss_burst": self.config.loss_burst,
                      "seed": self.config.seed},
                     result.metrics(), result.errors)
        return result
