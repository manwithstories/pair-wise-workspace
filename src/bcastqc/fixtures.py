"""Synthetic delivery assets used by the demo and the test-suite.

Everything is generated locally from numpy/soundfile/PyAV; no external media
is referenced.  Generation is deterministic (``fflags=+bitexact`` and fixed
content), so a rerun over the same work directory produces byte-identical
masters and the resume cache can be exercised honestly.

The master carries an A/V sync probe: a 2.5 kHz tone burst for 60 ms and a
4-frame white flash at the *same* instants (1 s, 4 s, 7 s).  Measuring both in
the output gives an end-to-end sync error that is independent of container
timestamps -- it catches real A/V drift, not just bookkeeping.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import av
import numpy as np
import soundfile as sf

from . import container

RATE = 48000
FPS = 25
WIDTH = 320
HEIGHT = 180
DURATION = 10.0
MARKERS = (1.0, 4.0, 7.0)
FLASH_FRAMES = 4
BURST_SECONDS = 0.060


@dataclass(frozen=True, slots=True)
class FixtureSet:
    normal: Path
    truncated: Path
    corrupt: Path
    incomplete_index: Path
    refused: Path

    def as_dict(self) -> dict[str, str]:
        return {
            "normal": str(self.normal),
            "truncated": str(self.truncated),
            "corrupt": str(self.corrupt),
            "incomplete_index": str(self.incomplete_index),
            "refused": str(self.refused),
        }

    def damaged(self) -> list[Path]:
        return [self.truncated, self.corrupt, self.incomplete_index]


def write_master_wav(path: Path) -> Path:
    count = int(RATE * DURATION)
    time = np.arange(count) / RATE
    signal = 0.02 * np.sin(2 * np.pi * 440 * time)
    burst_len = int(BURST_SECONDS * RATE)
    window = 0.5 - 0.5 * np.cos(2 * np.pi * np.arange(burst_len) / burst_len)
    for marker in MARKERS:
        start = int(round((marker - BURST_SECONDS / 2) * RATE))
        burst = 0.6 * np.sin(2 * np.pi * 2500 * np.arange(burst_len) / RATE) * window
        signal[start : start + burst_len] += burst
    stereo = np.stack([signal, signal], axis=1).astype(np.float32)
    path.parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(path), stereo, RATE, subtype="FLOAT")
    return path


def _flash_frame(index: int) -> bool:
    for marker in MARKERS:
        first = int(round(marker * FPS)) - FLASH_FRAMES // 2
        if first <= index < first + FLASH_FRAMES:
            return True
    return False


def encode_master(path: Path, wav: Path) -> Path:
    """Encode the deterministic H.264/AAC master with a leading moov."""
    audio, rate = sf.read(str(wav), dtype="float32")
    path.parent.mkdir(parents=True, exist_ok=True)
    with av.open(
        str(path),
        mode="w",
        format="mp4",
        options={"movflags": "+faststart", "fflags": "+bitexact"},
    ) as out:
        video = out.add_stream("h264", rate=FPS)
        video.width = WIDTH
        video.height = HEIGHT
        video.pix_fmt = "yuv420p"
        video.options = {"preset": "ultrafast", "crf": "23", "bf": "0", "g": str(FPS), "flags": "+bitexact"}
        audio_stream = out.add_stream("aac", rate=rate)
        audio_stream.layout = "stereo"
        audio_stream.bit_rate = 96000
        audio_stream.options = {"flags": "+bitexact"}

        for index in range(int(DURATION * FPS)):
            image = np.full((HEIGHT, WIDTH, 3), 30, dtype=np.uint8)
            if _flash_frame(index):
                image[:] = 235
            frame = av.VideoFrame.from_ndarray(image, format="rgb24")
            frame.pts = index
            for packet in video.encode(frame):
                out.mux(packet)
        for packet in video.encode():
            out.mux(packet)

        block = 1024
        for offset in range(0, len(audio), block):
            segment = audio[offset : offset + block]
            if len(segment) < block:
                segment = np.pad(segment, ((0, block - len(segment)), (0, 0)))
            frame = av.AudioFrame.from_ndarray(
                segment.T.copy(), format="fltp", layout="stereo"
            )
            frame.sample_rate = rate
            frame.pts = offset
            for packet in audio_stream.encode(frame):
                out.mux(packet)
        for packet in audio_stream.encode():
            out.mux(packet)
    return path


def make_truncated(source: Path, destination: Path, keep: float = 0.85) -> Path:
    """Cut the mdat tail; the index keeps describing the missing samples."""
    data = source.read_bytes()
    report = container.inspect_index(data)
    if report.mdat_end is None or report.mdat_start is None:
        raise RuntimeError("source has no mdat")
    cut = report.mdat_start + int((report.mdat_end - report.mdat_start) * keep)
    destination.write_bytes(data[:cut])
    return destination


def _packet_ranges(source: Path) -> list[tuple[int, int]]:
    """Byte ranges of the compressed video packets, via the index."""
    data = source.read_bytes()
    report = container.inspect_index(data)
    ranges: list[tuple[int, int]] = []
    for track in report.tracks:
        if track.handler == "vide":
            ranges.extend(track.sample_ranges())
    return ranges


def make_corrupt(source: Path, destination: Path, count: int = 3) -> Path:
    """Zero the length field of several video packets, in place of their body.

    The container stays intact; only the compressed payload is damaged, which
    is the failure a supplier delivers when a transfer glitches mid-file.
    """
    data = bytearray(source.read_bytes())
    ranges = _packet_ranges(source)
    if len(ranges) < count + 2:
        raise RuntimeError("source has too few video packets")
    step = len(ranges) // (count + 1)
    for i in range(1, count + 1):
        offset, size = ranges[i * step]
        start = offset + 4
        end = min(offset + size, start + 40)
        data[start:end] = b"\x00" * (end - start)
        # Break the length prefix too, so the framing cannot be mistaken for valid.
        data[offset : offset + 4] = (0).to_bytes(4, "big")
    destination.write_bytes(bytes(data))
    return destination


def make_incomplete_index(
    source: Path, destination: Path, keep: float = 0.85
) -> Path:
    """Shorten every sample table consistently, but leave mdat and durations.

    The result is a file whose index *under-reports* the media it carries: the
    tables agree with each other (so a strict parser can read the file), the
    declared duration still says 10 s, and the mdat tail is unreferenced.
    This is the "incomplete index" failure mode, and it must be repaired by
    rebuilding tables from the retained samples -- not by trusting the index's
    silence as "end of file".
    """
    data = source.read_bytes()
    moov = container.find_path(data, "moov")
    if moov is None:
        raise RuntimeError("source has no moov")
    overrides: dict[int, dict[int, bytes]] = {}

    for trak in container.parse_boxes(data, moov.body_start, moov.body_end):
        if trak.type != "trak":
            continue
        stbl = container._read_stbl(data, trak)
        if not stbl:
            continue
        tables = _trim_tables(data, stbl, keep)
        if tables:
            overrides[trak.pos] = tables

    new_moov_body = _rewrite_children(data, moov, overrides)
    new_moov = container._pack("moov", new_moov_body)
    if len(new_moov) > moov.size:
        raise RuntimeError("shortened index grew; cannot keep chunk offsets stable")
    pad = moov.size - len(new_moov)
    if pad >= 8:
        new_moov = container._pack("moov", new_moov_body + container._pack("free", b"\x00" * (pad - 8)))
    elif pad:
        new_moov = container._pack("moov", new_moov_body + b"\x00" * pad)
        new_moov = len(new_moov).to_bytes(4, "big") + b"moov" + new_moov[8:]
    destination.write_bytes(data[: moov.pos] + new_moov + data[moov.body_end :])
    return destination


def _chunk_sample_counts(data: bytes, stbl: dict[str, container.Box], chunk_count: int) -> list[int]:
    stsc = stbl.get("stsc")
    if stsc is None:
        return [1] * chunk_count
    body = container.payload(data, stsc)
    entries = [
        (container._u32(body, 8 + 12 * i), container._u32(body, 12 + 12 * i))
        for i in range(container._u32(body, 4))
    ]
    per_chunk: list[int] = []
    for chunk in range(1, chunk_count + 1):
        samples = 0
        for first, count in entries:
            if first <= chunk:
                samples = count
        per_chunk.append(samples)
    return per_chunk


def _trim_tables(
    data: bytes, stbl: dict[str, container.Box], keep_ratio: float
) -> dict[int, bytes]:
    stco = stbl.get("stco")
    stsz = stbl.get("stsz")
    stts = stbl.get("stts")
    stsc = stbl.get("stsc")
    if stco is None or stsz is None or stts is None or stsc is None:
        return {}

    size_body = container.payload(data, stsz)
    uniform = container._u32(size_body, 4)
    sample_count = container._u32(size_body, 8)
    sizes = (
        [uniform] * sample_count
        if uniform
        else [container._u32(size_body, 12 + 4 * i) for i in range(sample_count)]
    )

    chunk_body = container.payload(data, stco)
    chunk_count = container._u32(chunk_body, 4)
    offsets = [container._u32(chunk_body, 8 + 4 * i) for i in range(chunk_count)]
    per_chunk = _chunk_sample_counts(data, stbl, chunk_count)

    target = int(sample_count * keep_ratio)
    kept_samples = 0
    kept_chunks = 0
    for count in per_chunk:
        if kept_samples + count > target:
            break
        kept_samples += count
        kept_chunks += 1
    if kept_chunks == 0:
        return {}

    # stsc: keep only entries whose first chunk is retained.
    entry_body = container.payload(data, stsc)
    entries = [
        (
            container._u32(entry_body, 8 + 12 * i),
            container._u32(entry_body, 12 + 12 * i),
            container._u32(entry_body, 16 + 12 * i),
        )
        for i in range(container._u32(entry_body, 4))
    ]
    kept_entries = [entry for entry in entries if entry[0] <= kept_chunks]
    while len(kept_entries) > 1 and kept_entries[-1][1] == kept_entries[-2][1]:
        kept_entries.pop()

    # stts: split the run-length table exactly at the retained sample count.
    stts_body = container.payload(data, stts)
    remaining = kept_samples
    runs: list[tuple[int, int]] = []
    for i in range(container._u32(stts_body, 4)):
        count = container._u32(stts_body, 8 + 8 * i)
        delta = container._u32(stts_body, 12 + 8 * i)
        if remaining <= 0:
            break
        take = min(count, remaining)
        runs.append((take, delta))
        remaining -= take

    payloads: dict[int, bytes] = {}
    payloads[stsz.pos] = (
        size_body[:8]
        + kept_samples.to_bytes(4, "big")
        + (b"" if uniform else b"".join(x.to_bytes(4, "big") for x in sizes[:kept_samples]))
    )
    payloads[stco.pos] = (
        chunk_body[:4]
        + kept_chunks.to_bytes(4, "big")
        + b"".join(x.to_bytes(4, "big") for x in offsets[:kept_chunks])
    )
    payloads[stsc.pos] = (
        entry_body[:4]
        + len(kept_entries).to_bytes(4, "big")
        + b"".join(
            first.to_bytes(4, "big") + count.to_bytes(4, "big") + desc.to_bytes(4, "big")
            for first, count, desc in kept_entries
        )
    )
    payloads[stts.pos] = (
        stts_body[:4]
        + len(runs).to_bytes(4, "big")
        + b"".join(count.to_bytes(4, "big") + delta.to_bytes(4, "big") for count, delta in runs)
    )

    stss = stbl.get("stss")
    if stss is not None:
        sync_body = container.payload(data, stss)
        sync = [
            container._u32(sync_body, 8 + 4 * i)
            for i in range(container._u32(sync_body, 4))
        ]
        kept_sync = [number for number in sync if number <= kept_samples]
        payloads[stss.pos] = (
            sync_body[:4]
            + len(kept_sync).to_bytes(4, "big")
            + b"".join(number.to_bytes(4, "big") for number in kept_sync)
        )
    return payloads


def _rewrite_children(
    data: bytes, box: container.Box, overrides: dict[int, dict[int, bytes]]
) -> bytes:
    parts: list[bytes] = []
    for child in container.parse_boxes(data, box.body_start, box.body_end):
        if child.type in container.CONTAINER_BOXES:
            nested = overrides.get(child.pos)
            if nested is None:
                nested = overrides.get(box.pos, {})
            parts.append(
                container._pack(child.type, _rewrite_children(data, child, {**overrides, child.pos: nested}))
            )
        elif child.type in ("stbl",):
            parts.append(container._pack(child.type, _rewrite_children(data, child, overrides)))
        else:
            replacement = overrides.get(box.pos, {}).get(child.pos)
            if replacement is not None:
                parts.append(container._pack(child.type, replacement))
            else:
                parts.append(data[child.pos : child.body_end])
    return b"".join(parts)


def make_refused(destination: Path) -> Path:
    """An MP4 carrying VP9/Opus: readable, but outside the copy allow-list."""
    destination.parent.mkdir(parents=True, exist_ok=True)
    with av.open(str(destination), mode="w", format="mp4") as out:
        video = out.add_stream("libvpx-vp9", rate=FPS)
        video.width = WIDTH
        video.height = HEIGHT
        video.pix_fmt = "yuv420p"
        video.options = {"crf": "40", "b:v": "0", "deadline": "realtime", "cpu-used": "8"}
        audio = out.add_stream("libopus", rate=RATE)
        audio.layout = "stereo"
        for index in range(int(2 * FPS)):
            frame = av.VideoFrame.from_ndarray(
                np.zeros((HEIGHT, WIDTH, 3), dtype=np.uint8), format="rgb24"
            )
            frame.pts = index
            for packet in video.encode(frame):
                out.mux(packet)
        for packet in video.encode():
            out.mux(packet)
        for offset in range(0, RATE, 960):
            samples = np.zeros((1, 960 * 2), dtype=np.int16)
            frame = av.AudioFrame.from_ndarray(samples, format="s16", layout="stereo")
            frame.sample_rate = RATE
            frame.pts = offset
            for packet in audio.encode(frame):
                out.mux(packet)
        for packet in audio.encode():
            out.mux(packet)
    return destination


def strip_audio(source: Path, destination: Path) -> Path:
    """Copy only the video track -- an asset missing a required audio track."""
    destination.parent.mkdir(parents=True, exist_ok=True)
    with av.open(str(source), mode="r", metadata_errors="ignore") as inp:
        video = next((s for s in inp.streams if s.type == "video"), None)
        if video is None:
            raise RuntimeError("source has no video stream")
        with av.open(str(destination), mode="w", format="mp4", options={"movflags": "+faststart"}) as out:
            template = out.add_stream(template=video)
            for packet in inp.demux(video):
                if packet.size == 0:
                    continue
                packet.stream = template
                out.mux(packet)
    return destination


def build_fixtures(workdir: Path) -> FixtureSet:
    """Create the demo assets under ``workdir/inputs``."""
    inputs = workdir / "inputs"
    inputs.mkdir(parents=True, exist_ok=True)
    wav = write_master_wav(workdir / "master.wav")
    normal = encode_master(inputs / "normal_master.mp4", wav)
    truncated = make_truncated(normal, inputs / "truncated_tail.mp4")
    corrupt = make_corrupt(normal, inputs / "corrupt_packets.mp4")
    incomplete = make_incomplete_index(normal, inputs / "incomplete_index.mp4")
    refused = make_refused(inputs / "refused_vp9_opus.mp4")
    return FixtureSet(
        normal=normal,
        truncated=truncated,
        corrupt=corrupt,
        incomplete_index=incomplete,
        refused=refused,
    )
