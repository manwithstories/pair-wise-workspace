"""Stable JSON summary of an EBML element tree."""

from __future__ import annotations

from collections import Counter

from mkvtool.ebml.tree import Element, ParseResult


def _unknown_entry(element: Element) -> dict:
    return {
        "path": element.path,
        "element": element.name,
        "id": f"0x{element.element_id:X}",
        "offset": element.offset,
        "data_offset": element.data_offset,
        "end_offset": element.end_offset,
        "boundary_reason": element.boundary_reason,
        "truncated": element.truncated,
    }


def build_tree_summary(result: ParseResult, source: str | None = None) -> dict:
    elements = list(result.walk())
    depths = Counter(item.depth for item in elements)
    names = Counter(item.name for item in elements)
    unknown = [item for item in elements if item.unknown_size]
    max_depth = max((item.depth for item in elements), default=0)
    summary = {
        "source": source,
        "file_size": len(result.data),
        "element_count": len(elements),
        "max_depth": max_depth,
        "depth_counts": {str(depth): depths[depth] for depth in sorted(depths)},
        "unknown_size_elements": len(unknown),
        "unknown_size_paths": [_unknown_entry(item) for item in unknown],
        "top_level": [item.name for item in result.roots],
        "element_name_counts": dict(sorted(names.items())),
        "truncated": result.truncated,
        "truncation_offset": result.truncation_offset,
        "issues": list(result.issues),
    }
    return summary
