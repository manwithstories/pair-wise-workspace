"""Generate every test fixture locally with PyAV; no external media is used.

Fixtures produced (all under ``--out``):

===============  ===========================================================
good.mkv         3 video GOPs + stereo 44.1 kHz PCM, BT.709 tags
good.mp4         the same material in MP4, exercising the colr-box reader
good.mov         the same material in MOV, exercising the brand whitelist
multi.mkv        two video tracks + one audio track, for explicit mapping
empty_track.mkv  a mapped audio track that carries no packets
truncated.mkv    a good file cut off before its declared duration
damaged.mp4      a good file with one interior GOP's payload scrambled
damaged.mkv      a good file with its last keyframe payload scrambled
boundary.mkv     video and audio start offsets that are not multiples of a tick
color_conflict.mkv  codec colour says BT.709 while the stream tag says BT.2020
keyframe_index.mkv  one keyframe flag cleared, so the index has a hole
===============  ===========================================================

Each file is small, deterministic in content and safe to delete; the tests
regenerate them into a temporary directory.
"""

from __future__ import annotations

import argparse
import os
import struct
import sys
from fractions import Fraction
from pathlib import Path

import av
import numpy as np

VIDEO_SIZE = (160, 90)
VIDEO_RATE = 24
GOP = 48
TONE_HZ = 440.0
AUDIO_RATE = 44_100
DURATION_S = 6.0


def _silent_audio_layout(layout: str = "stereo") -> av.AudioLayout:
    return av.AudioLayout(layout)


def _video_frame(index: int) -> av.VideoFrame:
    height, width = VIDEO_SIZE[1], VIDEO_SIZE[0]
    array = np.zeros((height, width, 3), dtype=np.uint8)
    ramp = np.arange(width, dtype=np.uint8)
    array[:, :, 0] = ramp[None, :]
    array[:, :, 1] = np.uint8((index * 5) % 256)
    array[:, :, 2] = np.uint8((index * 11) % 256)
    frame = av.VideoFrame.from_ndarray(array, format="rgb24")
    frame.pts = index
    return frame


def _tone(seconds: float, rate: int, channels: int, amplitude: float = 0.08) -> np.ndarray:
    t = np.arange(int(rate * seconds)) / rate
    base = amplitude * np.sin(2 * np.pi * TONE_HZ * t)
    return np.stack([base] * channels, axis=0).astype(np.float32)


def _video_stream(out: av.container.OutputContainer, *, gop: int = GOP) -> av.VideoStream:
    stream = out.add_stream("libx264", rate=VIDEO_RATE)
    stream.width, stream.height = VIDEO_SIZE
    stream.pix_fmt = "yuv420p"
    stream.options = {
        "g": str(gop),
        "keyint_min": str(gop),
        "sc_threshold": "0",
        "preset": "veryfast",
        "crf": "28",
    }
    stream.codec_context.color_primaries = 1  # bt709
    stream.codec_context.color_trc = 1
    stream.codec_context.colorspace = 1
    stream.codec_context.color_range = 1
    stream.metadata["COLOR_PRIMARIES"] = "bt709"
    stream.metadata["COLOR_TRANSFER"] = "bt709"
    stream.metadata["COLOR_SPACE"] = "bt709"
    return stream


def _audio_stream(out: av.container.OutputContainer, rate: int, channels: int) -> av.AudioStream:
    stream = out.add_stream("pcm_s16le", rate=rate)
    stream.codec_context.layout = "stereo" if channels == 2 else "mono"
    stream.time_base = Fraction(1, rate)
    return stream


def _write_video(out, stream, start: int, count: int) -> None:
    for offset in range(count):
        frame = _video_frame(start + offset)
        frame.pts = start + offset
        for packet in stream.encode(frame):
            out.mux(packet)


def _write_audio(out, stream, samples: np.ndarray, pts: int, rate: int) -> int:
    block = 2048
    cursor = 0
    while cursor < samples.shape[1]:
        chunk = samples[:, cursor: cursor + block]
        frame = av.AudioFrame.from_ndarray(
            np.ascontiguousarray(chunk), format="fltp", layout=stream.codec_context.layout.name
        )
        frame.sample_rate = rate
        frame.pts = pts + cursor
        frame.time_base = Fraction(1, rate)
        for packet in stream.encode(frame):
            out.mux(packet)
        cursor += chunk.shape[1]
    return pts + samples.shape[1]


def _flush(out, *streams) -> None:
    for stream in streams:
        try:
            for packet in stream.encode(None):
                out.mux(packet)
        except Exception:  # noqa: BLE001 - an empty stream may refuse to flush
            pass


def make_good(path: Path, *, container: str = "matroska", audio: bool = True,
              rate: int = AUDIO_RATE, channels: int = 2,
              video_offset: int = 0, audio_offset: int = 0) -> None:
    """Encode the baseline fixture; offsets are in stream ticks."""
    out = av.open(str(path), "w", format=container)
    out.metadata["COMMENT"] = "mediarc fixture; generated locally"
    video = _video_stream(out)
    if video_offset:
        video.time_base = Fraction(1, 1000)
    audio_stream = _audio_stream(out, rate, channels) if audio else None
    count = int(DURATION_S * VIDEO_RATE)
    _write_video(out, video, video_offset, count)
    if audio_stream is not None:
        samples = _tone(DURATION_S, rate, channels)
        _write_audio(out, audio_stream, samples, audio_offset, rate)
    _flush(out, *([video, audio_stream] if audio_stream is not None else [video]))
    out.close()


def make_multitrack(path: Path) -> None:
    out = av.open(str(path), "w", format="matroska")
    first = _video_stream(out)
    second = _video_stream(out)
    audio_stream = _audio_stream(out, AUDIO_RATE, 2)
    count = int(DURATION_S * VIDEO_RATE)
    for index in range(count):
        for stream in (first, second):
            frame = _video_frame(index)
            frame.pts = index
            for packet in stream.encode(frame):
                out.mux(packet)
    _flush(out, first, second)
    _write_audio(out, audio_stream, _tone(DURATION_S, AUDIO_RATE, 2), 0, AUDIO_RATE)
    _flush(out, audio_stream)
    out.close()


def make_empty_track(path: Path) -> None:
    """A container that declares an audio stream but never writes audio."""
    out = av.open(str(path), "w", format="matroska")
    video = _video_stream(out)
    audio_stream = _audio_stream(out, AUDIO_RATE, 2)
    _write_video(out, video, 0, int(DURATION_S * VIDEO_RATE))
    _flush(out, video)
    _flush(out, audio_stream)
    out.close()


def truncate_file(src: Path, dst: Path, ratio: float) -> None:
    data = src.read_bytes()
    dst.write_bytes(data[: int(len(data) * ratio)])


def _packet_payload_offset(src: Path, stream_index: int, pts: int) -> tuple[int, int] | None:
    """Locate a packet's payload in the file: (file offset, payload length)."""
    data = src.read_bytes()
    container = av.open(str(src))
    try:
        stream = container.streams[stream_index]
        for packet in container.demux(stream):
            if packet.pts != pts or packet.pos is None:
                continue
            header = data[packet.pos: packet.pos + 32]
            body = bytes(packet)[: min(16, packet.size)]
            delta = header.find(body)
            if delta < 0:
                return None
            return packet.pos + delta, packet.size
    finally:
        container.close()
    return None


def scramble_packet(src: Path, dst: Path, stream_index: int, pts: int,
                     keep_head: int = 0, length: int = 256) -> bool:
    """Damage one packet's payload **in place**, preserving every container box.

    For MP4 the file is rewritten in place (never truncated) so the ``moov``
    index at the end survives and the damage stays a payload defect rather than
    an unreadable file.
    """
    located = _packet_payload_offset(src, stream_index, pts)
    if located is None:
        return False
    offset, size = located
    data = bytearray(src.read_bytes())
    start = offset + keep_head
    stop = min(len(data), start + min(length, size - keep_head))
    if stop <= start:
        return False
    data[start:stop] = bytes((byte ^ 0x5A) for byte in data[start:stop])
    dst.write_bytes(bytes(data))
    return True


def scramble_all_keyframes(src: Path, dst: Path, length: int = 64) -> int:
    """Damage the payload of every video keyframe; returns how many were hit.

    Used for the "unrecoverable" fixture: with no intact keyframe anywhere there
    is no safe boundary to resume from, so the repair path must decline rather
    than emit a file that only looks repaired.
    """
    data = bytearray(src.read_bytes())
    container = av.open(str(src))
    hits: list[tuple[int, int]] = []
    try:
        stream = container.streams.video[0]
        for packet in container.demux(stream):
            if packet.is_keyframe and packet.pos is not None and packet.size > length:
                payload = bytes(packet)[:8]
                delta = data[packet.pos:packet.pos + 16].find(payload)
                if delta >= 0:
                    hits.append((packet.pos + delta, length))
    finally:
        container.close()
    for offset, count in hits:
        data[offset:offset + count] = bytes(
            byte ^ 0x5A for byte in data[offset:offset + count]
        )
    dst.write_bytes(bytes(data))
    return len(hits)


def make_boundary(path: Path) -> None:
    """Video starts at 1.5 s, audio at 0.5 s; both edges are off-tick."""
    out = av.open(str(path), "w", format="matroska")
    video = out.add_stream("libx264", rate=VIDEO_RATE)
    video.width, video.height = VIDEO_SIZE
    video.pix_fmt = "yuv420p"
    video.options = {"g": str(GOP), "keyint_min": str(GOP), "sc_threshold": "0",
                     "preset": "veryfast", "crf": "30"}
    video.time_base = Fraction(1, 1000)
    audio_stream = _audio_stream(out, AUDIO_RATE, 2)
    for index in range(int(4.5 * VIDEO_RATE)):
        frame = _video_frame(index)
        frame.pts = 1500 + int(index * 1000 / VIDEO_RATE)
        for packet in video.encode(frame):
            out.mux(packet)
    _flush(out, video)
    _write_audio(out, audio_stream, _tone(4.0, AUDIO_RATE, 2), 22_050, AUDIO_RATE)
    _flush(out, audio_stream)
    out.close()


def make_color_conflict(path: Path) -> None:
    """Codec-level colour is BT.709 while the stream tag claims BT.2020/HDR."""
    out = av.open(str(path), "w", format="matroska")
    video = _video_stream(out)
    video.codec_context.color_primaries = 1
    video.codec_context.color_trc = 1
    video.codec_context.colorspace = 1
    video.metadata["COLOR_PRIMARIES"] = "bt2020"
    video.metadata["COLOR_TRANSFER"] = "smpte2084"
    video.metadata["COLOR_SPACE"] = "bt709"
    _write_video(out, video, 0, int(2.0 * VIDEO_RATE))
    _flush(out, video)
    out.close()


_CONTAINERS = {b"moov", b"trak", b"mdia", b"minf", b"stbl"}


def _rewrite_boxes(data: bytes, start: int, end: int, drop_last_stss: bool) -> tuple[bytes, bool]:
    """Recursively rebuild a box region, shortening one ``stss`` by an entry.

    Sizes of every enclosing container are recomputed from the rebuilt children,
    which keeps the file structurally valid; byte offsets inside ``moof``/``mdat``
    are untouched because only ``moov`` is rewritten and it sits at the end.
    """
    output = bytearray()
    position = start
    changed = False
    while position + 8 <= end:
        size, kind = struct.unpack(">I4s", data[position:position + 8])
        header = 8
        if size == 1:
            if position + 16 > end:
                return bytes(data[start:end]), False
            size = struct.unpack(">Q", data[position + 8:position + 16])[0]
            header = 16
        elif size == 0:
            size = end - position
        if size < header or position + size > end:
            return bytes(data[start:end]), False
        body_start, body_end = position + header, position + size
        if kind == b"stss" and drop_last_stss and not changed:
            version = struct.unpack(">I", data[body_start:body_start + 4])[0]
            count = struct.unpack(">I", data[body_start + 4:body_start + 8])[0]
            entries = list(struct.unpack(
                f">{count}I", data[body_start + 8:body_start + 8 + 4 * count]
            ))
            if len(entries) < 2:
                output += data[position:position + size]
                position += size
                continue
            payload = struct.pack(">II", version, len(entries) - 1) + b"".join(
                struct.pack(">I", value) for value in entries[:-1]
            )
            body = payload
            changed = True
        elif kind in _CONTAINERS:
            body, sub_changed = _rewrite_boxes(data, body_start, body_end, drop_last_stss)
            if sub_changed:
                changed = True
            else:
                body = bytes(data[body_start:body_end])
        else:
            output += data[position:position + size]
            position += size
            continue
        if header == 8:
            output += struct.pack(">I", 8 + len(body)) + kind + body
        else:
            output += struct.pack(">I", 1) + kind + struct.pack(">Q", 16 + len(body)) + body
        position += size
    return bytes(output), changed


def make_unsupported_color(path: Path) -> None:
    """A colour tag outside the enumeration, with no codec value to override it.

    The codec fields are left unset on purpose: if a codec value were present it
    would legitimately win, and the unsupported *tag* would never be visible.
    """
    out = av.open(str(path), "w", format="matroska")
    video = out.add_stream("libx264", rate=VIDEO_RATE)
    video.width, video.height = VIDEO_SIZE
    video.pix_fmt = "yuv420p"
    video.options = {"g": str(GOP), "keyint_min": str(GOP), "sc_threshold": "0",
                     "preset": "veryfast", "crf": "28"}
    video.metadata["COLOR_PRIMARIES"] = "bt2020-extended-gamut"
    video.metadata["COLOR_TRANSFER"] = "smpte2084"
    _write_video(out, video, 0, int(1.5 * VIDEO_RATE))
    _flush(out, video)
    out.close()


def make_keyframe_hole(src: Path, dst: Path) -> bool:
    """Remove the last sync-sample entry from an MP4 ``stss`` index.

    The media payload stays untouched and fully decodable; only the container's
    keyframe index loses a real keyframe.  That is the "incomplete keyframe
    index" case: a demuxer trusting the bitstream still reports the keyframe, so
    the defect is only visible by comparing both views.
    """
    data = src.read_bytes()
    rewritten, changed = _rewrite_boxes(data, 0, len(data), True)
    if not changed:
        return False
    dst.write_bytes(rewritten)
    return True


def generate(out_dir: Path) -> dict[str, Path]:
    out_dir.mkdir(parents=True, exist_ok=True)
    made: dict[str, Path] = {}

    good_mkv = out_dir / "good.mkv"
    make_good(good_mkv)
    made["good.mkv"] = good_mkv

    good_mp4 = out_dir / "good.mp4"
    make_good(good_mp4, container="mp4")
    made["good.mp4"] = good_mp4

    multi = out_dir / "multi.mkv"
    make_multitrack(multi)
    made["multi.mkv"] = multi

    # MOV is the same box structure as MP4 with a different brand; it exists to
    # prove the container whitelist and colr reader accept both spellings.
    good_mov = out_dir / "good.mov"
    make_good(good_mov, container="mov")
    made["good.mov"] = good_mov

    empty = out_dir / "empty_track.mkv"
    make_empty_track(empty)
    made["empty_track.mkv"] = empty

    truncated = out_dir / "truncated.mkv"
    truncate_file(good_mkv, truncated, 0.45)
    made["truncated.mkv"] = truncated

    damaged_mp4 = out_dir / "damaged.mp4"
    # 3072 is an interior P-frame in the first GOP of the MP4 fixture.
    ok = scramble_packet(good_mp4, damaged_mp4, 0, 3072, keep_head=0, length=64)
    if not ok:
        truncate_file(good_mp4, damaged_mp4, 0.55)
    made["damaged.mp4"] = damaged_mp4

    # The Matroska sample damages every keyframe: defects stay locatable, but no
    # safe boundary exists, so it is deliberately *not* recoverable.  The MP4
    # sample damages one interior frame, which is recoverable from the next
    # keyframe.  Together they cover both repair outcomes.
    damaged_mkv = out_dir / "damaged.mkv"
    if scramble_all_keyframes(good_mkv, damaged_mkv) < 2:
        truncate_file(good_mkv, damaged_mkv, 0.55)
    made["damaged.mkv"] = damaged_mkv

    boundary = out_dir / "boundary.mkv"
    make_boundary(boundary)
    made["boundary.mkv"] = boundary

    conflict = out_dir / "color_conflict.mkv"
    make_color_conflict(conflict)
    made["color_conflict.mkv"] = conflict

    # Colour metadata that is present but outside the enumeration: the archive
    # must keep the raw value and mark it unsupported rather than invent a name.
    unsupported = out_dir / "color_unsupported.mkv"
    make_unsupported_color(unsupported)
    made["color_unsupported.mkv"] = unsupported

    # The sync-sample index lives in MP4, so the incomplete-index fixture is MP4.
    hole = out_dir / "keyframe_index.mp4"
    if not make_keyframe_hole(good_mp4, hole):
        hole.write_bytes(good_mp4.read_bytes())
    made["keyframe_index.mp4"] = hole

    return made


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, help="directory to write fixtures into")
    parser.add_argument("--quiet", action="store_true")
    args = parser.parse_args(argv)
    out_dir = Path(args.out)
    made = generate(out_dir)
    if not args.quiet:
        for name, path in made.items():
            print(f"fixture {name} bytes={path.stat().st_size}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
