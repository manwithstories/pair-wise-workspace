"""Explicit input-to-output track mapping.

A mapping is never inferred silently.  The batch policy always contains a
rule per output track ("report": ``{"video": [{"source": 0}], ...}``); with
``--track-map auto`` (the default for the demo) a *deterministic* rule set is
derived from the probe and recorded in the report, so the same input always
yields the same mapping and the same output.

Rule syntax
-----------
Each rule is a small dict::

    {"output": 0, "source": 0, "kind": "video", "required": true, "label": "Picture"}

``output`` defaults to the rule's position in the list.  ``source`` is the
input stream index.  ``kind`` must match the probed stream type.  ``required``
controls whether a missing source is fatal (recoverable assets are still
written, but a required missing track makes the asset partial).

Rejections (all carry a located :class:`~bcastqc.errors.MediaLocation`):

* duplicate output slots          -> :class:`DuplicateMappingError`
* duplicate use of one input      -> :class:`DuplicateMappingError`
* missing required track          -> :class:`MissingTrackError`
* mismatched type / parameters    -> :class:`IncompatibleTrackError`
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable

from .demux import AssetProbe, StreamInfo
from .errors import FatalError, MediaLocation, QCError


class MappingError(FatalError):
    code = "track_mapping_error"


class DuplicateMappingError(MappingError):
    code = "duplicate_mapping"


class MissingTrackError(MappingError):
    code = "missing_track"


class IncompatibleTrackError(MappingError):
    code = "incompatible_track"


@dataclass(frozen=True, slots=True)
class TrackRule:
    output: int
    source: int
    kind: str
    required: bool = True
    label: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "output": self.output,
            "source": self.source,
            "kind": self.kind,
            "required": self.required,
            "label": self.label,
        }


@dataclass(slots=True)
class TrackNode:
    """One output track and the input stream feeding it."""

    output: int
    rule: TrackRule
    source: StreamInfo
    # Recovery outcome attached by :mod:`bcastqc.recover`.
    recovered_packets: int = 0
    skipped_packets: int = 0
    written_packets: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "output": self.output,
            "source": self.source.index,
            "type": self.source.type,
            "codec": self.source.codec,
            "required": self.rule.required,
            "label": self.rule.label,
            "recovered_packets": self.recovered_packets,
            "skipped_packets": self.skipped_packets,
            "written_packets": self.written_packets,
        }


@dataclass(slots=True)
class TrackGraph:
    nodes: list[TrackNode]
    missing_required: list[int] = field(default_factory=list)
    ignored_inputs: list[int] = field(default_factory=list)

    def ordered(self) -> list[TrackNode]:
        """Output order used when writing: video first, then audio, then rest.

        Video must be written before audio for players to discover a usable
        picture quickly, and MP4 requires the first *track* to be enabled for
        broadcast QC tools.  Ties keep the operator's rule order.
        """
        priority = {"video": 0, "audio": 1, "subtitle": 2}
        return sorted(
            self.nodes,
            key=lambda node: (priority.get(node.source.type, 3), node.output),
        )

    @property
    def partial(self) -> bool:
        return bool(self.missing_required)

    def source_indices(self) -> list[int]:
        return [node.source.index for node in self.nodes]

    def to_dict(self) -> dict[str, Any]:
        return {
            "nodes": [node.to_dict() for node in self.ordered()],
            "missing_required": list(self.missing_required),
            "ignored_inputs": list(self.ignored_inputs),
            "partial": self.partial,
        }


def stable_automatic_rules(probe: AssetProbe) -> list[TrackRule]:
    """Deterministic mapping: every copyable stream, in index order.

    This is what the demo uses so the report's mapping is reproducible across
    runs; it is a *recorded policy*, not hidden inference.
    """
    rules: list[TrackRule] = []
    order = {"video": 0, "audio": 1}
    ordered = sorted(
        (s for s in probe.streams if s.copyable),
        key=lambda s: (order.get(s.type, 9), s.index),
    )
    for slot, stream in enumerate(ordered):
        rules.append(
            TrackRule(
                output=slot,
                source=stream.index,
                kind=stream.type,
                required=True,
                label=f"{stream.type}:{stream.codec}",
            )
        )
    return rules


def parse_rules(raw: Iterable[dict[str, Any]]) -> list[TrackRule]:
    rules: list[TrackRule] = []
    for position, item in enumerate(raw):
        if not isinstance(item, dict):
            raise MissingTrackError(
                f"rule {position} must be an object",
                location=MediaLocation(rule=position),
            )
        output = int(item.get("output", position))
        if "source" not in item:
            raise MissingTrackError(
                f"rule {position} has no source",
                location=MediaLocation(rule=position),
            )
        source = int(item["source"])
        kind = str(item.get("kind", ""))
        if kind not in {"video", "audio", "subtitle"}:
            raise IncompatibleTrackError(
                f"rule {position} has unsupported kind {kind!r}",
                location=MediaLocation(rule=position),
            )
        rules.append(
            TrackRule(
                output=output,
                source=source,
                kind=kind,
                required=bool(item.get("required", True)),
                label=str(item.get("label", "")),
            )
        )
    return rules


def build_track_graph(probe: AssetProbe, rules: list[TrackRule]) -> TrackGraph:
    """Validate rules against the probe and build the output track graph."""
    outputs: dict[int, TrackRule] = {}
    sources: dict[int, TrackRule] = {}
    nodes: list[TrackNode] = []
    missing_required: list[int] = []

    for rule in rules:
        if rule.output in outputs:
            raise DuplicateMappingError(
                f"output slot {rule.output} is mapped twice "
                f"(rules {outputs[rule.output].source} and {rule.source})",
                location=MediaLocation(
                    stream=rule.source,
                    rule=rule.output,
                    detail="duplicate output slot",
                ),
            )
        outputs[rule.output] = rule
        if rule.source in sources:
            raise DuplicateMappingError(
                f"input stream {rule.source} is used by output slots "
                f"{sources[rule.source].output} and {rule.output}",
                location=MediaLocation(
                    stream=rule.source,
                    rule=rule.output,
                    detail="duplicate source stream",
                ),
            )
        sources[rule.source] = rule
        stream = probe.by_index(rule.source)
        if stream is None:
            if rule.required:
                missing_required.append(rule.output)
                raise MissingTrackError(
                    f"required output {rule.output} needs input stream {rule.source}, "
                    "which the asset does not contain",
                    location=MediaLocation(
                        asset=str(probe.path), stream=rule.source, rule=rule.output
                    ),
                )
            continue
        if stream.type != rule.kind:
            raise IncompatibleTrackError(
                f"output {rule.output} expects {rule.kind} but input stream "
                f"{rule.source} is {stream.type}",
                location=MediaLocation(
                    asset=str(probe.path),
                    stream=rule.source,
                    rule=rule.output,
                    detail="stream type mismatch",
                ),
            )
        if not stream.copyable:
            raise IncompatibleTrackError(
                f"input stream {rule.source} codec {stream.codec!r} cannot be copied losslessly",
                location=MediaLocation(
                    asset=str(probe.path),
                    stream=rule.source,
                    rule=rule.output,
                    detail=f"unsupported codec {stream.codec!r}",
                ),
            )
        nodes.append(TrackNode(output=rule.output, rule=rule, source=stream))

    # A mapping must cover at least one real stream.
    if not nodes:
        raise MissingTrackError(
            "mapping covers no streams",
            location=MediaLocation(asset=str(probe.path), detail="empty mapping"),
        )

    # Parameter compatibility between mapped duplicates of the same kind is a
    # caller error: two different video streams with different dimensions in
    # the same output slot family is fine, but the *same* source cannot change
    # parameters between runs -- that is checked against the policy digest.
    _check_parameter_consistency(nodes)

    ignored = [
        stream.index
        for stream in probe.streams
        if stream.index not in sources and stream.copyable
    ]
    return TrackGraph(nodes=nodes, missing_required=missing_required, ignored_inputs=ignored)


def _check_parameter_consistency(nodes: list[TrackNode]) -> None:
    seen: dict[str, tuple[int, tuple[Any, ...]]] = {}
    for node in nodes:
        src = node.source
        sig = (
            src.codec,
            src.width,
            src.height,
            src.pix_fmt,
            src.sample_rate,
            src.channels,
            src.sample_format,
            src.extradata_sha256,
        )
        key = f"{src.type}:{src.codec}"
        previous = seen.get(key)
        if previous is not None and previous[1] != sig:
            raise IncompatibleTrackError(
                f"streams {previous[0]} and {src.index} share codec {src.codec!r} "
                "but have different codec parameters",
                location=MediaLocation(
                    stream=src.index,
                    rule=node.output,
                    detail="codec parameter mismatch",
                ),
            )
        seen.setdefault(key, (src.index, sig))


def describe_mapping(graph: TrackGraph) -> list[str]:
    return [
        f"out{node.output} <- in{node.source.index} ({node.source.type}/{node.source.codec})"
        for node in graph.ordered()
    ]


def mapping_errors(graph: TrackGraph) -> list[QCError]:
    errors: list[QCError] = []
    for output in graph.missing_required:
        errors.append(
            MissingTrackError(
                f"output {output} is missing its required input",
                location=MediaLocation(rule=output),
            )
        )
    return errors
