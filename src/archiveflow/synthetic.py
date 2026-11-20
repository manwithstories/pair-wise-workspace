"""Self-generated normal and corrupted RTP/RTCP packet streams.

Builds a deterministic synthetic program (procedural video + sine audio),
encodes it with PyAV, packetizes into RTP with the archiveflow payload
mini-header, emits periodic RTCP Sender Reports, then applies selectable
impairments: duplicate sequence numbers, reordering beyond a window, loss
bursts, conflicting Sender Reports, tail truncation and empty input.
"""

from __future__ import annotations

import fractions
import struct
from dataclasses import dataclass, field

import av
import numpy as np

from . import CLOCK_RATE
from .errors import CapabilityError
from .rtp import CODEC_AUDIO, CODEC_VIDEO, RTCP_PT_SR, pack_payload

MAX_RTP_PAYLOAD = 1200
VIDEO_SSRC = 0xA5C0DE01
AUDIO_SSRC = 0xA5C0DE02


@dataclass
class SyntheticParams:
    duration_s: float = 24.0
    fps: int = 15
    width: int = 1920
    height: int = 1080
    audio_rate: int = 48000
    gop: int = 30
    seed: int = 12772
    source_bitrate: int = 8_000_000
    duplicate_prob: float = 0.0
    reorder_span: int = 0  # shuffle packets within this span
    loss_burst_prob: float = 0.0  # chance per packet that a burst starts
    loss_burst_len: int = 12
    rtcp_conflict: bool = False
    truncate_tail: float = 0.0  # fraction of media packets dropped at end
    empty: bool = False


@dataclass
class Bundle:
    packets: list[bytes]
    video_ssrc: int = VIDEO_SSRC
    audio_ssrc: int = AUDIO_SSRC
    fps: int = 15
    width: int = 1920
    height: int = 1080
    duration_s: float = 0.0
    n_frames: int = 0
    audio: np.ndarray = field(default_factory=lambda: np.zeros(0, np.int16))
    seed: int = 0

    def frame_rgb(self, index: int) -> np.ndarray:
        return synth_frame(index, self.width, self.height, self.seed)


def synth_frame(index: int, width: int, height: int, seed: int) -> np.ndarray:
    """Deterministic procedural RGB frame: moving gradient, drifting block
    and per-frame noise so encoders do real work."""
    rng = np.random.default_rng(seed * 100003 + index)
    yy, xx = np.mgrid[0:height, 0:width]
    t = index * 3
    r = (xx + t * 2) % 256
    g = (yy + t) % 256
    b = ((xx // 4) + (yy // 4) + t) % 256
    frame = np.stack([r, g, b], axis=-1).astype(np.uint8)
    # Drifting bright block gives motion vectors something to track.
    bx, by = (t * 5) % max(width - 200, 1), (t * 3) % max(height - 200, 1)
    frame[by:by + 200, bx:bx + 200] = 255 - frame[by:by + 200, bx:bx + 200]
    noise = rng.integers(0, 6, size=frame.shape, dtype=np.uint8)
    return np.clip(frame.astype(np.int16) + noise - 3, 0, 255).astype(np.uint8)


def synth_audio(duration_s: float, rate: int, seed: int) -> np.ndarray:
    n = int(duration_s * rate)
    t = np.arange(n) / rate
    rng = np.random.default_rng(seed + 7)
    sig = (0.4 * np.sin(2 * np.pi * 440 * t)
           + 0.2 * np.sin(2 * np.pi * 880 * t)
           + 0.02 * rng.standard_normal(n))
    return (np.clip(sig, -1, 1) * 32767).astype(np.int16)


def _rtp_header(seq: int, ts: int, ssrc: int, pt: int, marker: bool) -> bytes:
    return struct.pack(">BBHII", 0x80, (0x80 if marker else 0) | pt,
                       seq & 0xFFFF, ts & 0xFFFFFFFF, ssrc)


def _rtcp_sr(ssrc: int, ntp_seconds: float, rtp_ts: int) -> bytes:
    msb = int(ntp_seconds)
    lsb = int((ntp_seconds - msb) * 2**32)
    body = struct.pack(">III", msb, lsb, rtp_ts & 0xFFFFFFFF) + b"\x00" * 20
    return struct.pack(">BBHI", 0x80, RTCP_PT_SR, 6, ssrc) + body


def _encode_video(params: SyntheticParams) -> list[tuple[bytes, bool]]:
    codec = av.codec.Codec("mpeg4", "w")
    ctx = av.codec.CodecContext.create(codec, "w")
    ctx.width, ctx.height = params.width, params.height
    ctx.pix_fmt = "yuv420p"
    ctx.time_base = fractions.Fraction(1, params.fps)
    ctx.gop_size = params.gop
    ctx.bit_rate = params.source_bitrate
    ctx.options = {"threads": "1"}
    ctx.open()
    out: list[tuple[bytes, bool]] = []
    n_frames = int(params.duration_s * params.fps)
    for i in range(n_frames):
        arr = synth_frame(i, params.width, params.height, params.seed)
        frame = av.VideoFrame.from_ndarray(arr, format="rgb24")
        frame = frame.reformat(format="yuv420p")
        frame.pts = i
        for pkt in ctx.encode(frame):
            out.append((bytes(pkt), bool(pkt.is_keyframe)))
    for pkt in ctx.encode(None):
        out.append((bytes(pkt), bool(pkt.is_keyframe)))
    return out


def _packetize(params: SyntheticParams) -> list[bytes]:
    packets: list[bytes] = []
    video = _encode_video(params)
    audio = synth_audio(params.duration_s, params.audio_rate, params.seed)
    chunk = 960  # 20 ms at 48 kHz
    n_chunks = int(np.ceil(len(audio) / chunk))
    vseq = aseq = 0
    ts_per_frame = CLOCK_RATE // params.fps
    events: list[tuple[int, bytes]] = []  # (sort key, packet)
    for fi, (data, keyframe) in enumerate(video):
        parts = max(1, (len(data) + MAX_RTP_PAYLOAD - 1) // MAX_RTP_PAYLOAD)
        for part in range(parts):
            piece = data[part * MAX_RTP_PAYLOAD:(part + 1) * MAX_RTP_PAYLOAD]
            payload = pack_payload(fi, part, parts, keyframe, CODEC_VIDEO,
                                   piece)
            marker = part == parts - 1
            packets_rtp = _rtp_header(vseq, fi * ts_per_frame, VIDEO_SSRC,
                                      96, marker)
            events.append((fi * 2, packets_rtp + payload))
            vseq += 1
    for ci in range(n_chunks):
        piece = audio[ci * chunk:(ci + 1) * chunk].tobytes()
        payload = pack_payload(ci, 0, 1, False, CODEC_AUDIO, piece)
        ts = ci * chunk * CLOCK_RATE // params.audio_rate
        events.append((ci * 2 + 1, _rtp_header(aseq, ts, AUDIO_SSRC, 97,
                                               False) + payload))
        aseq += 1
    events.sort(key=lambda e: e[0])
    media = [p for _, p in events]
    # Interleave Sender Reports roughly every 2 seconds of media time.
    sr_every = max(1, len(media) // max(1, int(params.duration_s / 2)))
    out: list[bytes] = []
    base_ntp = 3_900_000_000.0
    for i, pkt in enumerate(media):
        if i and i % sr_every == 0:
            t_s = i / len(media) * params.duration_s
            out.append(_rtcp_sr(VIDEO_SSRC, base_ntp + t_s,
                                int(t_s * CLOCK_RATE)))
            out.append(_rtcp_sr(AUDIO_SSRC, base_ntp + t_s,
                                int(t_s * CLOCK_RATE)))
            if params.rtcp_conflict:
                out.append(_rtcp_sr(VIDEO_SSRC, base_ntp + t_s + 5.0,
                                    int(t_s * CLOCK_RATE)))
        out.append(pkt)
    return out


def _apply_impairments(packets: list[bytes], params: SyntheticParams,
                       is_rtcp: list[bool]) -> list[bytes]:
    rng = np.random.default_rng(params.seed + 11)
    items = list(zip(packets, is_rtcp))
    if params.loss_burst_prob > 0:
        # loss_burst_prob is bursts per second of media; each burst drops
        # loss_burst_len consecutive media packets.
        media_idx = [k for k, (_, r) in enumerate(items) if not r]
        n_bursts = int(round(params.loss_burst_prob * params.duration_s))
        drop: set[int] = set()
        if media_idx and n_bursts:
            starts = rng.choice(len(media_idx),
                                size=min(n_bursts, len(media_idx)),
                                replace=False)
            for pos in starts:
                drop.update(media_idx[pos:pos + params.loss_burst_len])
        items = [it for k, it in enumerate(items) if k not in drop]
    if params.reorder_span > 1:
        out, buf = [], []
        for it in items:
            buf.append(it)
            if len(buf) >= params.reorder_span:
                order = rng.permutation(len(buf))
                out.extend(buf[j] for j in order)
                buf = []
        out.extend(buf)
        items = out
    if params.duplicate_prob > 0:
        out = []
        for it in items:
            out.append(it)
            pkt, is_r = it
            if not is_r and rng.random() < params.duplicate_prob:
                out.append(it)  # retransmitted duplicate
        items = out
    if params.truncate_tail > 0:
        media = [k for k, (_, r) in enumerate(items) if not r]
        cut = media[int(len(media) * (1 - params.truncate_tail))] if media else 0
        items = [it for k, it in enumerate(items)
                 if k < cut or items[k][1]]
    return [pkt for pkt, _ in items]


def generate(params: SyntheticParams) -> Bundle:
    try:
        av.codec.Codec("mpeg4", "w")
    except Exception as exc:
        raise CapabilityError(f"mpeg4 encoder unavailable: {exc}") from exc
    if params.empty:
        return Bundle(packets=[], fps=params.fps, width=params.width,
                      height=params.height, duration_s=params.duration_s,
                      seed=params.seed)
    raw = _packetize(params)
    is_rtcp = [p[1] == RTCP_PT_SR for p in raw]
    pkts = _apply_impairments(raw, params, is_rtcp)
    n_frames = int(params.duration_s * params.fps)
    return Bundle(packets=pkts, fps=params.fps, width=params.width,
                  height=params.height, duration_s=params.duration_s,
                  n_frames=n_frames,
                  audio=synth_audio(params.duration_s, params.audio_rate,
                                    params.seed),
                  seed=params.seed)
