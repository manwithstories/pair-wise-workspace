"""Block/SimpleBlock payload decoding, including all three lacing modes."""

from __future__ import annotations

from . import ebml_schema as ids
from ._matroska_types import Block, Cluster, Frame, ModelError
from ._matroska_track import _uint_child
from .ebml_schema import decode_payload


# ── block payload decoding ────────────────────────────────────────────────


def _read_vint_raw(data: bytes, pos: int) -> tuple[int, int]:
    if pos >= len(data):
        raise ModelError("block_truncated", f"block VINT at {pos} past payload")
    first = data[pos]
    if first == 0:
        raise ModelError("block_vint_invalid", f"block VINT marker 0x00 at {pos}")
    width = 9 - first.bit_length()
    if width < 1 or width > 8:
        raise ModelError("block_vint_invalid", f"block VINT width {width} at {pos}")
    if pos + width > len(data):
        raise ModelError("block_truncated", f"block VINT at {pos} runs past payload")
    raw = int.from_bytes(data[pos : pos + width], "big")
    return raw & ((1 << (7 * width)) - 1), width


def parse_block_payload(
    payload: bytes,
    *,
    data_offset: int,
    cluster_timecode: int,
    is_simple: bool,
    strict: bool = True,
) -> tuple[int, int, int, str, list[tuple[int, int]]]:
    """Decode a Block/SimpleBlock payload.

    Returns ``(track_number, relative_timecode, flags, lacing, frames)`` where
    ``frames`` are ``(absolute_offset, size)`` pairs. Absolute offsets point at
    the frame bytes inside the source file.
    """
    if len(payload) < 4:
        raise ModelError("block_truncated", "block payload shorter than 4 octets")
    track_number, width = _read_vint_raw(payload, 0)
    if track_number == 0:
        raise ModelError("block_track_zero", "block track number 0 is invalid")
    pos = width
    if pos + 2 > len(payload):
        raise ModelError("block_truncated", "block missing relative timestamp")
    relative = int.from_bytes(payload[pos : pos + 2], "big", signed=True)
    pos += 2
    flags = payload[pos]
    pos += 1
    lacing_bits = (flags >> 1) & 0x03
    lacing = {0: "none", 1: "xiph", 2: "fixed", 3: "ebml"}[lacing_bits]
    if flags & 0x06 == 0x02:
        # Reserved lacing pattern is only reachable through bit math above.
        pass

    frame_count = 1
    sizes: list[int] = []
    if lacing == "none":
        if pos > len(payload):
            raise ModelError("block_truncated", "block payload ended before frame")
        sizes.append(len(payload) - pos)
    else:
        if pos >= len(payload):
            raise ModelError("block_truncated", "laced block missing lace count")
        count = payload[pos]
        pos += 1
        frame_count = count + 1
        if frame_count <= 0:
            raise ModelError("block_lacing_invalid", "laced block has no frames")
        if lacing == "fixed":
            remaining = len(payload) - pos
            if remaining % frame_count != 0:
                raise ModelError(
                    "block_lacing_invalid",
                    f"fixed lacing {frame_count} frames over {remaining} bytes",
                )
            sizes = [remaining // frame_count] * frame_count
        elif lacing == "xiph":
            for _ in range(frame_count - 1):
                size = 0
                while True:
                    if pos >= len(payload):
                        raise ModelError("block_truncated", "xiph lace size truncated")
                    byte = payload[pos]
                    pos += 1
                    size += byte
                    if byte != 255:
                        break
                sizes.append(size)
            sizes.append(len(payload) - pos - sum(sizes))
        else:  # ebml lacing
            first_size, width = _read_vint_raw(payload, pos)
            pos += width
            sizes.append(first_size)
            prev = first_size
            for _ in range(frame_count - 2):
                delta, width = _read_vint_raw(payload, pos)
                pos += width
                # Signed deltas use a bias of (2^(7*width) - 1): the top value
                # bit is the sign, and negative values are offset by the mask.
                sign_bit = 1 << (7 * width - 1)
                if delta & sign_bit:
                    delta -= (1 << (7 * width)) - 1
                prev = prev + delta
                if prev < 0:
                    raise ModelError("block_lacing_invalid", "EBML lace size went negative")
                sizes.append(prev)
            if frame_count > 1:
                sizes.append(len(payload) - pos - sum(sizes))
        if any(size < 0 for size in sizes[:-1]):
            raise ModelError("block_lacing_invalid", "lace size negative")
    if sum(sizes) + pos != len(payload):
        if strict:
            raise ModelError(
                "block_lacing_invalid",
                f"laced frame sizes {sum(sizes)} do not fill payload {len(payload) - pos}",
            )
        sizes[-1] = len(payload) - pos - sum(sizes[:-1])
        if sizes[-1] < 0:
            raise ModelError(
                "block_lacing_invalid",
                "final laced frame has negative size",
            )

    frames: list[tuple[int, int]] = []
    cursor = data_offset + pos
    for size in sizes:
        frames.append((cursor, size))
        cursor += size
    return track_number, relative, flags, lacing, frames


def build_cluster(
    cluster_el: Element,
    *,
    index: int,
    warnings: list[dict[str, object]],
) -> Cluster:
    timecode = _uint_child(cluster_el, ids.TIMESTAMP, 0) or 0
    cluster = Cluster(
        offset=cluster_el.offset,
        data_offset=cluster_el.data_offset,
        size=cluster_el.resolved_size,
        unknown_size=cluster_el.unknown_size,
        timecode=timecode,
        position=_uint_child(cluster_el, ids.POSITION),
        index=index,
    )
    for child in cluster_el.children:
        if child.element_id == ids.SIMPLE_BLOCK:
            block = _build_block(
                child,
                cluster=cluster,
                is_simple=True,
                duration=None,
                references=[],
                group_offset=None,
                warnings=warnings,
            )
            cluster.blocks.append(block)
        elif child.element_id == ids.BLOCK_GROUP:
            group_blocks = _build_block_group(child, cluster=cluster, warnings=warnings)
            cluster.blocks.extend(group_blocks)
    cluster.blocks.sort(key=lambda b: b.data_offset)
    return cluster


def _build_block_group(
    group: Element, *, cluster: Cluster, warnings: list[dict[str, object]]
) -> list[Block]:
    block_el = group.child(ids.BLOCK)
    if block_el is None:
        warnings.append(
            {
                "code": "blockgroup_without_block",
                "message": "BlockGroup has no Block; skipped",
                "offset": group.offset,
            }
        )
        return []
    duration = _uint_child(group, ids.BLOCK_DURATION)
    references = []
    for ref in group.children_named(ids.REFERENCE_BLOCK):
        if ref.data is not None:
            references.append(decode_payload("int", ref.data))
    block = _build_block(
        block_el,
        cluster=cluster,
        is_simple=False,
        duration=duration,
        references=references,
        group_offset=group.offset,
        warnings=warnings,
    )
    return [block]


def _build_block(
    block_el: Element,
    *,
    cluster: Cluster,
    is_simple: bool,
    duration: int | None,
    references: list[int],
    group_offset: int | None,
    warnings: list[dict[str, object]],
) -> Block:
    payload = block_el.data
    if payload is None:
        raise ModelError(
            "block_payload_missing",
            "block payload was not retained by the reader",
            block_el.offset,
        )
    track_number, relative, flags, lacing, raw_frames = parse_block_payload(
        payload,
        data_offset=block_el.data_offset,
        cluster_timecode=cluster.timecode,
        is_simple=is_simple,
        strict=True,
    )
    frames = [Frame(index=i, offset=off, size=size) for i, (off, size) in enumerate(raw_frames)]
    if is_simple:
        keyframe = bool(flags & 0x80)
    else:
        keyframe = not references
    block = Block(
        track_number=track_number,
        relative_timecode=relative,
        cluster_timecode=cluster.timecode,
        flags=flags,
        data_offset=block_el.data_offset,
        size=block_el.declared_size,
        frames=frames,
        keyframe=keyframe,
        invisible=bool(flags & 0x08),
        discardable=bool(flags & 0x01),
        lacing=lacing,
        duration_ticks=duration,
        references=list(references),
        group_offset=group_offset,
        cluster_index=cluster.index,
    )
    if not is_simple and any(f.size == 0 for f in frames):
        warnings.append(
            {
                "code": "empty_block_frame",
                "message": f"block for track {track_number} has a zero-size frame",
                "offset": block_el.offset,
            }
        )
    return block


