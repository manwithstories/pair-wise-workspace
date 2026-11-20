#!/usr/bin/env python
"""Generate deterministic edge captures used for demos and tests.

Creates three fixture families under ``--out``:

``good``     clean stereo 48 kHz, 3 RTP packets per 1 s chunk, RTCP mapping
``damaged``  duplicates, reordering, burst loss, sequence wrap, a truncated
             packet, timestamp wrap, clock drift and a truncated record
``boundary`` ``empty`` (no packets) and ``wrap_short`` (timestamp wrap plus a
             final chunk shorter than one chunk)
"""

from __future__ import annotations

import argparse
import json
import math
import struct
import sys
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from region_media import packet  # noqa: E402
from region_media.metadata import VIDEO_COLOUR_KEYS  # noqa: E402

# 5.1 / 7.1 channel order produced by PyAV/FFmpeg and expected by our matrix.
CHANNEL_ORDER = {
    "mono": ["FC"],
    "stereo": ["FL", "FR"],
    "5.1": ["FL", "FR", "FC", "LFE", "BL", "BR"],
    "7.1": ["FL", "FR", "FC", "LFE", "BL", "BR", "SL", "SR"],
}


def make_tone(
    rate: int, layout: str, seconds: float, *, silence: bool = False
) -> np.ndarray:
    """Deterministic multi-channel material with a per-channel tone."""
    channels = len(CHANNEL_ORDER[layout])
    n = int(round(rate * seconds))
    t = np.arange(n, dtype=np.float64) / rate
    out = np.zeros((n, channels), dtype=np.float32)
    if silence:
        return out
    base = {"mono": 440.0, "stereo": 440.0, "5.1": 220.0, "7.1": 180.0}[layout]
    for c in range(channels):
        # Distinct, low-level tones so channel mapping mistakes are audible
        # and measurable without risk of clipping.
        freq = base * (1.0 + 0.25 * c)
        env = 1.0 + 0.2 * np.sin(2 * math.pi * 0.5 * t + c * 0.3)
        out[:, c] = (0.08 * np.sin(2 * math.pi * freq * t) * env).astype(
            np.float32
        )
    return out


def rtp_packet(
    samples: np.ndarray,
    *,
    sequence: int,
    timestamp: int,
    ssrc: int,
    payload_type: int = 96,
    marker: bool = False,
) -> bytes:
    header = struct.pack(
        "!BBHII",
        0x80,
        (0x80 if marker else 0x00) | (payload_type & 0x7F),
        sequence & 0xFFFF,
        timestamp & 0xFFFFFFFF,
        ssrc & 0xFFFFFFFF,
    )
    return header + packet.encode_audio_payload(samples)


def make_sender_report(
    *, ssrc: int, rtp_timestamp: int, unix_time: float, packet_count: int
) -> bytes:
    ntp = unix_time + 2208988800.0
    ntp_sec = int(ntp)
    ntp_frac = int((ntp - ntp_sec) * (1 << 32))
    body = struct.pack(
        "!IIIIII", ssrc, ntp_sec, ntp_frac, rtp_timestamp & 0xFFFFFFFF,
        packet_count, 0
    )
    length_words = (len(body) + 4) // 4 - 1
    return struct.pack("!BBH", 0x80, 200, length_words) + body


def video_record(
    *, pts_samples: int, duration_samples: int, metadata: dict[str, str]
) -> bytes:
    return packet.encode_video_record(pts_samples, duration_samples, metadata)


def write_capture(
    directory: Path,
    spec: packet.StreamSpec,
    records: list[tuple[int, bytes, int]],
    *,
    truncate_at: int | None = None,
) -> None:
    """Write ``stream.json`` and a capture built from ``(kind, payload, flags)``.

    ``truncate_at`` models an interrupted disk flush: the record at that index
    is written with only a third of its payload, leaving its declared length
    and CRC intact.  The following records are written normally so the reader
    has to resynchronise on the next magic marker.
    """
    directory.mkdir(parents=True, exist_ok=True)
    (directory / "stream.json").write_text(
        json.dumps(spec.to_json(), indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    blob = bytearray()
    for position, (kind, payload, flags) in enumerate(records):
        if truncate_at is not None and position == truncate_at:
            blob += packet.write_truncated_record(
                kind, payload, max(1, len(payload) // 3), flags=flags
            )
            continue
        blob += packet.write_record(kind, payload, flags=flags)
    (directory / "capture.edg").write_bytes(bytes(blob))


def build_good(out: Path) -> None:
    rate, layout = 48000, "stereo"
    spec = packet.StreamSpec(
        sample_rate=rate, channels=2, layout=layout, clock_rate=rate,
        ssrc=0x11111111, nominal_samples=480,
    )
    audio = make_tone(rate, layout, 3.0)
    per_packet = 480  # 10 ms
    records: list[tuple[int, bytes, int]] = []
    ts0 = 0xFFFFF000  # start close to the 32-bit wrap to exercise it
    for i in range(audio.shape[0] // per_packet):
        block = audio[i * per_packet : (i + 1) * per_packet]
        records.append(
            (
                packet.KIND_RTP,
                rtp_packet(
                    block,
                    sequence=1000 + i,
                    timestamp=ts0 + i * per_packet,
                    ssrc=spec.ssrc,
                ),
                0,
            )
        )
    # RTCP sender report mapping RTP ts to wall clock, then a second one.
    records.insert(
        30,
        (
            packet.KIND_RTCP,
            make_sender_report(
                ssrc=spec.ssrc, rtp_timestamp=ts0, unix_time=1_700_000_000.0,
                packet_count=30,
            ),
            0,
        ),
    )
    records.append(
        (
            packet.KIND_VIDEO,
            video_record(
                pts_samples=0,
                duration_samples=rate,
                metadata={
                    "color_space": "bt709",
                    "color_primaries": "bt709",
                    "color_transfer": "bt709",
                    "color_range": "tv",
                },
            ),
            0,
        )
    )
    records.append(
        (
            packet.KIND_VIDEO,
            video_record(
                pts_samples=rate,
                duration_samples=rate,
                metadata={
                    "color_space": "bt709",
                    "color_primaries": "bt709",
                    "color_transfer": "bt709",
                    "color_range": "tv",
                },
            ),
            0,
        )
    )
    write_capture(out / "good", spec, records)


def build_damaged(out: Path) -> None:
    rate, layout = 48000, "stereo"
    spec = packet.StreamSpec(
        sample_rate=rate, channels=2, layout=layout, clock_rate=rate,
        ssrc=0x22222222, nominal_samples=480,
    )
    audio = make_tone(rate, layout, 4.0)
    per_packet = 480
    total = audio.shape[0] // per_packet
    order = list(range(total))
    # reorder a handful of neighbours
    for a, b in [(20, 21), (21, 20), (60, 62), (61, 60), (62, 61)]:
        if a < len(order) and b < len(order):
            order[a], order[b] = order[b], order[a]
    burst = {40, 41, 42, 43, 44}  # contiguous loss burst
    records: list[tuple[int, bytes, int]] = []
    ts0 = 0xFFFFFF00
    seq0 = 0xFFF0  # 16-bit sequence wrap shortly after the start
    emitted = 0
    for i in order:
        if i in burst:
            continue
        block = audio[i * per_packet : (i + 1) * per_packet]
        sequence = (seq0 + i) & 0xFFFF
        timestamp = (ts0 + i * per_packet) & 0xFFFFFFFF
        flags = 0
        payload = rtp_packet(
            block, sequence=sequence, timestamp=timestamp, ssrc=spec.ssrc
        )
        if i in (10, 11):
            # duplicate packet (same payload written twice)
            records.append((packet.KIND_RTP, payload, flags))
        records.append((packet.KIND_RTP, payload, flags))
        if i == 70:
            records.append(
                (
                    packet.KIND_RTP,
                    rtp_packet(
                        block,
                        sequence=sequence,
                        timestamp=timestamp,
                        ssrc=spec.ssrc,
                    ),
                    0x01,  # flagged truncated by the edge
                )
            )
        emitted += 1
    # RTCP mapping with a deliberately drifting clock (0.2 % fast).
    records.insert(
        5,
        (
            packet.KIND_RTCP,
            make_sender_report(
                ssrc=spec.ssrc, rtp_timestamp=ts0, unix_time=1_700_000_000.0,
                packet_count=5,
            ),
            0,
        ),
    )
    drift_rtp = (ts0 + 60 * per_packet) & 0xFFFFFFFF
    records.insert(
        90,
        (
            packet.KIND_RTCP,
            make_sender_report(
                ssrc=spec.ssrc, rtp_timestamp=drift_rtp,
                unix_time=1_700_000_000.0 + 0.6 * 0.998,
                packet_count=90,
            ),
            0,
        ),
    )
    # Conflicting colour metadata between two video records.
    records.append(
        (
            packet.KIND_VIDEO,
            video_record(
                pts_samples=0, duration_samples=rate * 2,
                metadata={
                    "color_space": "bt709",
                    "color_primaries": "bt709",
                    "color_transfer": "bt709",
                    "color_range": "tv",
                },
            ),
            0,
        )
    )
    records.append(
        (
            packet.KIND_VIDEO,
            video_record(
                pts_samples=rate * 2, duration_samples=rate * 2,
                metadata={
                    "color_space": "bt2020nc",
                    "color_primaries": "bt2020",
                    "color_transfer": "smpte2084",
                    "color_range": "pc",
                },
            ),
            0,
        )
    )
    # A near-end record is cut short, with one more valid packet after it, to
    # exercise resynchronisation inside the damaged family too.
    write_capture(out / "damaged", spec, records)


def build_boundary(out: Path) -> None:
    # 1) empty stream: valid capture, RTCP only, zero RTP packets.
    empty_spec = packet.StreamSpec(
        sample_rate=48000, channels=2, layout="stereo", clock_rate=48000,
        ssrc=0x33333333,
    )
    write_capture(
        out / "boundary" / "empty",
        empty_spec,
        [
            (
                packet.KIND_RTCP,
                make_sender_report(
                    ssrc=empty_spec.ssrc, rtp_timestamp=0,
                    unix_time=1_700_000_100.0, packet_count=0,
                ),
                0,
            )
        ],
    )
    # 2) timestamp wrap + short final block (2.35 s at 1 s chunks -> 0.35 s tail)
    rate, layout = 48000, "stereo"
    wrap_spec = packet.StreamSpec(
        sample_rate=rate, channels=2, layout=layout, clock_rate=rate,
        ssrc=0x44444444,
    )
    seconds = 2.35
    audio = make_tone(rate, layout, seconds)
    per_packet = 480
    ts0 = 0xFFFFF800
    records = []
    for i in range(audio.shape[0] // per_packet):
        block = audio[i * per_packet : (i + 1) * per_packet]
        records.append(
            (
                packet.KIND_RTP,
                rtp_packet(
                    block,
                    sequence=(500 + i) & 0xFFFF,
                    timestamp=(ts0 + i * per_packet) & 0xFFFFFFFF,
                    ssrc=wrap_spec.ssrc,
                ),
                0,
            )
        )
    write_capture(out / "boundary" / "wrap_short", wrap_spec, records)
    # 3) single-packet stream
    single_spec = packet.StreamSpec(
        sample_rate=rate, channels=2, layout=layout, clock_rate=rate,
        ssrc=0x55555555,
    )
    write_capture(
        out / "boundary" / "single",
        single_spec,
        [
            (
                packet.KIND_RTP,
                rtp_packet(
                    make_tone(rate, layout, 0.01),
                    sequence=7, timestamp=0xFFFFFF00, ssrc=single_spec.ssrc,
                ),
                0,
            )
        ],
    )
    # 4) unsupported / unknown layout: 3 channels, not in the matrix.
    weird_spec = packet.StreamSpec(
        sample_rate=rate, channels=3, layout="3.0", clock_rate=rate,
        ssrc=0x66666666,
    )
    block = np.zeros((per_packet, 3), dtype=np.float32)
    block[:, 0] = 0.05
    write_capture(
        out / "boundary" / "unknown_layout",
        weird_spec,
        [
            (
                packet.KIND_RTP,
                rtp_packet(
                    block, sequence=1, timestamp=0, ssrc=weird_spec.ssrc
                ),
                0,
            )
        ],
    )
    # 5) a record truncated mid-file, followed by more valid packets.
    trunc_spec = packet.StreamSpec(
        sample_rate=rate, channels=2, layout=layout, clock_rate=rate,
        ssrc=0x77777777,
    )
    base = make_tone(rate, layout, 0.5)
    recs = []
    for i in range(base.shape[0] // per_packet):
        recs.append(
            (
                packet.KIND_RTP,
                rtp_packet(
                    base[i * per_packet : (i + 1) * per_packet],
                    sequence=i,
                    timestamp=i * per_packet,
                    ssrc=trunc_spec.ssrc,
                ),
                0,
            )
        )
    write_capture(
        out / "boundary" / "truncated_record", trunc_spec, recs, truncate_at=12
    )
    # 6) a 44.1 kHz stereo capture: exercises resampling to 48 kHz.
    rate44 = 44100
    spec44 = packet.StreamSpec(
        sample_rate=rate44, channels=2, layout="stereo", clock_rate=rate44,
        ssrc=0x88888888,
    )
    audio44 = make_tone(rate44, "stereo", 2.0)
    recs44 = []
    for i in range(audio44.shape[0] // 441):
        recs44.append(
            (
                packet.KIND_RTP,
                rtp_packet(
                    audio44[i * 441 : (i + 1) * 441],
                    sequence=i,
                    timestamp=i * 441,
                    ssrc=spec44.ssrc,
                ),
                0,
            )
        )
    write_capture(out / "boundary" / "rate_44100", spec44, recs44)
    # 7) a 5.1 capture: exercises the explicit channel matrix.
    spec51 = packet.StreamSpec(
        sample_rate=rate, channels=6, layout="5.1", clock_rate=rate,
        ssrc=0x99999999,
    )
    audio51 = make_tone(rate, "5.1", 2.0)
    recs51 = []
    for i in range(audio51.shape[0] // per_packet):
        recs51.append(
            (
                packet.KIND_RTP,
                rtp_packet(
                    audio51[i * per_packet : (i + 1) * per_packet],
                    sequence=i,
                    timestamp=i * per_packet,
                    ssrc=spec51.ssrc,
                ),
                0,
            )
        )
    write_capture(out / "good" / "surround51", spec51, recs51)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, help="fixture output directory")
    parser.add_argument(
        "--only",
        choices=["good", "damaged", "boundary"],
        help="generate a single fixture family",
    )
    args = parser.parse_args(argv)
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    builders = {
        "good": build_good,
        "damaged": build_damaged,
        "boundary": build_boundary,
    }
    families = [args.only] if args.only else list(builders)
    for family in families:
        builders[family](out)
    summary = {}
    for family in families:
        root = out / family
        summary[family] = sorted(
            str(p.relative_to(out)) for p in root.rglob("stream.json")
        ) if root.exists() else []
    print(json.dumps({"fixtures": str(out), "families": summary}, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
