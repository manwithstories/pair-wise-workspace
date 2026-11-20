"""Manifest parsing facade.

HLS and DASH specifics live in :mod:`archive_media.manifest_hls` and
:mod:`archive_media.manifest_dash`; this module re-exports the public surface
so callers use a single import path.
"""

from __future__ import annotations

from .manifest_core import (
    SegmentPlan,
    SegmentRef,
    locator_for,
    normalize_ref,
    parse_manifest,
)
from .manifest_dash import parse_dash, render_dash
from .manifest_hls import parse_hls, render_hls


def render_plan(plan: SegmentPlan) -> str:
    """Round-trip serialization for either supported format."""
    if plan.kind == "hls":
        return render_hls(plan)
    if plan.kind == "dash":
        return render_dash(plan)
    raise ValueError(f"unknown plan kind: {plan.kind!r}")


__all__ = [
    "SegmentPlan",
    "SegmentRef",
    "locator_for",
    "normalize_ref",
    "parse_dash",
    "parse_hls",
    "parse_manifest",
    "render_dash",
    "render_hls",
    "render_plan",
]
