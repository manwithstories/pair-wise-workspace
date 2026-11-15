"""Element tree model and JSON summary export.

Shared by the inspect path (full tree walk) and the remux path (demux
consumes the same tree), so both export paths reuse one parser.
"""
from __future__ import annotations

import json
import os

from .reader import EbmlReader, Element
from .schema import ID_CLUSTER, ID_BLOCK, ID_SIMPLE_BLOCK


class Tree:
    """Parsed element forest plus tolerated truncation offsets."""

    def __init__(self, roots: list[Element], truncations: list[int]):
        self.roots = roots
        self.truncations = truncations

    def walk(self):
        stack = list(reversed(self.roots))
        while stack:
            el = stack.pop()
            yield el
            if el.children:
                stack.extend(reversed(el.children))


def build_tree(path, tolerant: bool = True) -> Tree:
    with EbmlReader(path, tolerant=tolerant) as reader:
        roots = reader.parse()
        return Tree(roots, list(reader.truncations))


def summarize(tree: Tree) -> dict:
    total = unknown = max_depth = clusters = blocks = 0
    for el in tree.walk():
        total += 1
        if el.unknown_size:
            unknown += 1
        if el.depth > max_depth:
            max_depth = el.depth
        if el.id == ID_CLUSTER:
            clusters += 1
        elif el.id in (ID_BLOCK, ID_SIMPLE_BLOCK):
            blocks += 1
    return {
        "elements_total": total,
        "unknown_size_elements": unknown,
        "max_depth": max_depth,
        "clusters": clusters,
        "blocks": blocks,
        "truncated_elements": len(tree.truncations),
    }


def to_jsonable(tree: Tree, max_depth: int) -> dict:
    elements = []
    for el in tree.walk():
        if el.depth > max_depth:
            continue
        elements.append(
            {
                "id": f"0x{el.id:X}",
                "name": el.name,
                "offset": el.offset,
                "size": el.size,
                "unknown_size": el.unknown_size,
                "depth": el.depth,
            }
        )
    return {"summary": summarize(tree), "elements": elements}


def export_json(tree: Tree, max_depth: int, out_path) -> dict:
    doc = to_jsonable(tree, max_depth)
    parent = os.path.dirname(os.fspath(out_path))
    if parent:
        os.makedirs(parent, exist_ok=True)
    with open(out_path, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, indent=1)
    return doc
