"""处理流水线：解析 -> 统一时间基准 -> 修复 -> 分段索引 -> 回读复核。

库 API 直接消费字节流与时间基准配置，CLI 只做演示/验证适配。
分段边界取媒体关键帧时间，保证索引可随机定位到最近关键帧；
中断续建时只重建缺失或未验证的分段，已验证证据原样复用。
"""
from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

from .index import SegmentIndex, canonical_digest
from .media import MediaInfo
from .model import (
    Cue,
    ErrorRecord,
    IndexEntry,
    Segment,
    SourceRef,
    TimecodeRecord,
)
from .repair import RepairConfig, repair_cues, repair_timecodes
from .subtitles import SubtitleParser
from .timecode_list import TimecodeListParser


@dataclass
class PipelineConfig:
    """流水线参数：帧率、分段 GOP、漂移阈值与输入标识。"""

    frame_rate: Fraction = Fraction(24)
    gop: int = 100
    drift_limit: Fraction = Fraction(1, 4)
    subtitle_source: str = "subtitles.srt"
    timecode_source: str = "timecodes.txt"


@dataclass
class PipelineResult:
    """一次流水线运行的全部产物与统计。"""

    segments: list[Segment]
    entries: dict[int, list[IndexEntry]]
    errors: list[ErrorRecord]
    media: MediaInfo | None
    attempted_blocks: int = 0          # 解析到的块总数（含被隔离块）
    recovered: int = 0                 # 可恢复并已修复的记录数
    verified: tuple[int, int] = (0, 0)
    keyframe_hits: int = 0
    keyframe_total: int = 0

    @property
    def roundtrip_rate(self) -> Fraction:
        if not self.attempted_blocks:
            return Fraction(0)
        valid = sum(seg.entry_count for seg in self.segments)
        return Fraction(valid, self.attempted_blocks)

    @property
    def keyframe_hit_rate(self) -> Fraction:
        if not self.keyframe_total:
            return Fraction(1)
        return Fraction(self.keyframe_hits, self.keyframe_total)


class Pipeline:
    """两段式流水线：build 写索引，verify 回读复核证据。"""

    def __init__(
        self,
        index_path: str | Path,
        config: PipelineConfig | None = None,
        repair_config: RepairConfig | None = None,
    ) -> None:
        self.index_path = Path(index_path)
        self.config = config or PipelineConfig()
        self.repair_config = repair_config or RepairConfig(
            drift_limit=self.config.drift_limit
        )
        self.errors: list[ErrorRecord] = []

    def build(
        self,
        subtitle_bytes: bytes,
        timecode_bytes: bytes,
        source_blobs: dict[str, bytes],
        media: MediaInfo,
        resume: bool = True,
    ) -> PipelineResult:
        """解析输入并建立（或续建）分段索引。"""

        cues, cue_errors, cue_blocks = self._parse_subtitles(subtitle_bytes)
        records, tc_errors = self._parse_timecodes(timecode_bytes)
        self.errors.extend(cue_errors + tc_errors)

        records, repair_errs = repair_timecodes(records, self.repair_config)
        cues, cue_repair_errs = repair_cues(cues)
        self.errors.extend(repair_errs + cue_repair_errs)

        boundaries = self._segment_boundaries(media)
        entries_plan = self._plan_entries(cues, records, boundaries)
        reusable = self._resume_or_open(resume, media)

        if reusable:
            index = SegmentIndex(self.index_path)
            index.load_existing()
            index.open_append()
        else:
            index = SegmentIndex(self.index_path)
            index.open_write(
                frame_rate=str(self.config.frame_rate),
                gop=self.config.gop,
            )
        rebuilt: set[int] = set()
        for sid, (start, end) in enumerate(boundaries):
            if sid in reusable:
                continue
            entries = entries_plan.get(sid, [])
            refs = [e.ref for e in entries]
            segment = Segment(
                segment_id=sid, start=start, end=end,
                entry_count=len(entries),
                byte_start=min((r.start for r in refs), default=0),
                byte_end=max((r.end for r in refs), default=0),
                sha256=canonical_digest(
                    source_blobs, refs, sid, start, end
                ),
            )
            index.append_segment(segment, entries)
            rebuilt.add(sid)
        index.close_write()

        reader = SegmentIndex(self.index_path)
        reader.load_existing()
        ok, total, failed = reader.verify(source_blobs)
        for sid in failed:
            self.errors.append(ErrorRecord(
                source=self.index_path.name, start=0, end=0,
                stage="index", reason=f"分段 {sid} SHA-256 复核失败",
                action="rebuild-required", recoverable=True,
            ))
        hits, kf_total = self._keyframe_alignment(entries_plan, media, boundaries)
        recovered = sum(1 for e in self.errors if e.recoverable)
        return PipelineResult(
            segments=sorted(reader.segments.values(), key=lambda s: s.segment_id),
            entries=reader.entries,
            errors=list(self.errors),
            media=media,
            attempted_blocks=cue_blocks + len(records) + len(tc_errors),
            recovered=recovered,
            verified=(ok, total),
            keyframe_hits=hits,
            keyframe_total=kf_total,
        )

    # ---- 内部步骤 ----------------------------------------------------

    def _parse_subtitles(
        self, subtitle_bytes: bytes
    ) -> tuple[list[Cue], list[ErrorRecord], int]:
        parser = SubtitleParser(self.config.subtitle_source)
        for i in range(0, len(subtitle_bytes), 4096):
            parser.feed(subtitle_bytes[i : i + 4096])
        cues, errs = parser.close()
        blocks = len(cues) + len(errs)
        return cues, errs, blocks

    def _parse_timecodes(
        self, timecode_bytes: bytes
    ) -> tuple[list[TimecodeRecord], list[ErrorRecord]]:
        parser = TimecodeListParser(
            self.config.timecode_source, self.config.frame_rate
        )
        for i in range(0, len(timecode_bytes), 4096):
            parser.feed(timecode_bytes[i : i + 4096])
        return parser.close()

    def _segment_boundaries(
        self, media: MediaInfo
    ) -> list[tuple[Fraction, Fraction]]:
        """以关键帧为分段起点，末段收口到媒体时长。"""

        keys = media.keyframes or []
        points = [k.pts_time for k in keys]
        if not points or points[0] != 0:
            points.insert(0, Fraction(0))
        points.append(media.duration)
        return [
            (points[i], points[i + 1])
            for i in range(len(points) - 1)
            if points[i + 1] > points[i]
        ]

    def _plan_entries(
        self,
        cues: list[Cue],
        records: list[TimecodeRecord],
        boundaries: list[tuple[Fraction, Fraction]],
    ) -> dict[int, list[IndexEntry]]:
        plan: dict[int, list[IndexEntry]] = {i: [] for i in range(len(boundaries))}

        def locate(t: Fraction) -> int:
            for sid, (start, end) in enumerate(boundaries):
                if start <= t < end:
                    return sid
            return len(boundaries) - 1

        for cue in cues:
            sid = locate(cue.start)
            plan[sid].append(IndexEntry(
                segment_id=sid, kind="cue", seq=cue.seq,
                start=cue.start, end=cue.end, ref=cue.ref, flags=list(cue.flags),
            ))
        for n, rec in enumerate(records):
            if rec.timeline is None:
                continue
            sid = locate(rec.timeline)
            plan[sid].append(IndexEntry(
                segment_id=sid, kind="timecode", seq=n,
                start=rec.timeline,
                end=rec.timeline + Fraction(1, rec.rate_fps),
                ref=rec.ref, flags=list(rec.flags),
            ))
        for sid in plan:
            plan[sid].sort(key=lambda e: (e.start, e.kind))
        return plan

    def _resume_or_open(
        self, resume: bool, media: MediaInfo
    ) -> set[int]:
        if not resume or not self.index_path.exists():
            return set()
        try:
            return SegmentIndex(self.index_path).load_existing()
        except Exception:
            # 版本不符/魔数损坏：整份索引不可信，整体重建
            self.index_path.unlink(missing_ok=True)
            return set()

    def _keyframe_alignment(
        self,
        plan: dict[int, list[IndexEntry]],
        media: MediaInfo,
        boundaries: list[tuple[Fraction, Fraction]],
    ) -> tuple[int, int]:
        """每个分段起点是否恰好落在关键帧上（命中率证据）。"""

        key_times = {k.pts_time for k in media.keyframes}
        hits = total = 0
        for sid, (start, _end) in enumerate(boundaries):
            if not plan.get(sid):
                continue
            total += 1
            if start in key_times or start == 0:
                hits += 1
        return hits, total
