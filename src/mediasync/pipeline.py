"""离线流水线：扫描、归一、切点规划、分块提交与取消恢复。

取消点在每块提交前检查；恢复时校验输入摘要、配置摘要与已确认块
校验和，不匹配则隔离旧检查点并从首个安全边界重算。
"""

from __future__ import annotations

import dataclasses
import time
from pathlib import Path

from .bitstream import (
    Codec,
    KeyframeEntry,
    build_timeline,
    scan_bitstream,
    validate_keyframe_index,
)
from .checkpoint import Block, CheckpointStore, canonical_json, sha256_bytes
from .clock import MediaClock
from .digest import config_digest, input_digest, read_json, write_json
from .errors import ErrorBook
from .recovery import RecoveryCoordinator
from .safety import ensure_within
from .segment import Representation, plan_cuts, plan_to_dict
from .stages import run_audio_stage, run_color_stage


@dataclasses.dataclass
class JobConfig:
    workdir: str
    representations: int = 3
    segment_ms: int = 500
    max_deviation_ms: int = 25
    block_records: int = 2
    corrupt_every: int = 0
    interrupt_after: int = 0
    resume: bool = False
    target_sample_rate: int = 48000
    target_layout: str = "stereo"
    corrupt_offset: int | None = None
    media_frames: int = 30
    media_fps: int = 30

    def digest_inputs(self) -> list[str]:
        rels = []
        for i in range(self.representations):
            rels.append(f"fixtures/rep{i}.{self._codec(i).value}")
            rels.append(f"fixtures/rep{i}.index.json")
        rels.append("fixtures/audio.wav")
        return rels

    @staticmethod
    def _codec(index: int) -> Codec:
        return (Codec.H264, Codec.H265, Codec.AV1)[index % 3]


class CancellationToken:
    """协作式取消：置位后流水线在下一个块边界安全停止。"""

    def __init__(self) -> None:
        self.cancelled = False

    def cancel(self) -> None:
        self.cancelled = True


@dataclasses.dataclass
class PipelineResult:
    exit_code: int
    segments: int
    aligned_representations: str
    manifest_sha256: str = ""
    index_sha256: str = ""
    resume_ms: int = 0
    corrupt_offset_bytes: int | None = None
    errors: ErrorBook = dataclasses.field(default_factory=ErrorBook)
    quarantined: str = ""
    cancelled: bool = False


class Pipeline:
    def __init__(self, config: JobConfig) -> None:
        self.cfg = config
        self.errors = ErrorBook()
        self.store = CheckpointStore("checkpoint", config.workdir, self.errors)
        self.recovery = RecoveryCoordinator(
            self.store, config.workdir, self.errors
        )
        self._quarantine_note = ""


    def run(self, token: CancellationToken | None = None) -> PipelineResult:
        token = token or CancellationToken()
        started = time.perf_counter()
        inputs = self.cfg.digest_inputs()
        input_sum = input_digest(self.cfg.workdir, inputs)
        cfg_sum = self._config_digest()

        resume_from = self.recovery.prepare_resume(input_sum, cfg_sum)
        self._quarantine_note = self.recovery.quarantine_note
        if self.errors.any_fatal():
            return self._fail(4, started)

        scanned = self._scan_all()
        if self.errors.any_fatal():
            return self._fail(3, started)

        self._normalize_audio_and_color()
        plan = self._plan_segments(scanned)
        if self.errors.any_fatal():
            return self._fail(3, started)

        records = self._index_records(scanned, plan)
        committed = self._commit_blocks(records, token, resume_from)
        if token.cancelled:
            return PipelineResult(
                exit_code=0,
                segments=len(plan.segments),
                aligned_representations=self._aligned_label(plan),
                errors=self.errors,
                cancelled=True,
            )

        manifest_path, index_path = self._publish(plan, records, input_sum, cfg_sum)
        result = PipelineResult(
            exit_code=0,
            segments=len(plan.segments),
            aligned_representations=self._aligned_label(plan),
            manifest_sha256=sha256_bytes(manifest_path.read_bytes()),
            index_sha256=sha256_bytes(index_path.read_bytes()),
            resume_ms=int((time.perf_counter() - started) * 1000),
            corrupt_offset_bytes=self.cfg.corrupt_offset,
            errors=self.errors,
            quarantined=self._quarantine_note,
        )
        return result


    def _config_digest(self) -> str:
        # workdir 是位置标识，resume/interrupt_after 是易失运行控制，
        # 三者都不构成配置身份；排除后不同目录/中断续跑的摘要仍可比较。
        volatile = {"workdir", "resume", "interrupt_after"}
        identity = {
            key: value
            for key, value in dataclasses.asdict(self.cfg).items()
            if key not in volatile
        }
        return config_digest(identity)

    def _scan_all(self) -> dict:
        from .digest import read_json

        scanned: dict[int, dict] = {}
        for i in range(self.cfg.representations):
            codec = JobConfig._codec(i)
            rel = f"fixtures/rep{i}.{codec.value}"
            if ensure_within(self.cfg.workdir, rel, self.errors) is None:
                continue
            data = Path(self._path(rel)).read_bytes()
            book = ErrorBook()
            units = scan_bitstream(data, codec, source=rel, errors=book)
            raw_entries = [
                KeyframeEntry(**e)
                for e in read_json(f"fixtures/rep{i}.index.json", self.cfg.workdir)
            ]
            entries = validate_keyframe_index(
                raw_entries, units, len(data), source=rel, errors=book
            )
            clock = MediaClock(self.cfg.media_fps)
            timeline = build_timeline(entries, clock, source=rel, errors=book)
            scanned[i] = {
                "codec": codec,
                "units": units,
                "entries": entries,
                "timeline": timeline,
                "book": book,
            }
            self.errors.merge(book)
        return scanned

    def _path(self, rel: str) -> Path:
        return ensure_within(self.cfg.workdir, rel, self.errors)  # type: ignore[return-value]


    def _normalize_audio_and_color(self) -> None:
        run_audio_stage(
            self.cfg.workdir,
            self.cfg.target_sample_rate,
            self.cfg.target_layout,
            self.errors,
        )
        run_color_stage(self.cfg.workdir, self.cfg.representations, self.errors)


    def _plan_segments(self, scanned: dict):
        reps = []
        for i, payload in sorted(scanned.items()):
            clock = MediaClock(self.cfg.media_fps)
            times = [clock.tick_to_ms(e.tick) for e in payload["entries"]]
            duration = clock.tick_to_ms(self.cfg.media_frames)
            reps.append(
                Representation(
                    name=f"rep{i}", keyframe_ms=times, duration_ms=duration
                )
            )
        return plan_cuts(
            reps,
            segment_ms=self.cfg.segment_ms,
            max_deviation_ms=self.cfg.max_deviation_ms,
            errors=self.errors,
        )


    def _index_records(self, scanned: dict, plan) -> list[dict]:
        def q(value) -> dict:
            return {"num": value.numerator, "den": value.denominator}

        records = [
            {
                "type": "segment",
                "sequence": seg.sequence,
                "start_ms": q(seg.start_ms),
                "end_ms": q(seg.end_ms),
                "cuts": {name: q(t) for name, t in sorted(seg.cuts.items())},
            }
            for seg in plan.segments
        ]
        # 追加每个表示层的关键帧索引记录，保证分块数量可支撑多块中断/恢复。
        for i, payload in sorted(scanned.items()):
            clock = MediaClock(self.cfg.media_fps)
            for entry in payload["entries"]:
                records.append(
                    {
                        "type": "keyframe",
                        "representation": f"rep{i}",
                        "offset": entry.offset,
                        "tick": entry.tick,
                        "time_ms": q(clock.tick_to_ms(entry.tick)),
                    }
                )
        return records

    def _commit_blocks(self, records: list[dict], token: CancellationToken,
                       resume_from: int) -> int:
        size = max(1, self.cfg.block_records)
        committed = resume_from
        for seq in range(resume_from, self._block_count(records, size)):
            if token.cancelled:
                break
            block = Block.build(seq, records[seq * size : (seq + 1) * size])
            if self.cfg.interrupt_after and committed >= self.cfg.interrupt_after:
                self._inject_interrupt(seq, block)
                token.cancel()
                break
            self.store.commit_block(block)
            committed += 1
        return committed

    def _inject_interrupt(self, seq: int, block: Block) -> None:
        # 模拟块写入中断：只留截断的临时文件，不做原子替换。
        tmp = self.store.blocks_dir / f"{seq:06d}.tmp"
        tmp.write_bytes(canonical_json(block.to_dict())[: 4 + seq])

    @staticmethod
    def _block_count(records: list[dict], size: int) -> int:
        return (len(records) + size - 1) // size


    def _publish(self, plan, records: list[dict], input_sum: dict, cfg_sum: str):
        manifest = plan_to_dict(plan)
        manifest_path = write_json("manifest.json", self.cfg.workdir, manifest)
        index_payload = {
            "records": records,
            "input_digest": input_sum,
            "config_digest": cfg_sum,
        }
        index_path = write_json("index.json", self.cfg.workdir, index_payload)
        return manifest_path, index_path


    @staticmethod
    def _aligned_label(plan) -> str:
        return f"{len(plan.aligned_representations)}/{plan.total_representations}"

    def _fail(self, code: int, started: float) -> PipelineResult:
        return PipelineResult(
            exit_code=code,
            segments=0,
            aligned_representations="0/0",
            resume_ms=int((time.perf_counter() - started) * 1000),
            corrupt_offset_bytes=self.cfg.corrupt_offset,
            errors=self.errors,
        )
