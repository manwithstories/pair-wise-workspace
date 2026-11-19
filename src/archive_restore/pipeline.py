"""Pipeline orchestration: parse -> repair -> index -> verify, resumable.

Every stage persists its result through checkpoint.py; an interrupted
run reuses completed stages when the input digest and parameters still
match, and refuses to resume otherwise.
"""

from __future__ import annotations

import hashlib
import json
import os
import time
from dataclasses import asdict

from . import checkpoint, index as index_mod, rtp
from .model import (
    MEDIA_CLOCK_RATE,
    PacketError,
    RepairResult,
    RtpPacket,
    SenderReport,
    Stage,
    UncertainInterval,
)
from .probe import probe_payload
from .repair import repair_packets

EXIT_OK = 0
EXIT_INPUT = 2
EXIT_CHECKPOINT = 3
EXIT_VERIFY = 4

FALLBACK_STRATEGIES = ("zero",)


def _packet_to_dict(p: RtpPacket) -> dict:
    return asdict(p)


def _packet_from_dict(d: dict) -> RtpPacket:
    return RtpPacket(**d)


def _sr_to_dict(s: SenderReport) -> dict:
    return asdict(s)


def _sr_from_dict(d: dict) -> SenderReport:
    return SenderReport(**d)


def _repair_to_payload(r: RepairResult) -> dict:
    return {
        "order": r.order,
        "ext_seq": {str(k): v for k, v in r.ext_seq.items()},
        "ext_ts": {str(k): v for k, v in r.ext_ts.items()},
        "duplicates": r.duplicates,
        "reordered": r.reordered,
        "lost": r.lost,
        "out_of_window": r.out_of_window,
        "uncertain": [asdict(u) for u in r.uncertain],
        "errors": [e.to_dict() for e in r.errors],
    }


def _repair_from_payload(d: dict) -> RepairResult:
    return RepairResult(
        order=[int(i) for i in d["order"]],
        ext_seq={int(k): int(v) for k, v in d["ext_seq"].items()},
        ext_ts={int(k): int(v) for k, v in d["ext_ts"].items()},
        duplicates=int(d["duplicates"]),
        reordered=int(d["reordered"]),
        lost=int(d["lost"]),
        out_of_window=int(d["out_of_window"]),
        uncertain=[UncertainInterval(**u) for u in d["uncertain"]],
        errors=[PacketError.from_dict(e) for e in d["errors"]],
    )


def _clock_mapper(sender_reports: list[SenderReport], fallback: str):
    """Return (map_fn, clock_source): extended ts -> seconds or None."""
    if not sender_reports:
        if fallback not in FALLBACK_STRATEGIES:
            raise ValueError(f"unknown fallback strategy: {fallback}")
        return None, "derived"
    srs = sorted(sender_reports, key=lambda s: s.rtp_timestamp)
    # Extend SR rtp timestamps into a common monotonic domain.
    ext = [srs[0].rtp_timestamp]
    for prev, sr in zip(srs, srs[1:]):
        delta = (sr.rtp_timestamp - prev.rtp_timestamp) % (1 << 32)
        ext.append(ext[-1] + delta)

    def map_ts(ts_ext: int) -> float:
        if ts_ext <= ext[0]:
            base = srs[0]
            return base.ntp_seconds + (ts_ext - ext[0]) / MEDIA_CLOCK_RATE
        for i in range(1, len(ext)):
            if ts_ext <= ext[i]:
                span = ext[i] - ext[i - 1]
                frac = 0.0 if span == 0 else (ts_ext - ext[i - 1]) / span
                return srs[i - 1].ntp_seconds + frac * (
                    srs[i].ntp_seconds - srs[i - 1].ntp_seconds
                )
        return srs[-1].ntp_seconds + (ts_ext - ext[-1]) / MEDIA_CLOCK_RATE

    return map_ts, "reported"


def run_restore(
    input_path: str,
    index_path: str,
    state_dir: str,
    window: int = 64,
    fallback: str = "zero",
    output_path: str | None = None,
) -> tuple[dict, int]:
    """Run the full recovery pipeline; returns (summary, exit_code)."""
    started = time.monotonic()
    try:
        with open(input_path, "rb") as fh:
            data = fh.read()
    except OSError as exc:
        return {"error": f"cannot read input: {exc}"}, EXIT_INPUT
    input_sha = hashlib.sha256(data).hexdigest()
    params = {"window": window, "fallback": fallback}
    try:
        manifest, fresh = checkpoint.open_run(state_dir, input_sha, params)
    except checkpoint.CheckpointMismatch as exc:
        return {"error": f"checkpoint mismatch: {exc}"}, EXIT_CHECKPOINT
    except ValueError as exc:
        return {"error": f"corrupt checkpoint: {exc}"}, EXIT_CHECKPOINT

    cached: dict[str, bool] = {}

    def stage(name: str, compute):
        payload = None if fresh else checkpoint.load_stage(state_dir, manifest, name)
        if payload is not None:
            cached[name] = True
            return payload
        cached[name] = False
        payload = compute()
        checkpoint.save_stage(state_dir, manifest, name, payload)
        return payload

    # ---- parse ------------------------------------------------------
    def do_parse() -> dict:
        res = rtp.parse_stream(data)
        return {
            "packets": [_packet_to_dict(p) for p in res.packets],
            "sender_reports": [_sr_to_dict(s) for s in res.sender_reports],
            "errors": [e.to_dict() for e in res.errors],
            "records_seen": res.records_seen,
        }

    p = stage("parse", do_parse)
    packets = [_packet_from_dict(d) for d in p["packets"]]
    sender_reports = [_sr_from_dict(d) for d in p["sender_reports"]]
    parse_errors = [PacketError.from_dict(d) for d in p["errors"]]

    # ---- repair -----------------------------------------------------
    rep = _repair_from_payload(
        stage("repair", lambda: _repair_to_payload(repair_packets(packets, window)))
    )

    # ---- index ------------------------------------------------------
    def do_index() -> dict:
        map_fn, clock_source = _clock_mapper(sender_reports, fallback)
        if map_fn is None:
            if not rep.order:
                raise ValueError("no packets to index")
            ts0 = rep.ext_ts[rep.order[0]]

            def map_fn(ts_ext: int) -> float:  # type: ignore[assignment]
                return (ts_ext - ts0) / MEDIA_CLOCK_RATE

        uncertain = [(u.ext_seq_start, u.ext_seq_end) for u in rep.uncertain]

        def is_uncertain(ext_seq: int) -> bool:
            return any(a <= ext_seq <= b for a, b in uncertain)

        entries = []
        for idx in rep.order:
            es = rep.ext_seq[idx]
            t = None if is_uncertain(es) else map_fn(rep.ext_ts[idx])
            entries.append(
                index_mod.build_entry(packets[idx], es, rep.ext_ts[idx], t)
            )
        salvaged = 0
        if os.path.exists(index_path):
            try:
                old, _damaged = index_mod.load_index_tolerant(index_path)
                valid, _failed = index_mod.validate_entries(old, data)
                have = {(e.ssrc, e.ext_seq) for e in entries}
                for e in valid:
                    key = (int(e["ssrc"]), int(e["ext_seq"]))
                    if key not in have:
                        entries.append(index_mod.IndexEntry(**{
                            k: e[k] for k in
                            ("ssrc", "ext_seq", "timestamp", "time_seconds",
                             "offset", "length")}))
                        salvaged += 1
            except ValueError:
                pass
        entries.sort(key=lambda e: (e.ssrc, e.ext_seq))
        doc = index_mod.index_document(entries, input_sha)
        index_mod.write_index_atomic(index_path, doc)
        return {"entries": [asdict(e) for e in entries],
                "clock_source": clock_source, "salvaged": salvaged}

    try:
        idx_payload = stage("index", do_index)
    except ValueError as exc:
        return {"error": f"index stage failed: {exc}"}, EXIT_INPUT
    entries = idx_payload["entries"]

    # ---- verify -----------------------------------------------------
    def do_verify() -> dict:
        valid, failed = index_mod.validate_entries(entries, data)
        nonmono = index_mod.check_monotonic_time(entries)
        out = bytearray()
        payload_bytes = bytearray()
        for idx in rep.order:
            pkt = packets[idx]
            rec = data[pkt.offset : pkt.offset + pkt.length]
            out += len(rec).to_bytes(2, "big") + rec
            payload_bytes += data[
                pkt.payload_offset : pkt.payload_offset + pkt.payload_length
            ]
        probe = probe_payload(bytes(payload_bytes))
        return {
            "valid_entries": len(valid),
            "failed_entries": [list(f) for f in failed],
            "nonmonotonic_rows": nonmono,
            "output_sha256": hashlib.sha256(bytes(out)).hexdigest(),
            "output_bytes": len(out),
            "probe_ok": probe.ok,
            "probe_details": probe.details,
            "probe_duration_seconds": probe.duration_seconds,
        }

    v = stage("verify", do_verify)

    if output_path is None:
        base, _ext = os.path.splitext(index_path)
        output_path = base + ".recovered.rtp"
    _write_output_atomic(output_path, data, packets, rep.order)

    errors = [e.to_dict() for e in parse_errors] + [e.to_dict() for e in rep.errors]
    first_error_offset = min((e["offset_start"] for e in errors), default=None)
    ok = (
        not v["failed_entries"]
        and not v["nonmonotonic_rows"]
        and v["probe_ok"]
    )
    summary = {
        "packets_seen": p["records_seen"],
        "packets_parsed": len(packets),
        "packets_reordered": rep.reordered,
        "packets_lost": rep.lost,
        "packets_duplicated": rep.duplicates,
        "packets_out_of_window": rep.out_of_window,
        "errors": errors,
        "error_count": len(errors),
        "first_error_offset": first_error_offset,
        "index_entries": len(entries),
        "index_salvaged_entries": idx_payload["salvaged"],
        "clock_source": idx_payload["clock_source"],
        "uncertain_intervals": [asdict(u) for u in rep.uncertain],
        "resumed": bool(cached) and all(cached.values()),
        "stages_cached": cached,
        "resume_seconds": round(time.monotonic() - started, 3),
        "output_path": output_path,
        "output_sha256": v["output_sha256"],
        "verify": {
            "valid_entries": v["valid_entries"],
            "failed_entries": len(v["failed_entries"]),
            "nonmonotonic_rows": len(v["nonmonotonic_rows"]),
            "probe_ok": v["probe_ok"],
            "probe_duration_seconds": v["probe_duration_seconds"],
        },
    }
    return summary, EXIT_OK if ok else EXIT_VERIFY


def _write_output_atomic(path: str, data: bytes, packets: list[RtpPacket],
                         order: list[int]) -> None:
    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "wb") as fh:
        for idx in order:
            pkt = packets[idx]
            rec = data[pkt.offset : pkt.offset + pkt.length]
            fh.write(len(rec).to_bytes(2, "big"))
            fh.write(rec)
    os.replace(tmp, path)


def run_verify(input_path: str, index_path: str) -> tuple[dict, int]:
    """Validate an index against its input stream."""
    try:
        with open(input_path, "rb") as fh:
            data = fh.read()
    except OSError as exc:
        return {"error": f"cannot read input: {exc}"}, EXIT_INPUT
    try:
        entries, damaged = index_mod.load_index_tolerant(index_path)
    except (OSError, ValueError) as exc:
        return {"error": f"cannot load index: {exc}"}, EXIT_INPUT
    valid, failed = index_mod.validate_entries(entries, data)
    nonmono = index_mod.check_monotonic_time(entries)
    summary = {
        "index_entries": len(entries),
        "index_damaged": damaged,
        "valid_entries": len(valid),
        "failed_entries": len(failed),
        "failures": [{"row": r, "reason": why} for r, why in failed],
        "nonmonotonic_rows": len(nonmono),
    }
    ok = not failed and not nonmono
    return summary, EXIT_OK if ok else EXIT_INPUT
