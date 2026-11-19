"""Per-asset pipeline: probe -> recover -> map -> remux -> verify -> report.

Each asset gets its own temporary output directory and is committed with an
atomic rename only after verification.  A batch interruption therefore leaves
either a complete output or no output -- never a half-written MP4 for an
engineer to trip over later.

Resume: an asset is skipped when a cache entry exists whose key covers both the
input's SHA-256 *and* the processing strategy (mapping, limits, tool version,
thresholds).  Same input + same policy reuses the result; any policy change
misses the cache and re-processes.
"""

from __future__ import annotations

import hashlib
import json
import shutil
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import av

from . import __version__, container, recover
from .demux import (
    MAX_ASSET_BYTES,
    MAX_TRACKS,
    AssetProbe,
    RawPacket,
    check_lossless_support,
    probe_asset,
    validate_asset_path,
)
from .errors import (
    AssetTimeoutError,
    LosslessCopyRefusedError,
    MediaLocation,
    QCError,
    RecoverableError,
    UnrecoverableMediaError,
    UsageError,
)
from .recover import decode_health, recover_asset
from .remux import atomic_output, packet_digest, remux_asset
from .report import (
    STATUS_FAILED,
    STATUS_OK,
    STATUS_PARTIAL,
    STATUS_RECOVERED,
    STATUS_REFUSED,
    AssetResult,
)
from .sync import measure_sync
from .timeline import analyse_timelines, build_stream_timeline
from .trackmap import build_track_graph, describe_mapping, parse_rules, stable_automatic_rules


@dataclass(slots=True)
class PipelineOptions:
    workdir: Path
    output_dir: Path
    cache_dir: Path
    track_map: list[dict[str, Any]] | None = None
    timeout_s: float = 60.0
    max_bytes: int = MAX_ASSET_BYTES
    max_tracks: int = MAX_TRACKS
    reuse: bool = True
    sync_markers: tuple[float, ...] = ()
    fps: float = 25.0
    decode_check: bool = True

    def strategy_fingerprint(self) -> dict[str, Any]:
        rules = self.track_map if self.track_map is not None else "auto"
        return {
            "tool": "bcastqc",
            "version": __version__,
            "rules": rules,
            "timeout_s": self.timeout_s,
            "max_bytes": self.max_bytes,
            "max_tracks": self.max_tracks,
            "decode_check": self.decode_check,
            "sync_markers": list(self.sync_markers),
        }


@dataclass(slots=True)
class PipelineResult:
    result: AssetResult
    reused: bool = False
    exit_fatal: bool = False


def _sha256_file(path: Path, *, chunk: int = 1 << 20) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while True:
            block = stream.read(chunk)
            if not block:
                break
            digest.update(block)
    return digest.hexdigest()


def cache_key(input_digest: str, fingerprint: dict[str, Any]) -> str:
    payload = json.dumps(
        {"input": input_digest, "strategy": fingerprint},
        sort_keys=True,
        separators=(",", ":"),
    )
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def _cache_paths(cache_dir: Path, key: str) -> tuple[Path, Path]:
    return cache_dir / f"{key}.json", cache_dir / f"{key}.mp4"


def load_cached(
    cache_dir: Path, key: str, suffix: str = ".mp4"
) -> tuple[dict[str, Any], Path] | None:
    meta, media = _cache_paths(cache_dir, key)
    if suffix != ".mp4":
        media = media.with_suffix(suffix)
    if not meta.exists() or not media.exists():
        return None
    try:
        payload = json.loads(meta.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    if payload.get("media_size") not in (None, media.stat().st_size):
        return None
    with media.open("rb") as stream:
        header = stream.read(1 << 16)
    if not container.has_valid_boxes(header):
        return None
    return payload, media


def store_cache(
    cache_dir: Path, key: str, payload: dict[str, Any], media: Path
) -> Path:
    cache_dir.mkdir(parents=True, exist_ok=True)
    meta, target = _cache_paths(cache_dir, key)
    target = target.with_suffix(media.suffix)
    shutil.copyfile(media, target)
    payload = dict(payload)
    payload["media_size"] = target.stat().st_size
    payload["media"] = target.name
    temporary = meta.with_suffix(".json.tmp")
    temporary.write_text(json.dumps(payload, indent=2), encoding="utf-8")
    temporary.replace(meta)
    return target


def _compare_packets(
    source: list[RawPacket],
    output_path: Path,
    stream_map: dict[int, int],
) -> tuple[int, int, list[dict[str, Any]]]:
    """Count byte-identical packets between the kept list and the written file.

    ``stream_map`` maps a source stream index to the *output* stream index the
    remuxer gave it.  The two differ whenever the graph reorders tracks (video
    first is the normal case), so comparing by source index would pit AAC
    payloads against H.264 ones and report a perfect copy as zero matches.

    Comparison is per stream, in order: an interleaving change by the muxer is
    not a mismatch, any payload difference is.
    """
    expected: dict[int, list[bytes]] = {}
    for packet in source:
        expected.setdefault(packet.stream, []).append(packet.data)
    actual: dict[int, list[bytes]] = {}
    try:
        with av.open(str(output_path), mode="r", metadata_errors="ignore") as out:
            for packet in out.demux():
                if packet.size == 0:
                    continue
                actual.setdefault(packet.stream.index, []).append(bytes(packet))
    except (av.error.FFmpegError, OSError, ValueError):
        return 0, len(source), [{"error": "output not readable"}]

    matched = 0
    total = 0
    mismatches: list[dict[str, Any]] = []
    consumed: set[int] = set()
    for source_stream, expected_packets in expected.items():
        output_stream = stream_map.get(source_stream, source_stream)
        consumed.add(output_stream)
        actual_packets = actual.get(output_stream, [])
        total += len(expected_packets)
        for index, payload in enumerate(expected_packets):
            if index < len(actual_packets) and actual_packets[index] == payload:
                matched += 1
            elif len(mismatches) < 8:
                mismatches.append(
                    {
                        "source_stream": source_stream,
                        "output_stream": output_stream,
                        "packet": index,
                        "expected_size": len(payload),
                        "actual_size": (
                            len(actual_packets[index]) if index < len(actual_packets) else None
                        ),
                    }
                )
    extra = sum(len(v) for k, v in actual.items() if k not in consumed)
    if extra and len(mismatches) < 8:
        mismatches.append({"extra_packets": extra})
    return matched, total, mismatches


def _publish_cached(cached: Path, output_dir: Path, source: Path) -> Path:
    """Copy a cached remux back to ``output_dir`` under the stable name."""
    target = output_dir / f"{source.stem}.remux{source.suffix}"
    if target.exists() and target.stat().st_size == cached.stat().st_size:
        return target
    output_dir.mkdir(parents=True, exist_ok=True)
    sink = atomic_output(output_dir, target.name)
    try:
        shutil.copyfile(cached, sink.temp_path)
        sink.commit()
    except BaseException:
        sink.discard()
        raise
    return sink.final_path


def process_asset(
    path: Path, options: PipelineOptions
) -> PipelineResult:
    started = time.monotonic()
    deadline = started + options.timeout_s
    result = AssetResult(asset=path, strategy="lossless_copy", status=STATUS_FAILED)

    try:
        validate_asset_path(path, max_bytes=options.max_bytes)
    except UsageError as exc:
        result.status = STATUS_FAILED
        result.errors.append(exc)
        result.latency_ms = (time.monotonic() - started) * 1000.0
        return PipelineResult(result, exit_fatal=True)

    input_digest = _sha256_file(path)
    fingerprint = options.strategy_fingerprint()
    key = cache_key(input_digest, fingerprint)
    result.extra["input_sha256"] = input_digest
    result.extra["cache_key"] = key

    if options.reuse:
        cached = load_cached(options.cache_dir, key, suffix=path.suffix)
        if cached is not None:
            payload, media = cached
            # Re-materialise the cached bytes under the normal output name.
            # The report must point at the same stable path whether the asset
            # was just processed or reused, or an operator following the JSON
            # would be sent into the cache directory.
            published = _publish_cached(media, options.output_dir, path)
            result = _result_from_cache(payload, path, published)
            result.latency_ms = (time.monotonic() - started) * 1000.0
            result.cache = "hit"
            result.extra["cache_key"] = key
            result.extra["input_sha256"] = input_digest
            return PipelineResult(result, reused=True)

    result.cache = "miss"
    try:
        probe = probe_asset(path, max_tracks=options.max_tracks)
        result.tracks = len(probe.streams)
        result.duration_s = None if probe.duration is None else float(probe.duration)
        result.extra["probe"] = probe.to_dict()
        _, index_report = container.scan_file_header(path)
        result.extra["container"] = index_report.to_dict()

        rules = (
            parse_rules(options.track_map)
            if options.track_map is not None
            else stable_automatic_rules(probe)
        )
        graph = build_track_graph(probe, rules)
        result.mapping = describe_mapping(graph)
        result.codec_pairs = sorted(
            {f"{node.source.type}:{node.source.codec}" for node in graph.nodes}
        )

        if _timed_out(deadline):
            raise AssetTimeoutError("asset exceeded the processing timeout")
        outcome = recover_asset(probe, deadline=deadline)
        result.strategies = list(outcome.plan.strategies)
        result.recovery_rate = outcome.recovery_rate
        result.expected_packets = outcome.expected_packets
        result.extra["recovery"] = outcome.to_dict()

        # Continuity of the *written* packet set, on one rational timeline.
        # Legitimate VFR intervals are not issues; real gaps, regressions and
        # overlaps are located and reported.  They do not abort the copy.
        built = {}
        for info in probe.streams:
            spans = [
                packet.to_span()
                for packet in outcome.packets
                if packet.stream == info.index
            ]
            built[info.index] = build_stream_timeline(
                info.index, info.type, info.time_base_raw, spans, asset=str(path)
            )
        timeline_report = analyse_timelines(built, asset=str(path))
        result.extra["timeline"] = timeline_report.to_dict()
        for issue in timeline_report.issues:
            result.warnings.append(
                RecoverableError(
                    issue.message,
                    location=issue.location,
                    code=issue.kind,
                )
            )

        if _timed_out(deadline):
            raise AssetTimeoutError("asset exceeded the processing timeout")
        if not outcome.packets:
            raise UnrecoverableMediaError(
                "no usable packets survived recovery",
                location=MediaLocation(asset=str(path)),
            )
        for slot in graph.missing_required:
            result.warnings.append(
                RecoverableError(
                    f"required output {slot} has no source stream; output is partial",
                    location=MediaLocation(asset=str(path), rule=slot),
                    code="partial_output",
                )
            )

        if options.sync_markers:
            # Measure the source even if we will refuse to copy it: a delivery
            # whose A/V is already out of sync is worth reporting either way.
            source_sync = measure_sync(path, options.sync_markers, fps=options.fps)
            result.extra["source_sync"] = source_sync.to_dict()
            result.sync_error_ms = source_sync.sync_error_ms
            if not source_sync.measured:
                result.warnings.append(
                    RecoverableError(
                        "source sync could not be measured: "
                        f"{source_sync.error or 'no markers found'}",
                        location=MediaLocation(asset=str(path)),
                        code="source_sync_unmeasured",
                    )
                )

        try:
            check_lossless_support([node.source for node in graph.nodes])
        except LosslessCopyRefusedError as exc:
            result.status = STATUS_REFUSED
            result.errors.append(exc)
            result.reasons.append(exc.message)
            result.latency_ms = (time.monotonic() - started) * 1000.0
            return PipelineResult(result)

        output_name = f"{path.stem}.remux{path.suffix}"
        sink = atomic_output(options.output_dir, output_name)
        try:
            remux = remux_asset(probe, graph, outcome.packets, sink.temp_path)
            matched, total, mismatches = _compare_packets(
                outcome.packets, sink.temp_path, remux.stream_map
            )
            match_rate = (matched / total) if total else 0.0
            result.copied_packets = remux.packets_written
            result.remux_packet_match_rate = match_rate
            result.packet_copy_exact = matched == total
            result.extra["remux"] = remux.to_dict()
            result.extra["packet_digest"] = packet_digest(outcome.packets)
            if mismatches:
                result.extra["packet_mismatches"] = mismatches
            if options.decode_check:
                result.extra["decode_health"] = decode_health(sink.temp_path)
            if options.sync_markers:
                measurement = measure_sync(
                    sink.temp_path, options.sync_markers, fps=options.fps
                )
                result.sync_error_ms = measurement.sync_error_ms
                result.extra["sync"] = measurement.to_dict()
                if not measurement.measured:
                    result.warnings.append(
                        RecoverableError(
                            "output sync could not be measured: "
                            f"{measurement.error or 'no markers'}",
                            location=MediaLocation(asset=str(path)),
                            code="sync_unmeasured",
                        )
                    )
            if _timed_out(deadline):
                raise AssetTimeoutError("asset exceeded the processing timeout")
            if match_rate < 1.0:
                result.warnings.append(
                    RecoverableError(
                        f"only {matched}/{total} packets survived the copy byte-identically",
                        location=MediaLocation(asset=str(path)),
                        code="packet_copy_mismatch",
                    )
                )
            sink.commit()
        except BaseException:
            sink.discard()
            raise
        result.output = sink.final_path
        result.output_bytes = sink.final_path.stat().st_size
        result.extra["output_sha256"] = _sha256_file(sink.final_path)

        if graph.partial:
            result.status = STATUS_PARTIAL
        elif (
            outcome.over_recovered
            or outcome.recovery_rate < 1.0
            or outcome.plan.repair_index
            or outcome.plan.truncated
        ):
            # Anything other than an exact, fully-explained recovery is a
            # recovered delivery, including an over-count we cannot explain.
            result.status = STATUS_RECOVERED
        else:
            result.status = STATUS_OK
        if outcome.over_recovered:
            result.warnings.append(
                RecoverableError(
                    f"recovered {outcome.recovered_packets} packets but the declared media "
                    f"implies {outcome.expected_packets}",
                    location=MediaLocation(asset=str(path)),
                    code="recovery_expectation_mismatch",
                )
            )
        if outcome.plan.truncated and result.status == STATUS_RECOVERED:
            result.reasons.append("tail truncated; boundary scan kept the surviving packets")
        if outcome.plan.corrupt_packets:
            result.reasons.append(
                f"{outcome.plan.corrupt_packets} damaged packet(s) skipped, never re-encoded"
            )
        if outcome.plan.incomplete_index:
            result.reasons.append("incomplete index detected; index rebuilt from kept packets")
        if graph.partial:
            result.reasons.append("required track missing; partial output written")

        if options.reuse:
            try:
                store_cache(
                    options.cache_dir,
                    key,
                    {
                        "result": _result_to_cache(result),
                        "strategy": fingerprint,
                        "input_sha256": input_digest,
                        "input_size": path.stat().st_size,
                    },
                    sink.final_path,
                )
            except OSError as exc:
                result.warnings.append(
                    RecoverableError(
                        f"could not persist resume cache: {exc}",
                        location=MediaLocation(asset=str(path)),
                        code="cache_write_failed",
                    )
                )
    except QCError as exc:
        result.errors.append(exc)
        result.strategies = result.strategies or ["none"]
        result.status = STATUS_FAILED
        if isinstance(exc, LosslessCopyRefusedError):
            result.status = STATUS_REFUSED
            result.reasons.append(exc.message)
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        result.errors.append(
            UnrecoverableMediaError(
                f"media processing failed: {exc}",
                location=MediaLocation(asset=str(path)),
            )
        )
        result.status = STATUS_FAILED
    finally:
        result.latency_ms = (time.monotonic() - started) * 1000.0
    return PipelineResult(result, exit_fatal=result.status == STATUS_FAILED)


def _timed_out(deadline: float) -> bool:
    return time.monotonic() > deadline


def _result_to_cache(result: AssetResult) -> dict[str, Any]:
    payload = result.to_dict()
    payload.pop("output", None)
    payload.pop("latency_ms", None)
    payload.pop("asset", None)
    payload.pop("cache", None)
    return payload


def _result_from_cache(payload: dict[str, Any], path: Path, media: Path) -> AssetResult:
    stored = payload.get("result", {})
    result = AssetResult(
        asset=path,
        strategy=stored.get("strategy", "lossless_copy"),
        status=stored.get("status", STATUS_OK),
        duration_s=stored.get("duration_s"),
        tracks=int(stored.get("tracks") or 0),
        output=media,
        output_bytes=int(stored.get("output_bytes") or media.stat().st_size),
        copied_packets=int(stored.get("copied_packets") or 0),
        expected_packets=int(stored.get("expected_packets") or 0),
        recovery_rate=stored.get("recovery_rate"),
        remux_packet_match_rate=stored.get("remux_packet_match_rate"),
        packet_copy_exact=stored.get("packet_copy_exact"),
        sync_error_ms=stored.get("sync_error_ms"),
        codec_pairs=list(stored.get("codec_pairs") or []),
        strategies=list(stored.get("strategies") or []),
        reasons=list(stored.get("reasons") or []),
        mapping=list(stored.get("mapping") or []),
    )
    for key, value in (stored.get("extra") or {}).items():
        result.extra[key] = value
    return result


def iter_assets(directory: Path) -> list[Path]:
    return sorted(
        path
        for path in directory.iterdir()
        if path.is_file() and not path.name.startswith(".")
    )
