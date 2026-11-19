"""Demo/verify orchestration.

``run_demo`` generates the fixture corpus, drives every asset through the
pipeline, and grades the results into the acceptance metrics the terminal
report prints:

* ``roundtrip_parse_rate`` - fraction of healthy outputs that reopen and demux;
* ``segment_hash_match`` - fraction of segments whose per-stream payload hash is
  identical between input and output;
* ``keyframe_hit_rate`` - measured seek hit rate over the generated indexes;
* ``peak_queue_items`` - highest queue depth observed, must stay at or below the
  configured size;
* ``peak_rss_mb`` - peak resident memory of the process;
* ``damaged_segments`` - number of damaged fixtures isolated without aborting.

``run_verify`` re-checks an existing output directory.  Both return a structured
report; printing and exit codes are the CLI's job.
"""

from __future__ import annotations

import resource
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path

from .audio import AudioReport, inspect_audio
from .demux import DemuxedPacket, Demuxer
from .fixtures import Corpus, Fixture, build_demo_corpus
from .hashing import HashComparison
from .keyframes import IndexReport, build_index, validate_index
from .mapping import resolve_mapping
from .pipeline import Pipeline, PipelineConfig, compare_roundtrip, timeline_of
from .pcm import PcmReport, compare_pcm
from .probe import ProbeResult, probe
from .timebase import AircheckError, ExitCode, InputError

#: Acceptance thresholds, mirrored in the README and the CLI help.
THRESHOLDS = {
    "roundtrip_parse_rate": 1.0,
    "keyframe_hit_rate": 0.99,
    "segment_hash_match": 1.0,
}

DAMAGE_CATEGORIES = {
    "truncated": "truncated",
    "corrupt": "corrupt",
    "timestamp_wrap": "timestamp",
    "timestamp_disorder": "timestamp",
    "bad_index": "index",
}


@dataclass
class AssetReport:
    """One asset's full journey, including its acceptance-relevant facts."""

    name: str
    path: Path
    kind: str                       # "healthy" | "damaged"
    damage: str | None
    ok: bool
    output: Path | None = None
    error: dict[str, object] | None = None
    probe_issues: list[str] = field(default_factory=list)
    hash_comparison: HashComparison | None = None
    timeline_issues: list[dict[str, object]] = field(default_factory=list)
    keyframe: IndexReport | None = None
    audio: AudioReport | None = None
    pcm: PcmReport | None = None
    stage: dict[str, object] | None = None
    expected_segments: int = 0
    isolated: bool = False
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, object]:
        return {
            "name": self.name,
            "path": str(self.path),
            "kind": self.kind,
            "damage": self.damage,
            "ok": self.ok,
            "isolated": self.isolated,
            "output": str(self.output) if self.output else None,
            "error": self.error,
            "probe_issues": list(self.probe_issues),
            "hash_comparison": self.hash_comparison.to_dict() if self.hash_comparison else None,
            "timeline_issues": list(self.timeline_issues),
            "keyframe": self.keyframe.to_dict() if self.keyframe else None,
            "audio": self.audio.to_dict() if self.audio else None,
            "pcm": self.pcm.to_dict() if self.pcm else None,
            "stage": self.stage,
            "expected_segments": self.expected_segments,
            "notes": list(self.notes),
        }


@dataclass
class DemoReport:
    output_dir: Path
    formats: list[str]
    queue_size: int
    slow_consumer_ms: int
    assets: list[AssetReport] = field(default_factory=list)
    metrics: dict[str, float] = field(default_factory=dict)
    peak_rss_mb: float = 0.0
    peak_queue_items: int = 0
    damaged_segments: int = 0
    duration_seconds: float = 0.0
    acceptable: bool = False
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, object]:
        return {
            "mode": "demo",
            "output_dir": str(self.output_dir),
            "formats": list(self.formats),
            "queue_size": self.queue_size,
            "slow_consumer_ms": self.slow_consumer_ms,
            "assets": [asset.to_dict() for asset in self.assets],
            "metrics": {name: round(value, 6) for name, value in self.metrics.items()},
            "peak_rss_mb": round(self.peak_rss_mb, 2),
            "peak_queue_items": self.peak_queue_items,
            "damaged_segments": self.damaged_segments,
            "duration_seconds": round(self.duration_seconds, 3),
            "thresholds": dict(THRESHOLDS),
            "acceptable": self.acceptable,
            "exit_code": int(ExitCode.OK if self.acceptable else ExitCode.ACCEPTANCE_DIFF),
            "notes": list(self.notes),
        }

    def summary_lines(self) -> list[str]:
        lines = [
            f"roundtrip_parse_rate={self.metrics.get('roundtrip_parse_rate', 0.0):.3f}",
            f"keyframe_hit_rate={self.metrics.get('keyframe_hit_rate', 0.0):.3f}",
            f"segment_hash_match={self.metrics.get('segment_hash_match', 0.0):.3f}",
            f"peak_queue_items={self.peak_queue_items}",
            f"peak_rss_mb={self.peak_rss_mb:.1f}",
            f"damaged_segments={self.damaged_segments}",
            f"acceptable={'yes' if self.acceptable else 'no'}",
        ]
        return lines


def _peak_rss_mb() -> float:
    """Peak resident set size of this process in MiB (macOS/Linux)."""
    # macOS reports bytes; Linux reports kilobytes.  rusage.ru_maxrss differs,
    # so normalise using sys.platform rather than guessing from magnitude.
    value = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    if sys.platform == "darwin":
        return value / (1024 * 1024)
    return value / 1024


def _segment_ticks_of(fixture: Fixture, default_ms: int = 500) -> int:
    if fixture.segment_ticks > 0:
        return fixture.segment_ticks
    return default_ms * 1_000_000


def _collect_demuxed(probe_result: ProbeResult, segment_ticks: int,
                     resume: int = 0) -> list[DemuxedPacket]:
    return list(
        Demuxer(probe=probe_result, segment_ticks=segment_ticks,
                resume_from_segment=resume).iter_packets()
    )


def _process_healthy(
    fixture: Fixture,
    output_dir: Path,
    *,
    queue_size: int,
    slow_consumer_ms: int,
    target_format: str = "mkv",
) -> AssetReport:
    report = AssetReport(
        name=fixture.name,
        path=fixture.path,
        kind="healthy",
        damage=None,
        ok=False,
    )
    probe_result = probe(fixture.path, decode_check=True)
    report.probe_issues = sorted({issue.kind for issue in probe_result.issues})
    if probe_result.unreadable:
        report.error = {
            "category": "input",
            "message": "healthy fixture could not be opened",
            "context": {"open_error": probe_result.open_error},
        }
        return report
    try:
        mapping = resolve_mapping(probe_result)
    except AircheckError as exc:
        report.error = exc.to_dict()
        return report

    segment_ticks = _segment_ticks_of(fixture)
    duration_ticks = probe_result.duration_ticks or fixture.duration_ticks
    try:
        source_packets = _collect_demuxed(probe_result, segment_ticks)
    except AircheckError as exc:
        report.error = exc.to_dict()
        return report
    report.expected_segments = len({packet.segment for packet in source_packets})

    timeline = timeline_of(
        source_packets,
        segment_ticks=segment_ticks,
        duration_ticks=duration_ticks,
        transition_ms=0,
    )
    report.timeline_issues = [issue.to_dict() for issue in timeline.issues]

    config = PipelineConfig(
        segment_ticks=segment_ticks,
        target_format=target_format,
        queue_size=queue_size,
        slow_consumer_ms=slow_consumer_ms,
    )
    output = output_dir / f"{fixture.name}.{target_format}"
    pipeline = Pipeline(probe_result, mapping, output, config)
    stage = pipeline.run()
    report.stage = stage.to_dict()
    report.output = stage.output
    if not stage.ok:
        report.error = stage.errors[0] if stage.errors else {"category": "internal"}
        return report

    output_probe = probe(stage.output)
    if output_probe.unreadable:
        report.error = {
            "category": "roundtrip",
            "message": "output could not be reopened",
            "context": {"open_error": output_probe.open_error},
        }
        return report
    output_packets = _collect_demuxed(output_probe, segment_ticks)
    comparison, drift, _plan = compare_roundtrip(source_packets, output_packets)
    report.hash_comparison = comparison

    index = build_index(stage.output, output_probe)
    report.keyframe = validate_index(
        stage.output, index, probe_result=output_probe
    )
    report.audio = inspect_audio(stage.output, output_probe, sample_stride=8)
    # For PCM audio, decode both sides and compare samples independently of the
    # compressed payload hash (non-PCM streams are reported as skipped).
    report.pcm = compare_pcm(fixture.path, stage.output, probe_result, output_probe)
    report.ok = (
        comparison.all_match
        and all(entry["within_tolerance"] for entry in drift)
        and not report.timeline_issues
        and report.pcm.ok
    )
    if not comparison.all_match:
        report.notes.append(
            f"{len(comparison.mismatched)} segment(s) hash-mismatched"
        )
    return report


def _process_damaged(fixture: Fixture, output_dir: Path, *,
                     queue_size: int) -> AssetReport:
    """Isolate one damaged asset: classify the fault, keep going."""
    report = AssetReport(
        name=fixture.name,
        path=fixture.path,
        kind="damaged",
        damage=fixture.damage,
        ok=False,
        isolated=True,
    )
    try:
        probe_result = probe(fixture.path, decode_check=True)
    except AircheckError as exc:
        report.error = exc.to_dict()
        return report
    kinds = sorted({issue.kind for issue in probe_result.issues})
    report.probe_issues = kinds
    report.error = {
        "category": DAMAGE_CATEGORIES.get(fixture.damage or "", "corrupt"),
        "message": f"damaged asset isolated ({fixture.damage})",
        "context": {
            "issues": kinds,
            "opens": not probe_result.unreadable,
            "streams": len(probe_result.streams),
        },
    }
    # Try the keyframe/container index path too: it surfaces cue-table damage
    # that packet scanning cannot see.
    try:
        index = build_index(fixture.path, probe_result)
        if index or probe_result.index_info.get("cue_entries"):
            keyframe = validate_index(
                fixture.path, index, probe_result=probe_result, probe_seeks=bool(index)
            )
            report.keyframe = keyframe
            if keyframe.problems:
                report.probe_issues = sorted(
                    set(report.probe_issues) | {problem.kind for problem in keyframe.problems}
                )
    except AircheckError as exc:
        report.notes.append(f"index check skipped: {exc.category.value}")
    # Isolation means the run continues; a damaged asset is not itself "ok".
    report.ok = probe_result.unreadable is False
    return report


def run_demo(
    output_dir: str | Path,
    *,
    formats: list[str],
    queue_size: int = 4,
    slow_consumer_ms: int = 0,
    target_format: str = "mkv",
) -> DemoReport:
    """Generate fixtures, process them, and compute acceptance metrics."""
    started = time.monotonic()
    out_dir = Path(output_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    fixtures_dir = out_dir / "fixtures"
    media_dir = out_dir / "media"
    media_dir.mkdir(parents=True, exist_ok=True)

    corpus: Corpus = build_demo_corpus(fixtures_dir, formats)
    report = DemoReport(
        output_dir=out_dir,
        formats=list(formats),
        queue_size=queue_size,
        slow_consumer_ms=slow_consumer_ms,
    )

    for fixture in corpus.healthy:
        asset = _process_healthy(
            fixture,
            media_dir,
            queue_size=queue_size,
            slow_consumer_ms=slow_consumer_ms,
            target_format=target_format,
        )
        report.assets.append(asset)
    for fixture in corpus.damaged:
        report.assets.append(
            _process_damaged(fixture, media_dir, queue_size=queue_size)
        )

    healthy = [asset for asset in report.assets if asset.kind == "healthy"]
    damaged = [asset for asset in report.assets if asset.kind == "damaged"]

    parsed = [asset for asset in healthy if asset.ok or (
        asset.hash_comparison is not None and asset.keyframe is not None
    )]
    report.metrics["roundtrip_parse_rate"] = (
        len(parsed) / len(healthy) if healthy else 0.0
    )

    total_segments = 0
    matched_segments = 0
    for asset in healthy:
        if asset.hash_comparison is None:
            continue
        total_segments += asset.hash_comparison.total
        matched_segments += len(asset.hash_comparison.matched)
    report.metrics["segment_hash_match"] = (
        matched_segments / total_segments if total_segments else 0.0
    )

    attempts = sum(asset.keyframe.seeks_attempted for asset in healthy if asset.keyframe)
    hits = sum(asset.keyframe.seeks_hit for asset in healthy if asset.keyframe)
    report.metrics["keyframe_hit_rate"] = hits / attempts if attempts else 0.0

    peaks = [
        value
        for asset in healthy
        if asset.stage
        for value in [
            queue.get("peak_items", 0)
            for queue in asset.stage.get("queues", {}).values()
        ]
    ]
    report.peak_queue_items = max(peaks) if peaks else 0
    report.damaged_segments = sum(
        1 for asset in damaged
        if asset.error is not None and asset.isolated
    )
    report.peak_rss_mb = _peak_rss_mb()
    report.duration_seconds = time.monotonic() - started

    report.acceptable = (
        report.metrics.get("roundtrip_parse_rate", 0.0) >= THRESHOLDS["roundtrip_parse_rate"]
        and report.metrics.get("segment_hash_match", 0.0) >= THRESHOLDS["segment_hash_match"]
        and report.metrics.get("keyframe_hit_rate", 0.0) >= THRESHOLDS["keyframe_hit_rate"]
        and 0 < report.peak_queue_items <= queue_size
        and report.damaged_segments >= 1
    )
    if report.peak_queue_items > queue_size:
        report.notes.append(
            f"queue ceiling exceeded: {report.peak_queue_items} > {queue_size}"
        )
    return report


@dataclass
class VerifyReport:
    input_dir: Path
    parse_rate: float
    segment_hash_match: float
    keyframe_hit_rate: float
    per_file: list[dict[str, object]] = field(default_factory=list)
    damaged_found: int = 0
    notes: list[str] = field(default_factory=list)
    acceptable: bool = False

    def to_dict(self) -> dict[str, object]:
        return {
            "mode": "verify",
            "input_dir": str(self.input_dir),
            "roundtrip_parse_rate": round(self.parse_rate, 6),
            "segment_hash_match": round(self.segment_hash_match, 6),
            "keyframe_hit_rate": round(self.keyframe_hit_rate, 6),
            "per_file": list(self.per_file),
            "damaged_segments": self.damaged_found,
            "thresholds": dict(THRESHOLDS),
            "acceptable": self.acceptable,
            "exit_code": int(ExitCode.OK if self.acceptable else ExitCode.ACCEPTANCE_DIFF),
            "notes": list(self.notes),
        }

    def summary_lines(self) -> list[str]:
        return [
            f"roundtrip_parse_rate={self.parse_rate:.3f}",
            f"keyframe_hit_rate={self.keyframe_hit_rate:.3f}",
            f"segment_hash_match={self.segment_hash_match:.3f}",
            f"damaged_segments={self.damaged_found}",
            f"acceptable={'yes' if self.acceptable else 'no'}",
        ]


def _output_pairs(media_dir: Path) -> list[tuple[Path, Path]]:
    """Pair each ``normal_*`` output with its ``fixtures/`` source."""
    pairs: list[tuple[Path, Path]] = []
    fixtures_dir = media_dir.parent / "fixtures"
    for candidate in sorted(media_dir.glob("normal_*.*")):
        base = candidate.stem
        # ``normal_mov`` -> ``fixtures/normal_mov.mov`` (original suffix kept)
        source_stem = base
        matches = list(fixtures_dir.glob(f"{source_stem}.*"))
        if matches:
            pairs.append((matches[0], candidate))
    return pairs


def run_verify(input_dir: str | Path) -> VerifyReport:
    """Re-check a demo output directory without regenerating fixtures."""
    root = Path(input_dir)
    if not root.exists():
        raise InputError("verify input directory does not exist", path=str(root))
    media_dir = root / "media"
    if not media_dir.exists():
        # Accept being pointed straight at a directory of outputs.
        media_dir = root
    report = VerifyReport(
        input_dir=root,
        parse_rate=0.0,
        segment_hash_match=0.0,
        keyframe_hit_rate=0.0,
    )
    pairs = _output_pairs(media_dir)
    if not pairs:
        report.notes.append(
            "no paired normal_* outputs with matching fixtures were found; "
            "verify expects the layout written by `aircheck demo`"
        )
        return report

    parsed = 0
    total_segments = 0
    matched_segments = 0
    attempts = 0
    hits = 0
    for source, output in pairs:
        entry: dict[str, object] = {"source": str(source), "output": str(output)}
        try:
            source_probe = probe(source)
            output_probe = probe(output)
        except AircheckError as exc:
            entry["error"] = exc.to_dict()
            report.per_file.append(entry)
            continue
        if output_probe.unreadable:
            entry["error"] = {"category": "roundtrip", "message": "output unreadable"}
            report.per_file.append(entry)
            continue
        parsed += 1
        segment_ticks = 500_000_000
        source_packets = _collect_demuxed(source_probe, segment_ticks)
        output_packets = _collect_demuxed(output_probe, segment_ticks)
        comparison, drift, _plan = compare_roundtrip(source_packets, output_packets)
        total_segments += comparison.total
        matched_segments += len(comparison.matched)
        index = build_index(output, output_probe)
        keyframe = validate_index(output, index, probe_result=output_probe)
        attempts += keyframe.seeks_attempted
        hits += keyframe.seeks_hit
        pcm = compare_pcm(source, output, source_probe, output_probe)
        entry["pcm"] = pcm.to_dict()
        entry.update(
            {
                "segments": comparison.total,
                "segments_matched": len(comparison.matched),
                "hash_match": comparison.all_match,
                "timeline_ok": all(item["within_tolerance"] for item in drift),
                "keyframe_hit_rate": keyframe.hit_rate,
                "keyframe_problems": [problem.kind for problem in keyframe.problems],
            }
        )
        report.per_file.append(entry)

    damaged_dir = root / "fixtures" / "damaged"
    if damaged_dir.exists():
        for candidate in sorted(damaged_dir.iterdir()):
            if not candidate.is_file():
                continue
            try:
                result = probe(candidate, decode_check=True)
            except AircheckError:
                report.damaged_found += 1
                continue
            if result.issues:
                report.damaged_found += 1

    report.parse_rate = parsed / len(pairs) if pairs else 0.0
    report.segment_hash_match = (
        matched_segments / total_segments if total_segments else 0.0
    )
    report.keyframe_hit_rate = hits / attempts if attempts else 0.0
    report.acceptable = (
        report.parse_rate >= THRESHOLDS["roundtrip_parse_rate"]
        and report.segment_hash_match >= THRESHOLDS["segment_hash_match"]
        and report.keyframe_hit_rate >= THRESHOLDS["keyframe_hit_rate"]
    )
    return report


__all__ = [
    "AssetReport",
    "DAMAGE_CATEGORIES",
    "DemoReport",
    "THRESHOLDS",
    "VerifyReport",
    "run_demo",
    "run_verify",
]
