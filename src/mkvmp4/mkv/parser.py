"""Streaming semantic parser built on :mod:`mkvmp4.ebml.reader`."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field

from mkvmp4.ebml.ids import (
    CHAPTERS, CLUSTER, CRC32, INFO, TRACKS, VOID,
)
from mkvmp4.ebml.reader import EBMLDecodeError, EBMLReader, ElementHeader, ElementNode, WalkStats
from .model import Chapter, Info, Track
from .timeline import TimelineError, parse_block_header


@dataclass(slots=True)
class ParsedBlock:
    track_number: int
    relative_ticks: int
    flags: int
    keyframe: bool
    payload_offset: int
    payload_size: int
    cluster_index: int
    duration_ticks: int | None = None
    reference_blocks: tuple[int, ...] = ()


@dataclass(slots=True)
class ParsedCluster:
    index: int
    offset: int
    timestamp_ticks: int = 0
    block_count: int = 0


@dataclass(slots=True)
class ParsedMedia:
    info: Info = field(default_factory=Info)
    tracks: list[Track] = field(default_factory=list)
    chapters: list[Chapter] = field(default_factory=list)
    clusters: list[ParsedCluster] = field(default_factory=list)
    blocks: list[ParsedBlock] = field(default_factory=list)
    stats: WalkStats = field(default_factory=WalkStats)
    digest: str = ""


class MediaParser:
    """Incrementally extracts metadata while retaining only block locations."""

    def __init__(self, reader: EBMLReader, *, collect_nodes: bool = False,
                 max_depth: int | None = None):
        self.reader = reader
        self.collect_nodes = collect_nodes
        self.max_depth = max_depth
        self.media = ParsedMedia()
        self._hash = hashlib.sha256()
        self._cluster: ParsedCluster | None = None
        self._chapter_ticks: dict[int, dict[str, int | str]] = {}
        self._chapter_index = -1
        self._nodes: list[ElementNode] = []

    def parse(self) -> ParsedMedia:
        stats = self.reader.walk(self._on_element, max_depth=self.max_depth,
                                 collect_nodes=self.collect_nodes)
        self.media.stats = stats
        scale = self.media.info.timestamp_scale_ns
        for index, entry in enumerate(self._chapter_ticks.values()):
            start = int(entry.get("start", 0)) * scale
            end_raw = entry.get("end")
            self.media.chapters[index].start_ns = start
            self.media.chapters[index].end_ns = int(end_raw) * scale if end_raw is not None else None
        self.media.digest = self._hash.hexdigest()
        return self.media

    def _on_element(self, header: ElementHeader) -> None:
        self._hash.update(
            f"{header.element_id:X}:{header.offset}:{header.size}:{header.unknown};".encode())
        if header.element_id in (VOID, CRC32):
            return
        depth = header.depth
        if header.element_id == INFO and depth == 1:
            self._parse_info_children(header)
        elif header.element_id == TRACKS and depth == 1:
            self._parse_tracks(header)
        elif header.element_id == CLUSTER and depth == 1:
            cluster = ParsedCluster(len(self.media.clusters), header.offset)
            self._cluster = cluster
            self.media.clusters.append(cluster)
        elif header.element_id == CHAPTERS and depth == 1:
            self._parse_chapters(header)
        elif header.element_id == 0xA3 and depth == 2 and self._cluster is not None:
            self._record_simple_block(header)
        elif header.element_id == 0xA0 and depth == 2 and self._cluster is not None:
            self._record_block_group(header)
        elif self._cluster is not None and header.element_id == 0xE7 and depth == 2:
            self._cluster.timestamp_ticks = self.reader.read_payload_uint(header)

    def _parse_info_children(self, parent: ElementHeader) -> None:
        end = parent.end or self.reader.size
        pos = parent.data_offset
        while pos < end:
            self.reader.seek(pos)
            child = self.reader.read_header(parent.depth + 1)
            if child.element_id == 0x2AD7B1:
                self.media.info.timestamp_scale_ns = self.reader.read_payload_uint(child) or 1_000_000
            elif child.element_id == 0x4489:
                self.media.info.duration_ticks = self.reader.read_payload_float(child)
            elif child.element_id == 0x4D80:
                self.media.info.muxing_app = self.reader.read_payload_string(child)
            elif child.element_id == 0x5741:
                self.media.info.writing_app = self.reader.read_payload_string(child)
            elif child.element_id == 0x7BA9:
                self.media.info.title = self.reader.read_payload_string(child)
            pos = child.end or end

    def _parse_tracks(self, parent: ElementHeader) -> None:
        end = parent.end or self.reader.size
        pos = parent.data_offset
        while pos < end:
            self.reader.seek(pos)
            child = self.reader.read_header(parent.depth + 1)
            if child.element_id == 0xAE:
                current = Track(number=0)
                self.media.tracks.append(current)
                self._parse_track_fields(current, child)
            pos = child.end or end
        if not self.media.tracks:
            raise EBMLDecodeError("Tracks contains no TrackEntry")
        if any(track.number <= 0 for track in self.media.tracks):
            raise EBMLDecodeError("TrackEntry is missing TrackNumber")

    def _parse_track_fields(self, current: Track, parent: ElementHeader) -> None:
        end = parent.end or self.reader.size
        pos = parent.data_offset
        while pos < end:
            self.reader.seek(pos)
            child = self.reader.read_header(parent.depth + 1)
            if child.element_id == 0xD7:
                current.number = self.reader.read_payload_uint(child)
            elif child.element_id == 0x73C5:
                current.uid = self.reader.read_payload_uint(child)
            elif child.element_id == 0x83:
                current.track_type = self.reader.read_payload_uint(child)
            elif child.element_id == 0x86:
                current.codec_id = self.reader.read_payload_string(child)
            elif child.element_id == 0x63A2:
                current.codec_private = self.reader.read_payload(child)
            elif child.element_id == 0x536E:
                current.name = self.reader.read_payload_string(child)
            elif child.element_id in (0x22B59C, 0x22B59D):
                current.language = self.reader.read_payload_string(child)
            elif child.element_id == 0x88:
                current.default = bool(self.reader.read_payload_uint(child))
            elif child.element_id == 0x55AA:
                current.forced = bool(self.reader.read_payload_uint(child))
            elif child.element_id == 0x23E383:
                current.default_duration_ns = self.reader.read_payload_uint(child)
            elif child.element_id in (0xE0, 0xE1):
                self._parse_track_extra(current, child)
            pos = child.end or end

    def _parse_track_extra(self, track: Track, parent: ElementHeader) -> None:
        end = parent.end or self.reader.size
        pos = parent.data_offset
        while pos < end:
            self.reader.seek(pos)
            child = self.reader.read_header(parent.depth + 1)
            if child.element_id == 0xB0:
                track.pixel_width = self.reader.read_payload_uint(child)
            elif child.element_id == 0xBA:
                track.pixel_height = self.reader.read_payload_uint(child)
            elif child.element_id == 0xB5:
                track.sampling_frequency = self.reader.read_payload_float(child)
            elif child.element_id == 0x9F:
                track.channels = self.reader.read_payload_uint(child)
            pos = child.end or end

    def _parse_chapters(self, parent: ElementHeader) -> None:
        end = parent.end or self.reader.size
        pos = parent.data_offset
        while pos < end:
            self.reader.seek(pos)
            child = self.reader.read_header(parent.depth + 1)
            if child.element_id == 0x45B9:
                self._parse_edition(child)
            elif child.element_id == 0xB6:
                self._parse_chapter_atom(child)
            pos = child.end or end
        self.media.chapters.extend(Chapter(
            uid=int(entry.get("uid", 0)), start_ns=0,
            title=str(entry.get("title", "")), language=str(entry.get("language", "und")))
            for entry in self._chapter_ticks.values())

    def _parse_edition(self, parent: ElementHeader) -> None:
        end = parent.end or self.reader.size
        pos = parent.data_offset
        while pos < end:
            self.reader.seek(pos)
            child = self.reader.read_header(parent.depth + 1)
            if child.element_id == 0xB6:
                self._parse_chapter_atom(child)
            pos = child.end or end

    def _parse_chapter_atom(self, parent: ElementHeader) -> None:
        self._chapter_index += 1
        index = self._chapter_index
        self._chapter_ticks[index] = {"start": 0, "uid": 0, "title": "", "language": "und"}
        end = parent.end or self.reader.size
        pos = parent.data_offset
        while pos < end:
            self.reader.seek(pos)
            child = self.reader.read_header(parent.depth + 1)
            if child.element_id == 0x73C4:
                self._chapter_ticks[self._chapter_index]["uid"] = self.reader.read_payload_uint(child)
            elif child.element_id == 0x91:
                self._chapter_ticks[self._chapter_index]["start"] = self.reader.read_payload_uint(child)
            elif child.element_id == 0x92:
                self._chapter_ticks[self._chapter_index]["end"] = self.reader.read_payload_uint(child)
            elif child.element_id == 0x80:
                self._parse_chapter_display(index, child)
            elif child.element_id == 0xB6:
                self._parse_chapter_atom(child)
            pos = child.end or end

    def _parse_chapter_display(self, index: int, parent: ElementHeader) -> None:
        end = parent.end or self.reader.size
        pos = parent.data_offset
        while pos < end:
            self.reader.seek(pos)
            child = self.reader.read_header(parent.depth + 1)
            if child.element_id == 0x85:
                self._chapter_ticks[index]["title"] = self.reader.read_payload_string(child)
            elif child.element_id in (0x437C, 0x437D):
                self._chapter_ticks[index]["language"] = self.reader.read_payload_string(child)
            pos = child.end or end

    def _record_simple_block(self, header: ElementHeader) -> None:
        assert self._cluster is not None
        self.reader.seek(header.data_offset)
        prefix = self.reader.read(min(8, header.size or 0))
        try:
            parsed = parse_block_header(prefix)
        except TimelineError as exc:
            raise EBMLDecodeError(f"invalid SimpleBlock at {header.offset}: {exc}") from exc
        if parsed.payload_offset > (header.size or 0):
            raise EBMLDecodeError("block header larger than element")
        self.media.blocks.append(ParsedBlock(
            parsed.track_number, parsed.relative_ticks, parsed.flags, parsed.keyframe,
            header.data_offset, header.size or 0, self._cluster.index))
        self._cluster.block_count += 1

    def _record_block_group(self, header: ElementHeader) -> None:
        assert self._cluster is not None
        end = header.end or self.reader.size
        pos = header.data_offset
        block: ParsedBlock | None = None
        duration: int | None = None
        refs: list[int] = []
        while pos < end:
            self.reader.seek(pos)
            child = self.reader.read_header(header.depth + 1)
            if child.element_id == 0xA1:
                self.reader.seek(child.data_offset)
                prefix = self.reader.read(min(8, child.size or 0))
                parsed = parse_block_header(prefix)
                block = ParsedBlock(parsed.track_number, parsed.relative_ticks,
                                    parsed.flags, parsed.keyframe, child.data_offset,
                                    child.size or 0, self._cluster.index)
            elif child.element_id == 0x9B:
                duration = self.reader.read_payload_uint(child)
            elif child.element_id == 0xFB:
                refs.append(self.reader.read_payload_int(child))
            pos = child.end or end
        if block is None:
            raise EBMLDecodeError("BlockGroup has no Block")
        block.duration_ticks = duration
        block.reference_blocks = tuple(refs)
        self.media.blocks.append(block)
        self._cluster.block_count += 1


def parse_matroska(reader: EBMLReader, *, collect_nodes: bool = False,
                   max_depth: int | None = None) -> ParsedMedia:
    return MediaParser(reader, collect_nodes=collect_nodes, max_depth=max_depth).parse()


def parse_digest(path: str) -> str:
    """Return the canonical digest shared by probe and remux."""
    with EBMLReader(path) as reader:
        return parse_matroska(reader).digest

