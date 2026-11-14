"""Element-tree probing and stable parse digests."""

from __future__ import annotations

from dataclasses import dataclass

from mkvmp4.ebml.reader import EBMLReader, ElementNode, WalkStats
from mkvmp4.mkv.parser import ParsedMedia, parse_digest, parse_matroska


@dataclass(slots=True)
class ProbeResult:
    elements: int
    unknown_size: int
    clusters: int
    digest: str
    tree: list[dict[str, object]]
    tracks: int
    chapters: int


def _node_dict(node: ElementNode, sibling_count: int) -> dict[str, object]:
    return {
        "name": node.name,
        "id": f"0x{node.element_id:X}",
        "offset": node.offset,
        "length": node.size if not node.unknown else "unknown",
        "unknown_size": node.unknown,
        "siblings": sibling_count,
        "children": [],
    }


def _serialize_nodes(nodes: list[ElementNode], depth: int, max_depth: int | None) -> list[dict[str, object]]:
    result: list[dict[str, object]] = []
    for node in nodes:
        item = _node_dict(node, len(nodes))
        if max_depth is None or depth < max_depth:
            item["children"] = _serialize_nodes(node.children, depth + 1, max_depth)
        else:
            item["children"] = []
        result.append(item)
    return result


def probe_file(path: str, *, max_depth: int | None = 4) -> ProbeResult:
    with EBMLReader(path) as reader:
        media = parse_matroska(reader, collect_nodes=True, max_depth=max_depth)
    return ProbeResult(
        elements=media.stats.elements,
        unknown_size=media.stats.unknown_size,
        clusters=media.stats.clusters,
        digest=media.digest,
        tree=_serialize_nodes(media.stats.nodes, 0, max_depth),
        tracks=len(media.tracks),
        chapters=len(media.chapters),
    )


def probe_dict(result: ProbeResult, path: str, max_depth: int | None) -> dict[str, object]:
    return {
        "file": path,
        "max_depth": max_depth,
        "elements": result.elements,
        "unknown_size": result.unknown_size,
        "clusters": result.clusters,
        "tracks": result.tracks,
        "chapters": result.chapters,
        "digest": result.digest,
        "digest_prefix": result.digest[:16],
        "tree": result.tree,
    }



