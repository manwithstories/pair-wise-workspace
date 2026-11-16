"""JSON persistence for traces, per-segment decisions and run summaries.

All files are plain local JSON (no network, no external services). The
trace reader is the single ingestion chokepoint, so every illegal input
listed in the requirements — empty trace, duplicate timestamps, negative
loss, non-numeric fields, missing keys — is rejected here with a clear
:class:`ABRTraceError` / :class:`ABRConfigError` message.
"""

from __future__ import annotations

import json
from pathlib import Path

from .metrics import SimulationSummary
from .models import (
    ABRConfigError,
    ABRTraceError,
    BitrateLadder,
    Decision,
    EstimatorState,
    NetworkSample,
    Trace,
)

TRACE_SCHEMA = "abr-trace/1"
RESULT_SCHEMA = "abr-result/1"


# ---------------------------------------------------------------------- #
# Bitrate ladder parsing
# ---------------------------------------------------------------------- #
def parse_bitrates(text: str) -> tuple[int, ...]:
    """Parse a comma-separated ``"800,1600,3000"`` ladder string."""
    if not text or not text.strip():
        raise ABRConfigError("bitrates list is empty")
    values: list[int] = []
    for token in text.split(","):
        token = token.strip()
        if not token:
            raise ABRConfigError(f"empty bitrate entry in {text!r}")
        try:
            value = int(token)
        except (TypeError, ValueError) as exc:
            raise ABRConfigError(
                f"bitrate {token!r} is not an integer"
            ) from exc
        values.append(value)
    # Validate eagerly so an unsorted/non-positive ladder is rejected at the
    # CLI entry point rather than failing later inside the simulation.
    return BitrateLadder(tuple(values)).bitrates


# ---------------------------------------------------------------------- #
# Trace IO
# ---------------------------------------------------------------------- #
def trace_to_dict(trace: Trace) -> dict:
    return {
        "schema": TRACE_SCHEMA,
        "segment_seconds": trace.segment_seconds,
        "seed": trace.seed,
        "name": trace.name,
        "segments": len(trace),
        "samples": [
            {
                "t": s.t,
                "throughput": s.throughput,
                "rtt": s.rtt,
                "loss": s.loss,
            }
            for s in trace.samples
        ],
    }


def save_trace(trace: Trace, path: str | Path) -> Path:
    out = Path(path)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(
        json.dumps(trace_to_dict(trace), indent=2, ensure_ascii=False),
        encoding="utf-8",
    )
    return out


def load_trace(path: str | Path) -> Trace:
    """Read and validate a trace JSON file from disk."""
    source = Path(path)
    if not source.exists():
        raise ABRTraceError(f"trace file not found: {source}")
    try:
        raw = json.loads(source.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise ABRTraceError(f"trace file is not valid JSON: {exc}") from exc
    if not isinstance(raw, dict):
        raise ABRTraceError("trace root must be a JSON object")

    segment_seconds = raw.get("segment_seconds")
    if not isinstance(segment_seconds, (int, float)) or isinstance(
        segment_seconds, bool
    ):
        raise ABRTraceError("trace requires a numeric 'segment_seconds'")

    raw_samples = raw.get("samples")
    if not isinstance(raw_samples, list) or not raw_samples:
        raise ABRTraceError("trace requires a non-empty 'samples' array")

    samples: list[NetworkSample] = []
    seen_timestamps: set[float] = set()
    for position, item in enumerate(raw_samples):
        if not isinstance(item, dict):
            raise ABRTraceError(f"sample #{position} must be an object")
        try:
            sample = NetworkSample(
                t=item["t"],
                throughput=item["throughput"],
                rtt=item["rtt"],
                loss=item["loss"],
            )
        except KeyError as exc:
            raise ABRTraceError(
                f"sample #{position} is missing field {exc.args[0]!r}"
            ) from exc
        except (TypeError, ValueError) as exc:
            raise ABRTraceError(f"sample #{position} is invalid: {exc}") from exc
        if sample.t in seen_timestamps:
            raise ABRTraceError(
                f"duplicate timestamp {sample.t} at sample #{position}; "
                "timestamps must be unique"
            )
        seen_timestamps.add(sample.t)
        samples.append(sample)

    return Trace(
        samples=tuple(samples),
        segment_seconds=float(segment_seconds),
        seed=raw.get("seed"),
        name=str(raw.get("name", source.stem)),
    )


# ---------------------------------------------------------------------- #
# Result IO
# ---------------------------------------------------------------------- #
def _decision_to_dict(decision: Decision) -> dict:
    return {
        "index": decision.index,
        "bitrate_kbps": decision.bitrate_kbps,
        "est_bandwidth_kbps": decision.est_bandwidth_kbps,
        "estimator_state": decision.estimator_state.value,
        "buffer_seconds": decision.buffer_seconds,
        "download_seconds": decision.download_seconds,
        "switch": decision.switch,
        "stall": decision.stall,
        "stall_seconds": decision.stall_seconds,
        "throughput_kbps": decision.throughput_kbps,
    }


def result_to_dict(
    trace: Trace,
    decisions: list[Decision],
    summary: SimulationSummary,
    config: dict,
) -> dict:
    return {
        "schema": RESULT_SCHEMA,
        "trace": {
            "name": trace.name,
            "seed": trace.seed,
            "segments": len(trace),
            "segment_seconds": trace.segment_seconds,
        },
        "config": config,
        "summary": summary.to_dict(),
        "decisions": [_decision_to_dict(d) for d in decisions],
    }


def save_result(
    trace: Trace,
    decisions: list[Decision],
    summary: SimulationSummary,
    config: dict,
    path: str | Path,
) -> Path:
    out = Path(path)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(
        json.dumps(
            result_to_dict(trace, decisions, summary, config),
            indent=2,
            ensure_ascii=False,
        ),
        encoding="utf-8",
    )
    return out


def estimator_state_from_value(value: str) -> EstimatorState:
    try:
        return EstimatorState(value)
    except ValueError as exc:
        raise ABRConfigError(f"unknown estimator state {value!r}") from exc
