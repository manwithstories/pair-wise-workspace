"""Shared test helpers: build in-memory Matroska documents from fixture specs."""

from __future__ import annotations

from pathlib import Path

from src.ebml_reader import parse_bytes, ReaderOptions
from src.fixture_builder import FixtureSpec, FixtureTrack, build_fixture
from src.matroska_model import MatroskaDocument, build_document
from src import ebml_schema as ids


def make_document(
    tracks: list[tuple[str, int, list[int]]],
    *,
    scale_ns: int = 1_000_000,
    default_duration_ns: int | None = None,
    unknown: bool = True,
) -> MatroskaDocument:
    """Build a parsed document from ``[(kind, number, ticks), ...]``."""
    specs = []
    for kind, number, ticks in tracks:
        codec = {"video": "V_MPEG4/ISO/AVC", "audio": "A_AAC", "subtitle": "S_TEXT/UTF8"}[kind]
        specs.append(
            FixtureTrack(
                number=number,
                uid=1000 + number,
                kind=kind,
                codec_id=codec,
                ticks=list(ticks),
                default_duration_ns=default_duration_ns,
            )
        )
    spec = FixtureSpec(
        scale_ns=scale_ns,
        unknown_segment=unknown,
        unknown_clusters=unknown,
        tracks=specs,
        samples_per_cluster=3,
    )
    return document_from_spec(spec)


def document_from_bytes(data: bytes, *, path: Path | None = None) -> MatroskaDocument:
    root, reader = parse_bytes(
        data,
        ReaderOptions(
            keep_payload_ids=frozenset({ids.SIMPLE_BLOCK, ids.BLOCK})
        ),
    )
    return build_document(
        root,
        path=path,
        file_size=len(data),
        element_count=reader.element_count,
        unknown_length_count=reader.unknown_length_count,
        warnings=reader.warnings,
    )


def document_from_spec(spec: FixtureSpec, *, path: Path | None = None) -> MatroskaDocument:
    return document_from_bytes(build_fixture(spec), path=path)
