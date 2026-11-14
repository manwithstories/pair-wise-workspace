"""Scan Cluster blocks, rebuild sample indexes, and validate Cues offsets."""

from __future__ import annotations

from dataclasses import dataclass

from mkvtomp4.ebml.reader import EBMLReadError, decode_vint
from mkvtomp4.mkv.tracks import (
    BlockSample,
    CueStats,
    MediaIndex,
    TrackInfo,
    ZeroSamplesError,
    parse_tracks,
    timecode_scale_ns,
)
from mkvtomp4.mkv.tree import ElementNode, MatroskaTree, uint_value


@dataclass
class ParsedBlock:
    track_number: int
    relative_ms: float
    frames: list[tuple[int, int]]
    keyframe: bool


class MultiSegmentError(EBMLReadError):
    """Raised when a concatenated multi-Segment file cannot be remuxed safely."""


def _parse_lacing(payload: bytes, offset: int, flags: int) -> list[tuple[int, int]]:
    lacing = (flags >> 1) & 0x03
    if lacing == 0:
        return [(offset, len(payload) - offset)]
    if offset >= len(payload):
        raise EBMLReadError("missing lacing frame count")
    count = payload[offset] + 1
    offset += 1
    sizes: list[int] = []
    if lacing == 2:
        remaining = len(payload) - offset
        if count == 0 or remaining % count:
            raise EBMLReadError("invalid fixed-size lacing")
        sizes = [remaining // count] * count
    elif lacing == 1:
        for _ in range(count - 1):
            size = 0
            while True:
                if offset >= len(payload):
                    raise EBMLReadError("truncated Xiph lacing")
                value = payload[offset]
                offset += 1
                size += value
                if value != 255:
                    break
            sizes.append(size)
        sizes.append(len(payload) - offset - sum(sizes))
    else:
        if count == 1:
            sizes = [len(payload) - offset]
        else:
            first, length = decode_vint(payload, offset, keep_marker=False)
            offset += length
            sizes.append(first)
            for _ in range(count - 2):
                signed, length = decode_vint(payload, offset, keep_marker=False)
                offset += length
                bias = (1 << (7 * length - 1)) - 1
                sizes.append(sizes[-1] + signed - bias)
            sizes.append(len(payload) - offset - sum(sizes))
    if any(size < 0 for size in sizes) or sum(sizes) > len(payload) - offset:
        raise EBMLReadError("invalid lacing sizes")
    frames: list[tuple[int, int]] = []
    cursor = offset
    for size in sizes:
        frames.append((cursor, size))
        cursor += size
    return frames


def parse_block_payload(payload: bytes, *, sync: bool | None) -> ParsedBlock:
    track_number, length = decode_vint(payload, 0, keep_marker=False)
    if track_number <= 0 or length + 3 > len(payload):
        raise EBMLReadError("invalid Matroska block header")
    relative = int.from_bytes(payload[length : length + 2], "big", signed=True)
    flags = payload[length + 2]
    frames = _parse_lacing(payload, length + 3, flags)
    return ParsedBlock(
        track_number=track_number,
        relative_ms=float(relative),
        frames=frames,
        keyframe=bool(flags & 0x80) if sync is None else sync,
    )


def _block_groups(cluster: ElementNode) -> list[ElementNode]:
    return [child for child in cluster.children if child.eid in (0xA3, 0xA0)]


def _extract_block_node(tree: MatroskaTree, node: ElementNode) -> tuple[ElementNode, bool | None]:
    if node.eid == 0xA3:
        return node, None
    block = node.first(0xA1)
    if block is None:
        raise EBMLReadError(f"BlockGroup at {node.offset} has no Block")
    return block, node.first(0xFB) is None


def _validate_cues(
    tree: MatroskaTree,
    segment: ElementNode,
    tracks: list[TrackInfo],
) -> CueStats:
    stats = CueStats()
    cues = segment.first(0x1C53BB6B)
    if cues is None:
        return stats
    known_tracks = {track.mkv_track for track in tracks}
    cluster_offsets = {child.offset for child in segment.children if child.eid == 0x1F43B675}
    segment_end = segment.end or tree.reader.size
    for point in cues.find_all(0xBB):
        stats.entries += 1
        for position in point.find_all(0xB7):
            track = uint_value(tree, position.first(0xF7))
            cluster_position = position.first(0xF1)
            if cluster_position is None or track not in known_tracks:
                stats.rejected += 1
                continue
            absolute = segment.data_offset + uint_value(tree, cluster_position)
            if absolute < segment.data_offset or absolute >= segment_end:
                stats.rejected += 1
            elif absolute not in cluster_offsets:
                stats.rejected += 1
            else:
                stats.valid += 1
    return stats


def _scan_cluster(
    tree: MatroskaTree,
    cluster: ElementNode,
    tracks_by_number: dict[int, TrackInfo],
    scale_ms: float,
    segment_index: int,
    sequence: list[int],
    samples: dict[int, list[BlockSample]],
) -> None:
    cluster_time = uint_value(tree, cluster.first(0xE7))
    for group in _block_groups(cluster):
        block, group_sync = _extract_block_node(tree, group)
        if block.size is None:
            raise EBMLReadError("block has no payload size")
        payload = tree.reader.read_range(block.data_offset, block.size)
        parsed = parse_block_payload(payload, sync=group_sync)
        track = tracks_by_number.get(parsed.track_number)
        if track is None:
            continue
        duration_ms = track.default_duration_ns / 1_000_000.0
        if duration_ms <= 0:
            duration_ms = 1.0
        if group.eid == 0xA0:
            block_duration = group.first(0x9B)
            if block_duration is not None:
                duration_ms = uint_value(tree, block_duration) * scale_ms
        for frame_index, (frame_offset, frame_size) in enumerate(parsed.frames):
            pts = cluster_time * scale_ms + parsed.relative_ms + frame_index * duration_ms
            sequence[0] += 1
            samples[parsed.track_number].append(
                BlockSample(
                    track_number=parsed.track_number,
                    pts_ms=pts,
                    dts_ms=pts,
                    duration_ms=duration_ms,
                    keyframe=parsed.keyframe,
                    source_offset=block.data_offset + frame_offset,
                    size=frame_size,
                    sequence=sequence[0],
                    segment_index=segment_index,
                )
            )


def parse_media_index(path: str) -> MediaIndex:
    tree = MatroskaTree(path)
    if len(tree.segments()) > 1:
        raise MultiSegmentError("multiple Segment elements require an explicit concat step")
    tracks, skipped, segment = parse_tracks(tree)
    if segment is None:
        raise ZeroSamplesError("no Segment available for sample extraction")
    scale_ns = timecode_scale_ns(tree, segment)
    scale_ms = scale_ns / 1_000_000.0
    cue_stats = _validate_cues(tree, segment, tracks)
    samples: dict[int, list[BlockSample]] = {track.mkv_track: [] for track in tracks}
    tracks_by_number = {track.mkv_track: track for track in tracks}
    sequence = [0]
    segment_index = tree.segments().index(segment)
    for cluster in segment.find_all(0x1F43B675):
        _scan_cluster(
            tree, cluster, tracks_by_number, scale_ms, segment_index, sequence, samples
        )
    if any(not items for items in samples.values()):
        empty = [number for number, items in samples.items() if not items]
        raise ZeroSamplesError(f"mapped track(s) have zero samples: {empty}")
    has_unknown_cluster = any(
        child.eid == 0x1F43B675 and child.was_unknown for child in segment.children
    )
    if cue_stats.valid > 0 and not has_unknown_cluster:
        cue_stats.index_source = "cues"
        cue_stats.rebuilt_samples = 0
    else:
        cue_stats.index_source = "scan"
        cue_stats.rebuilt_samples = sum(len(items) for items in samples.values())
    return MediaIndex(
        path=path,
        tree=tree,
        tracks=tracks,
        skipped=skipped,
        samples=samples,
        timecode_scale_ns=scale_ns,
        cue_stats=cue_stats,
        warnings=list(tree.warnings),
        segment=segment,
    )
