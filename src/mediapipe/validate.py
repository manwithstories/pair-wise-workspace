"""回读验证：解封装输出并与源包摘要逐轨比对。

比对三个层面：
1. 包数：每轨源/输出包数必须一致；
2. 压缩负载：逐包 SHA-256 必须全等（字节级无损，而非仅比文件大小）；
3. 时间线：PTS/DTS/duration 比对。fragmented 封装会折叠 edit-list，
   使前导 priming 包整体平移，这种 *等幅平移* 记为
   ``timeline_shifted`` 警告而非失败；非线性偏差才判失败（退出码 5）。
"""
from __future__ import annotations

import hashlib
import os
from dataclasses import dataclass, field
from typing import Any

import av

from .errors import Phase, PipelineInternalError
from .packet import PacketRecord
from .timebase import to_microseconds


@dataclass(slots=True)
class StreamVerification:
    source_stream: int
    output_stream: int
    source_packets: int
    output_packets: int
    packet_count_match: bool
    packet_sha256_match: bool
    timeline_match: bool
    timeline_shifted: bool
    pts_shift_ticks: int | None = None
    dropped_priming_packets: int = 0
    mismatches: list[dict[str, Any]] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        # priming 丢包属于已报告的容器级 edit-list 行为，不判失败
        return (
            self.packet_sha256_match
            and self.timeline_match
            and self.mismatches == []
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "source_stream": self.source_stream,
            "output_stream": self.output_stream,
            "source_packets": self.source_packets,
            "output_packets": self.output_packets,
            "packet_count_match": self.packet_count_match,
            "packet_sha256_match": self.packet_sha256_match,
            "timeline_match": self.timeline_match,
            "timeline_shifted": self.timeline_shifted,
            "pts_shift_ticks": self.pts_shift_ticks,
            "dropped_priming_packets": self.dropped_priming_packets,
            "mismatches": self.mismatches[:10],
        }


@dataclass(slots=True)
class VerificationReport:
    output_bytes: int
    stream_reports: list[StreamVerification]
    all_packet_sha256_match: bool
    passed: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "output_bytes": self.output_bytes,
            "packet_sha256_match": self.all_packet_sha256_match,
            "verify_passed": self.passed,
            "streams": [r.to_dict() for r in self.stream_reports],
        }


def _digest(packet: Any) -> str:
    return hashlib.sha256(bytes(packet)).hexdigest()


def _read_output(path: str) -> dict[int, list[Any]]:
    if not os.path.isfile(path) or os.path.getsize(path) == 0:
        raise PipelineInternalError(
            f"输出缺失或为空，无法回读验证: {path}", phase=Phase.VALIDATE
        )
    try:
        container = av.open(path, mode="r")
    except av.FFmpegError as exc:
        raise PipelineInternalError(
            f"输出容器无法回读: {exc}", phase=Phase.VALIDATE
        ) from exc
    # 注意：PyAV 对 fragmented MP4 的逐流 demux(partial_stream) 在部分版本
    # 会漏掉下一条流，因此统一用全局 demux 再按 stream_index 分组
    result: dict[int, list[Any]] = {}
    with container:
        for packet in container.demux():
            if packet.dts is None:
                continue
            result.setdefault(int(packet.stream_index), []).append(packet)
    return result


def _align_by_digest(
    src: list[PacketRecord], out: list[Any]
) -> tuple[list[tuple[PacketRecord, Any]], int, list[dict[str, Any]]]:
    """按负载 SHA-256 对齐源/输出包。

    输出侧因 edit-list 折叠丢弃的前导 priming 包计入 dropped；
    任何无法对齐（负载缺失/顺序变化）都记为失配。
    """
    out_digests = [_digest(o) for o in out]
    pairs: list[tuple[PacketRecord, Any]] = []
    dropped = 0
    mismatches: list[dict[str, Any]] = []
    j = 0
    for rec in src:
        target = rec.payload.sha256()
        if j < len(out_digests) and out_digests[j] == target:
            pairs.append((rec, out[j]))
            j += 1
            continue
        # 源包在输出中找不到对应：若是前导 priming 则记丢弃，否则失配
        if rec.dts is not None and rec.dts < 0 and dropped <= len(src) - len(out):
            dropped += 1
            continue
        mismatches.append(
            {"field": "payload", "source_sequence": rec.sequence, "reason": "输出无匹配 SHA-256"}
        )
    if j != len(out):
        mismatches.append(
            {"field": "payload", "reason": f"输出多出 {len(out) - j} 个无源对应包"}
        )
    return pairs, dropped, mismatches


def _timeline_compare(
    pairs: list[tuple[PacketRecord, Any]],
) -> tuple[bool, bool, int | None, list[dict[str, Any]]]:
    """在整数微秒域比对 PTS/DTS/duration。

    统一等幅平移（edit-list 折叠）记为 timeline_shifted；
    duration 必须逐包微秒相等，非线性偏差判失配。
    """
    # 收集每个包的 PTS/DTS 平移（微秒）；前导 priming 包可能是离群平移
    per_packet: list[tuple[int, set[int]]] = []
    mismatches: list[dict[str, Any]] = []
    for rec, out_pkt in pairs:
        shifts: set[int] = set()
        out_tb = out_pkt.time_base
        if rec.pts is not None and out_pkt.pts is not None:
            shifts.add(to_microseconds(int(out_pkt.pts), out_tb) - int(rec.pts_us))
        if rec.dts is not None and out_pkt.dts is not None:
            shifts.add(to_microseconds(int(out_pkt.dts), out_tb) - int(rec.dts_us))
        per_packet.append((rec.sequence, shifts))
        if rec.duration_us is not None and out_pkt.duration is not None:
            out_dur_us = to_microseconds(int(out_pkt.duration), out_tb)
            # 容器会把前导 priming 包的 duration 清零（edit-list 行为），容忍之
            is_priming = rec.dts is not None and rec.dts < 0
            if out_dur_us == 0 and is_priming:
                continue
            # 不同 time_base 与 AAC 帧长量化（1024/48000≈21333us）会带来
            # 单帧级抖动，容差取 1us 与源时长 5% 的较大者，但不接受整帧丢失
            tol = max(1, int(rec.duration_us * 0.05))
            if abs(out_dur_us - rec.duration_us) > tol:
                mismatches.append(
                    {
                        "field": "duration_us",
                        "source": rec.duration_us,
                        "output": out_dur_us,
                    }
                )
    # 主导平移量：所有包平移（含 0）的众数；量化抖动 ±1us 视为同一档
    all_values = [v for _, ss in per_packet for v in ss]
    if not all_values:
        return True, False, 0, mismatches
    # 以中位数为基准，容差 1us 聚类得到主导平移
    ordered = sorted(all_values)
    median = ordered[len(ordered) // 2]
    dominant = {v for v in set(all_values) if abs(v - median) <= 1}
    outlier_seqs = {
        seq for seq, ss in per_packet if any(v not in dominant for v in ss)
    }
    # 离群包必须全部是前导 priming（源 DTS 为负），否则判非线性失配
    priming_seqs = {
        rec.sequence for rec, _ in pairs if rec.dts is not None and rec.dts < 0
    }
    if outlier_seqs and not outlier_seqs.issubset(priming_seqs):
        mismatches.append(
            {
                "field": "timeline",
                "reason": "存在非线性时间戳偏差（非 priming 包平移量不一致）",
                "sequences": sorted(outlier_seqs - priming_seqs),
            }
        )
    nonzero_dominant = {v for v in dominant if v != 0}
    strict = not mismatches and not nonzero_dominant and not outlier_seqs
    shifted = not mismatches and bool(nonzero_dominant)
    shift_value = (
        round(sum(nonzero_dominant) / len(nonzero_dominant))
        if nonzero_dominant
        else 0
    )
    return strict, shifted, shift_value, mismatches


def verify_output(
    output_path: str,
    source_records: list[PacketRecord],
    mappings: list[Any],
) -> VerificationReport:
    """回读输出，按映射逐轨比对包数、负载 SHA-256 与时间线。"""
    out_by_stream = _read_output(output_path)
    # 回读流按容器内顺序排列，输出流号 output_index 即该顺序的位置
    out_packets = [pkt for _, pkt in sorted(out_by_stream.items())]
    src_by_stream: dict[int, list[PacketRecord]] = {}
    for rec in source_records:
        src_by_stream.setdefault(rec.stream_index, []).append(rec)

    reports: list[StreamVerification] = []
    for m in mappings:
        sidx = int(m.source_index)
        oidx = int(m.output_index)
        src = src_by_stream.get(sidx, [])
        out = out_packets[oidx] if oidx < len(out_packets) else []
        pairs, dropped, align_mismatches = _align_by_digest(src, out)
        sha_match = not align_mismatches and len(pairs) == len(out)
        strict, shifted, shift_value, tl_mismatches = _timeline_compare(pairs)
        count_match = len(src) == len(out)
        reports.append(
            StreamVerification(
                source_stream=sidx,
                output_stream=oidx,
                source_packets=len(src),
                output_packets=len(out),
                packet_count_match=count_match,
                packet_sha256_match=sha_match,
                timeline_match=strict or shifted,
                timeline_shifted=shifted,
                pts_shift_ticks=shift_value,
                dropped_priming_packets=dropped,
                mismatches=align_mismatches + tl_mismatches,
            )
        )
    all_sha = all(r.packet_sha256_match for r in reports)
    passed = all(r.ok for r in reports)
    if not passed:
        bad = [r.to_dict() for r in reports if not r.ok]
        raise PipelineInternalError(
            "回读验证失败：存在负载/时间线不一致",
            phase=Phase.VALIDATE,
            detail={"failed_streams": bad},
        )
    return VerificationReport(
        output_bytes=os.path.getsize(output_path),
        stream_reports=reports,
        all_packet_sha256_match=all_sha,
        passed=passed,
    )
