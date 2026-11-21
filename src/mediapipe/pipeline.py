"""流水线编排：解析 → 有界队列 → 映射变换 → 写出 → 回读验证。

线程模型：
* 生产者线程跑 Demuxer，逐包放入 :class:`BoundedQueue`，慢消费者时
  在队列上阻塞（或 ABORT 取消），内存占用有界；
* 主线程为消费者，按映射写 Remuxer，在包边界持久化检查点；
* 任一阶段抛错即停止下游、关闭队列与文件，保留 *首个* 错误上下文。

续跑：检查点指纹通过后，先解复用并重放至 ``committed_sequence``
（仅计算 SHA 核对，不写出），再从下一个包继续，保证输出与一次跑成等价。
"""
from __future__ import annotations

import os
import threading
import time
from dataclasses import dataclass, field
from typing import Any

from .buffer import BackpressurePolicy, BoundedQueue, QueueStats
from .checkpoint import (
    CheckpointState,
    CheckpointStore,
    StreamState,
    file_fingerprint,
    mapping_fingerprint,
)
from .demux import Demuxer
from .errors import CheckpointIncompatibleError, MediaError, Phase, PipelineInternalError
from .mapping import StreamMapping, validate_mapping
from .packet import PacketRecord
from .remux import Remuxer
from .validate import VerificationReport, verify_output


@dataclass(slots=True)
class PipelineConfig:
    input_path: str
    output_path: str
    checkpoint_path: str
    queue_bytes: int = 1 << 20
    queue_packets: int | None = None
    slow_consumer_ms: float = 0.0
    checkpoint_every: int = 50
    verify: bool = True
    backpressure_abort: bool = False


@dataclass(slots=True)
class PipelineResult:
    exit_code: int
    input_bytes: int
    streams: list[dict[str, Any]]
    packets: int
    resumed: bool
    resume_ms: float
    resumed_from_sequence: int | None
    queue: dict[str, Any]
    remux: dict[str, Any] | None = None
    verification: dict[str, Any] | None = None
    metadata_not_preserved: list[str] = field(default_factory=list)
    error: dict[str, Any] | None = None


class Pipeline:
    def __init__(self, config: PipelineConfig, mappings: list[StreamMapping]) -> None:
        self.cfg = config
        self.mappings = mappings
        self.queue = BoundedQueue(
            max_bytes=config.queue_bytes,
            max_packets=config.queue_packets,
            policy=(
                BackpressurePolicy.ABORT
                if config.backpressure_abort
                else BackpressurePolicy.BLOCK
            ),
        )
        self._producer_error: MediaError | None = None
        self._first_error: MediaError | None = None
        self._error_lock = threading.Lock()
        self._digests: list[str] = []

    def _record_error(self, exc: MediaError) -> None:
        with self._error_lock:
            if self._first_error is None:
                self._first_error = exc

    # ---- 生产者 -------------------------------------------------------
    def _produce(self, demuxer: Demuxer, skip_to: int) -> None:
        try:
            for record in demuxer.iter_packets():
                if record.sequence <= skip_to:
                    continue
                self.queue.put(record)
        except MediaError as exc:
            self._producer_error = exc
            self._record_error(exc)
        except Exception as exc:  # 未预期异常包装为内部错误，保上下文
            wrapped = PipelineInternalError(
                f"解析阶段未预期异常: {exc}", phase=Phase.DEMUX
            )
            self._producer_error = wrapped
            self._record_error(wrapped)
        finally:
            self.queue.close()

    # ---- 主流程 -------------------------------------------------------
    def run(self) -> PipelineResult:
        cfg = self.cfg
        started = time.monotonic()
        store = CheckpointStore(cfg.checkpoint_path, input_path=cfg.input_path)
        fingerprint = file_fingerprint(cfg.input_path)
        mfingerprint = mapping_fingerprint(self.mappings)

        saved = store.load() if store.exists() else None
        skip_to = -1
        resumed = False
        if saved is not None:
            store.verify_resume(saved, fingerprint, mfingerprint)
            skip_to = saved.committed_sequence
            resumed = True
            prior_records = self._replay_committed(saved)
        else:
            prior_records = []
        resume_ms = round((time.monotonic() - started) * 1000, 3)

        with Demuxer(cfg.input_path) as demuxer:
            infos = demuxer.stream_infos
            self.mappings = validate_mapping(self.mappings, infos)
            remuxer = Remuxer(
                cfg.output_path,
                demuxer.av_streams,
                self.mappings,
                input_path=cfg.input_path,
            )
            stream_states: dict[str, StreamState] = {
                str(m.source_index): StreamState() for m in self.mappings
            }
            producer = threading.Thread(
                target=self._produce, args=(demuxer, skip_to), name="demux-producer"
            )
            producer.start()
            remux_result = None
            verification: VerificationReport | None = None
            all_records: list[PacketRecord] = []
            committed = skip_to
            try:
                remuxer.open()
                # 续跑：fragmented 输出不可追加，重建容器后先重写已提交包，
                # 使最终输出与一次跑成等价；新包紧随其后，全局序号连续。
                for prior in prior_records:
                    remuxer.write(prior)
                    self._update_stream_state(stream_states, prior)
                while True:
                    record = self.queue.get(timeout=0.5)
                    if record is None:
                        break
                    if cfg.slow_consumer_ms > 0:
                        time.sleep(cfg.slow_consumer_ms / 1000.0)
                    remuxer.write(record)
                    all_records.append(record)
                    self._digests.append(record.payload.sha256())
                    committed = record.sequence
                    self._update_stream_state(stream_states, record)
                    if (record.sequence + 1) % max(1, cfg.checkpoint_every) == 0:
                        self._save_checkpoint(
                            store, fingerprint, mfingerprint,
                            stream_states, committed, remuxer,
                        )
                producer.join(timeout=1.0)
                if self._producer_error is not None:
                    raise self._producer_error
                remux_result = remuxer.close()
                self._save_checkpoint(
                    store, fingerprint, mfingerprint,
                    stream_states, committed, remuxer, final=True,
                )
                if cfg.verify:
                    full_records = prior_records + all_records
                    verification = verify_output(
                        cfg.output_path, full_records, self.mappings
                    )
            except BaseException:
                self._shutdown(remuxer, producer)
                raise
            finally:
                producer.join(timeout=1.0)

        return self._build_result(
            infos=infos,
            packets=len(prior_records) + len(all_records),
            resumed=resumed,
            resume_ms=resume_ms,
            resumed_from=skip_to if resumed else None,
            remux_result=remux_result,
            verification=verification,
        )

    # ---- 辅助 ---------------------------------------------------------
    def _replay_committed(self, saved: CheckpointState) -> list[PacketRecord]:
        """续跑前重放已提交包，逐包核对 SHA-256，返回已提交记录供全量验证。"""
        expected = list(saved.packet_digests)
        replayed: list[PacketRecord] = []
        with Demuxer(self.cfg.input_path) as replay:
            for record in replay.iter_packets():
                if record.sequence > saved.committed_sequence:
                    break
                replayed.append(record)
        got = [r.payload.sha256() for r in replayed]
        if got != expected:
            raise CheckpointIncompatibleError(
                "续跑重放校验失败：已提交包的逐包 SHA-256 与检查点不一致",
                phase=Phase.CHECKPOINT,
                detail={
                    "expected_packets": len(expected),
                    "replayed_packets": len(got),
                },
            )
        self._digests = list(expected)
        return replayed

    def _update_stream_state(
        self, states: dict[str, StreamState], record: PacketRecord
    ) -> None:
        st = states.get(str(record.stream_index))
        if st is None:
            return
        st.last_pts = record.pts
        st.last_dts = record.dts
        st.last_pts_us = record.pts_us
        st.last_dts_us = record.dts_us
        st.packet_count += 1

    def _save_checkpoint(
        self,
        store: CheckpointStore,
        fingerprint: dict[str, Any],
        mfingerprint: str,
        states: dict[str, StreamState],
        committed: int,
        remuxer: Remuxer,
        *,
        final: bool = False,
    ) -> None:
        state = CheckpointState(
            version=1,
            input_fingerprint=fingerprint,
            mapping_fingerprint=mfingerprint,
            committed_sequence=committed,
            stream_states=dict(states),
            output_bytes=os.path.getsize(remuxer.output_path)
            if os.path.exists(remuxer.output_path)
            else 0,
            packet_digests=list(self._digests),
        )
        store.save(state)

    def _shutdown(self, remuxer: Remuxer, producer: threading.Thread) -> None:
        # 停止下游：关闭队列解除生产者阻塞，安全关闭输出文件
        self.queue.close()
        producer.join(timeout=2.0)
        try:
            if remuxer._container is not None:
                remuxer._container.close()
                remuxer._container = None
        except Exception:
            pass  # 关闭错误不掩盖首个业务错误

    def _build_result(
        self,
        *,
        infos: list[Any],
        packets: int,
        resumed: bool,
        resume_ms: float,
        resumed_from: int | None,
        remux_result: Any,
        verification: VerificationReport | None,
    ) -> PipelineResult:
        return PipelineResult(
            exit_code=0,
            input_bytes=os.path.getsize(self.cfg.input_path),
            streams=[i.to_dict() for i in infos],
            packets=packets,
            resumed=resumed,
            resume_ms=resume_ms,
            resumed_from_sequence=resumed_from,
            queue=self.queue.stats.to_dict(),
            remux=remux_result.to_dict() if remux_result else None,
            verification=verification.to_dict() if verification else None,
            metadata_not_preserved=(
                remux_result.metadata_not_preserved if remux_result else []
            ),
        )
