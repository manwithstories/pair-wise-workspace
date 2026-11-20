"""Deterministic fixture generator for normal, truncated, corrupt and jittered media.

Every sample is produced from code - no external files - with ``bitexact``
muxing and a fixed PRNG seed, so two runs on the same machine emit
byte-identical files.  Each fixture records what was injected in a JSON
sidecar (``<name>.json``) so tests can assert on ground truth instead of
guessing:

``normal.mov``
    Clean H.264 + AAC MOV, 30 fps, 12 s (~720 video packets).
``truncated.mov``
    Faststart MOV with the tail of ``mdat`` cut: ``moov`` survives, so the
    readable prefix is repairable.
``broken_index.mov``
    MOV with the trailing ``moov`` deleted: structurally unrecoverable.
``corrupt.mov``
    ``normal.mov`` with one video packet and one later AAC packet zeroed in
    place.  The sidecar names their global packet indices; the pipeline's
    ``first_error_packet`` must match within one packet.
``jitter.mp4``
    Rewritten timestamps: a +1 s clock jump, a duplicated dts and a
    backwards dts (no container damage).
``zero_packets.mp4``
    An initialisation-only fragmented MP4 with streams but no samples.
``slow_consumer.mov``
    ``normal.mov`` with more frames, used with ``--consumer-delay`` to force
    the producer to block on a full queue.
``unsupported_codec.mov``
    Valid MOV encoded as MPEG-4 part 2; must be rejected with exit code 2.

The corrupt fixture is long enough that ``--interrupt-after 512`` (the
acceptance demo) really stops mid-stream, leaving committed chunks behind.
"""

from __future__ import annotations

import json
import os
import random
import shutil
import struct
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path

import av
import numpy as np

from .errors import StructuralError
from .mp4write import MUX_OPTIONS, find_box, iter_boxes, iter_children

SAMPLE_RATE = 48_000
VIDEO_RATE = 30
WIDTH = 320
HEIGHT = 240


@dataclass
class SampleSpec:
    name: str
    path: Path
    kind: str
    metadata: dict

    def as_dict(self) -> dict:
        return {"name": self.name, "path": str(self.path), "kind": self.kind,
                "metadata": self.metadata}


def _tone(seconds: float, frequency: float = 440.0) -> np.ndarray:
    samples = int(SAMPLE_RATE * seconds)
    t = np.arange(samples, dtype=np.float64) / SAMPLE_RATE
    wave = 0.18 * np.sin(2 * np.pi * frequency * t)
    return wave.astype(np.float32)


def _encode_mov(path: Path, seconds: float, *, gop: int = 15,
                faststart: bool = False) -> None:
    """Encode a deterministic H.264 + AAC MOV."""
    path.parent.mkdir(parents=True, exist_ok=True)
    options = {"fflags": "+bitexact"}
    if faststart:
        options["movflags"] = "+faststart"
    container = av.open(str(path), "w", format="mov", options=options)
    video = container.add_stream("libx264", rate=VIDEO_RATE)
    video.width = WIDTH
    video.height = HEIGHT
    video.pix_fmt = "yuv420p"
    video.options = {
        "preset": "ultrafast", "tune": "zerolatency", "bf": "0",
        "g": str(gop), "crf": "32", "sc_threshold": "0",
    }
    audio = container.add_stream("aac", rate=SAMPLE_RATE)
    audio.layout = "stereo"
    audio.bit_rate = 96_000

    frames = int(seconds * VIDEO_RATE)
    for index in range(frames):
        luma = (index * 7) % 256
        plane = np.full((HEIGHT, WIDTH, 3), luma, dtype=np.uint8)
        plane[:, :, 1] = (index * 3) % 256
        frame = av.VideoFrame.from_ndarray(plane, format="rgb24")
        for packet in video.encode(frame):
            container.mux(packet)
    for packet in video.encode():
        container.mux(packet)

    wave = _tone(seconds)
    stereo = np.stack([wave, wave], axis=1)
    for start in range(0, len(stereo) - 1024 + 1, 1024):
        block = np.ascontiguousarray(stereo[start:start + 1024].T)
        frame = av.AudioFrame.from_ndarray(block, format="fltp", layout="stereo")
        frame.sample_rate = SAMPLE_RATE
        frame.pts = start
        frame.time_base = Fraction(1, SAMPLE_RATE)
        for packet in audio.encode(frame):
            container.mux(packet)
    for packet in audio.encode():
        container.mux(packet)
    container.close()


def _packet_map(path: Path) -> list[tuple[int, int, int, int]]:
    """``(global_index, stream_index, pos, size)`` for every demuxed packet."""
    container = av.open(str(path))
    result = []
    index = 0
    try:
        for packet in container.demux():
            if packet.dts is None and packet.pts is None and packet.size == 0:
                continue
            result.append((index, packet.stream.index, packet.pos, packet.size))
            index += 1
    finally:
        container.close()
    return result


def _zero_region(path: Path, pos: int, size: int) -> None:
    with open(path, "r+b") as handle:
        handle.seek(pos)
        handle.write(b"\x00" * size)
        handle.flush()
        os.fsync(handle.fileno())


def _make_normal(target: Path, seconds: float = 12.0) -> SampleSpec:
    _encode_mov(target, seconds)
    packets = _packet_map(target)
    return SampleSpec("normal", target, "normal", {
        "packet_count": len(packets),
        "video_packets": sum(1 for p in packets if p[1] == 0),
        "audio_packets": sum(1 for p in packets if p[1] == 1),
        "duration_seconds": seconds,
    })


def _make_truncated(normal: Path, target: Path) -> SampleSpec:
    """Faststart MOV with the tail of ``mdat`` cut off.

    ``moov`` sits at the front, so the demuxer can still describe streams and
    read the surviving packets; only the tail samples are lost.  Repair must
    keep the readable prefix, record the truncation and exit 0.
    """
    fast = target.with_name(target.stem + ".seed.mov")
    _encode_mov(fast, 8.0, faststart=True)
    data = fast.read_bytes()
    fast.unlink(missing_ok=True)
    cut = int(len(data) * 0.8)
    target.write_bytes(data[:cut])
    return SampleSpec("truncated", target, "truncated", {
        "original_bytes": len(data),
        "bytes": cut,
        "recoverable": True,
    })


def _make_broken_index(normal: Path, target: Path) -> SampleSpec:
    """MOV whose trailing ``moov`` is missing entirely.

    There is no way to recover streams or an index, so this fixture must be
    rejected as a structural error (exit code 2) rather than repaired.
    """
    data = normal.read_bytes()
    with open(normal, "rb") as handle:
        head = handle.read(1024 * 1024)
    moov = head.rfind(b"moov")
    if moov < 4:
        target.write_bytes(data[: max(1, len(data) // 2)])
    else:
        target.write_bytes(data[:moov - 4])
    return SampleSpec("broken_index", target, "broken_index", {
        "bytes": target.stat().st_size,
        "recoverable": False,
    })


def _make_corrupt(normal: Path, target: Path) -> SampleSpec:
    shutil.copyfile(normal, target)
    packets = _packet_map(target)
    video = [p for p in packets if p[1] == 0]
    audio = [p for p in packets if p[1] == 1]
    # Choose packets well inside the stream so priming packets stay intact.
    video_target = video[len(video) // 2]
    audio_target = audio[len(audio) * 3 // 4]
    _zero_region(target, video_target[2], video_target[3])
    _zero_region(target, audio_target[2], audio_target[3])
    return SampleSpec("corrupt", target, "corrupt", {
        "first_error_packet": video_target[0],
        "video_packet": video_target[0],
        "audio_packet": audio_target[0],
        "injections": [
            {"global_packet": video_target[0], "stream": video_target[1],
             "pos": video_target[2], "size": video_target[3], "filler": "zeros"},
            {"global_packet": audio_target[0], "stream": audio_target[1],
             "pos": audio_target[2], "size": audio_target[3], "filler": "zeros"},
        ],
    })


def _fragmented_copy(normal: Path, target: Path) -> None:
    """Remux ``normal`` into fragmented MP4 with one ``moof`` per keyframe."""
    source = av.open(str(normal))
    output = av.open(str(target), "w", format="mp4", options=dict(MUX_OPTIONS))
    try:
        for stream in source.streams:
            output.add_stream(template=stream)
        for packet in source.demux():
            if packet.dts is None:
                continue
            packet.stream = output.streams[packet.stream.index]
            output.mux(packet)
    finally:
        source.close()
        output.close()


def _video_moofs(data: bytes, video_track_id: int) -> list[tuple[int, int, int, int]]:
    """Return ``(moof_offset, tfdt_offset, version, timescale)`` per video traf."""
    timescale = 90_000
    moov = find_box(data, ("moov",))
    if moov:
        for box_type, box_offset, box_size in iter_children(data, moov[0], moov[1]):
            if box_type != "trak":
                continue
            track_id = None
            for child_type, child_offset, child_size in iter_children(data, box_offset, box_size):
                if child_type == "tkhd":
                    version = data[child_offset + 8]
                    base = child_offset + 12
                    track_id = struct.unpack(
                        ">I", data[base + (16 if version == 1 else 8):base + (20 if version == 1 else 12)]
                    )[0]
                elif child_type == "mdia":
                    for media_type, media_offset, media_size in iter_children(data, child_offset, child_size):
                        if media_type == "mdhd":
                            version = data[media_offset + 8]
                            base = media_offset + 12
                            timescale = struct.unpack(
                                ">I", data[base + (16 if version == 1 else 8):base + (20 if version == 1 else 12)]
                            )[0]
            if track_id == video_track_id:
                break
    results = []
    for box_type, moof_offset, moof_size in iter_boxes(data):
        if box_type != "moof":
            continue
        for traf_type, traf_offset, traf_size in iter_children(data, moof_offset, moof_size):
            if traf_type != "traf":
                continue
            track_id = None
            tfdt = None
            for child_type, child_offset, child_size in iter_children(data, traf_offset, traf_size):
                if child_type == "tfhd":
                    track_id = struct.unpack(">I", data[child_offset + 12:child_offset + 16])[0]
                elif child_type == "tfdt":
                    tfdt = (child_offset, data[child_offset + 8])
            if track_id == video_track_id and tfdt is not None:
                results.append((moof_offset, tfdt[0], tfdt[1], timescale))
    return results


def _patch_tfdt(data: bytearray, offset: int, version: int, value: int) -> None:
    base = offset + 12
    if version == 1:
        struct.pack_into(">Q", data, base, value & 0xFFFFFFFFFFFFFFFF)
    else:
        struct.pack_into(">I", data, base, value & 0xFFFFFFFF)


def _make_jitter(normal: Path, target: Path) -> SampleSpec:
    """Introduce clock anomalies by rewriting fragment decode times.

    Encoding timestamps directly would make the source muxer reject
    non-monotonic dts, so the fixture is built in two passes: a clean
    fragmented copy, then surgical ``tfdt`` edits.  The result is a valid
    container whose timestamps jump, repeat and step backwards - exactly the
    damage repair has to unify.
    """
    _fragmented_copy(normal, target)
    data = bytearray(target.read_bytes())
    moofs = _video_moofs(bytes(data), 1)
    if len(moofs) < 4:
        raise StructuralError("jitter fixture needs at least four video fragments")
    def read(offset: int, version: int) -> int:
        end = offset + (20 if version == 1 else 16)
        return struct.unpack(">Q" if version == 1 else ">I", bytes(data[offset + 12:end]))[0]

    timescale = moofs[0][3]
    # Fragment 1: +12 s forward clock jump, far beyond the 5 s tolerance.
    _, offset, version, _ = moofs[1]
    _patch_tfdt(data, offset, version, read(offset, version) + 12 * timescale)
    # Fragment 2: -8 s backwards clock jump.
    _, offset, version, _ = moofs[2]
    _patch_tfdt(data, offset, version, read(offset, version) - 8 * timescale)
    # Fragment 3: repeat fragment 2's decode time - a block of duplicates.
    _, prev_offset, version, _ = moofs[2]
    _, dup_offset, version, _ = moofs[3]
    _patch_tfdt(data, dup_offset, version, read(prev_offset, version))
    target.write_bytes(bytes(data))
    return SampleSpec("jitter", target, "jitter", {
        "forward_jump_seconds": 12.0,
        "backwards_jump_seconds": 8.0,
        "jump_fragment": 1,
        "backwards_fragment": 2,
        "duplicate_fragment": 3,
        "video_fragments": len(moofs),
    })


def _make_zero_packets(normal: Path, target: Path) -> SampleSpec:
    """Initialisation segment only: streams described, no samples at all."""
    temp = target.with_name(target.stem + ".seed.mp4")
    _fragmented_copy(normal, temp)
    data = temp.read_bytes()
    temp.unlink(missing_ok=True)
    parts = []
    for box_type, offset, size in iter_boxes(data):
        if box_type in ("ftyp", "moov"):
            parts.append(data[offset:offset + size])
        elif parts:
            break
    payload = b"".join(parts)
    if not payload:
        raise StructuralError("could not build a zero-packet fixture")
    target.write_bytes(payload)
    return SampleSpec("zero_packets", target, "zero_packets", {"bytes": len(payload)})


def _make_unsupported(normal: Path, target: Path) -> SampleSpec:
    """Short MOV encoded with MPEG-4 part 2, which the pipeline refuses.

    Proves the codec check is real: the container is valid, the streams are
    readable, but the tool must reject the codec instead of copying bytes it
    cannot validate.
    """
    path = target
    path.parent.mkdir(parents=True, exist_ok=True)
    container = av.open(str(path), "w", format="mov", options={"fflags": "+bitexact"})
    try:
        video = container.add_stream("mpeg4", rate=VIDEO_RATE)
        video.width = WIDTH
        video.height = HEIGHT
        video.pix_fmt = "yuv420p"
        video.bit_rate = 400_000
        for index in range(VIDEO_RATE):
            plane = np.full((HEIGHT, WIDTH, 3), (index * 5) % 256, dtype=np.uint8)
            frame = av.VideoFrame.from_ndarray(plane, format="rgb24")
            for packet in video.encode(frame):
                container.mux(packet)
        for packet in video.encode():
            container.mux(packet)
    finally:
        container.close()
    return SampleSpec("unsupported_codec", path, "unsupported_codec", {
        "codec": "mpeg4",
        "recoverable": False,
    })


def _make_slow(normal: Path, target: Path, seconds: float = 20.0) -> SampleSpec:
    _encode_mov(target, seconds)
    packets = _packet_map(target)
    return SampleSpec("slow_consumer", target, "slow_consumer", {
        "packet_count": len(packets),
        "duration_seconds": seconds,
    })


def generate(out_dir: Path) -> list[SampleSpec]:
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    random.seed(1337)

    specs: list[SampleSpec] = []
    normal_path = out_dir / "normal.mov"
    specs.append(_make_normal(normal_path))
    specs.append(_make_truncated(normal_path, out_dir / "truncated.mov"))
    specs.append(_make_broken_index(normal_path, out_dir / "broken_index.mov"))
    specs.append(_make_corrupt(normal_path, out_dir / "corrupt.mov"))
    specs.append(_make_jitter(normal_path, out_dir / "jitter.mp4"))
    specs.append(_make_zero_packets(normal_path, out_dir / "zero_packets.mp4"))
    specs.append(_make_slow(normal_path, out_dir / "slow_consumer.mov"))
    specs.append(_make_unsupported(normal_path, out_dir / "unsupported_codec.mov"))

    for spec in specs:
        sidecar = spec.path.with_suffix(spec.path.suffix + ".json")
        sidecar.write_text(json.dumps(spec.as_dict(), indent=2, sort_keys=True), encoding="utf-8")
    return specs
