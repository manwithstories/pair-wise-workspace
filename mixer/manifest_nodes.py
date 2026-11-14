"""Node parameter validation and canonical graph construction for manifests."""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from .automation import _db, _finite
from .errors import ManifestError
from .graph import Node, parse_nodes

if TYPE_CHECKING:
    from .manifest import Track


def validate_node_params(nodes: dict[str, Node]) -> None:
    for node in nodes.values():
        params = node.params
        if node.type == "read" and node.track is None:
            raise ManifestError(f"read node {node.id!r} requires track", details={"node_id": node.id})
        if node.type == "gain" and "gain_db" in params:
            _db(params["gain_db"], f"node {node.id}.gain_db")
        if node.type == "pan" and "pan" in params:
            pan = _finite(params["pan"], f"node {node.id}.pan")
            if not -1.0 <= pan <= 1.0:
                raise ManifestError(
                    f"node {node.id!r} pan must be in [-1, 1]",
                    details={"node_id": node.id, "pan": pan},
                )
        if node.type == "trim" and "max_samples" in params:
            max_samples = params["max_samples"]
            if isinstance(max_samples, bool) or not isinstance(max_samples, int) or max_samples < 0:
                raise ManifestError(
                    f"node {node.id!r} max_samples must be a non-negative integer",
                    details={"node_id": node.id},
                )
        if node.type == "limiter":
            ceiling = _finite(params.get("ceiling", 1.0), f"node {node.id}.ceiling")
            softness = _finite(params.get("softness", 1.0), f"node {node.id}.softness")
            if not 0.0 < ceiling <= 1.0:
                raise ManifestError(
                    f"node {node.id!r} ceiling must be in (0, 1]",
                    details={"node_id": node.id, "ceiling": ceiling},
                )
            if softness < 1.0:
                raise ManifestError(
                    f"node {node.id!r} softness must be >= 1",
                    details={"node_id": node.id},
                )


def canonical_nodes(
    tracks: list["Track"], buses: list[dict[str, Any]], duration_frames: int
) -> tuple[dict[str, Node], str]:
    bus = buses[0]
    declarations: list[dict[str, Any]] = []
    sum_inputs: dict[str, str] = {}
    for index, track in enumerate(tracks):
        read_id = f"{track.id}.read"
        gain_id = f"{track.id}.gain"
        pan_id = f"{track.id}.pan"
        declarations.extend(
            [
                {"id": read_id, "type": "read", "track": track.id},
                {"id": gain_id, "type": "gain", "track": track.id, "input": read_id},
                {"id": pan_id, "type": "pan", "track": track.id, "input": gain_id},
            ]
        )
        sum_inputs[f"in{index}"] = pan_id
    bus_name = bus["id"]
    declarations.extend(
        [
            {"id": f"{bus_name}.sum", "type": "sum", "inputs": sum_inputs},
            {
                "id": f"{bus_name}.gain",
                "type": "gain",
                "gain_db": bus["gain_db"],
                "input": f"{bus_name}.sum",
            },
            {
                "id": f"{bus_name}.trim",
                "type": "trim",
                "max_samples": duration_frames,
                "input": f"{bus_name}.gain",
            },
            {
                "id": f"{bus_name}.limiter",
                "type": "limiter",
                "ceiling": bus.get("ceiling", 1.0),
                "softness": bus.get("softness", 1.0),
                "input": f"{bus_name}.trim",
            },
        ]
    )
    nodes = parse_nodes(declarations)
    validate_node_params(nodes)
    return nodes, f"{bus_name}.limiter"
