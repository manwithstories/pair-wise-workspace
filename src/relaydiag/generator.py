"""Deterministic test-fixture generation.

The generator synthesises a small but structurally complete programme:

* an H.264 video elementary stream with a known keyframe cadence,
* an AAC stereo audio elementary stream,
* a second WAV audio track rendered with soundfile,
* multi-track edit points and transition boundaries,
* reference clock anchors, including one region with a deliberate drift break,
* 32-bit PTS wraparound near the end of the timeline.

Two artefacts are produced:

* ``media.mp4`` / ``audio2.wav`` -- real decodable media (proof the probe
  handles genuine codec streams),
* ``stream.rlyd`` -- the relay container carrying per-packet byte offsets and
  every anomaly the diagnosis stages need.

All randomness is seeded, so repeated runs produce byte-identical output.
"""

from __future__ import annotations

import io
import math
import os
from dataclasses import asdict, dataclass

import av
import numpy as np
import soundfile as sf

from .container import PacketRecord, write_container

#: Video tick rate; PTS values are expressed on this 90 kHz clock.
VIDEO_RATE = 90000
AUDIO_RATE = 48000
WRAP_MODULUS = 1 << 32
#: Deterministic payload fill byte; kept non-zero so truncation is visible.
PAYLOAD_FILL = 0x5A


@dataclass(frozen=True, slots=True)
class GenerationConfig:
    width: int = 176
    height: int = 144
    fps_num: int = 30
    fps_den: int = 1
    frame_count: int = 90            # 3 seconds at 30 fps
    gop: int = 15                    # keyframe every 0.5 s
    vfr_frames: tuple[int, ...] = (30, 31, 60, 61)  # doubled cadence points
    seed: int = 13309


def render_video_mp4(cfg: GenerationConfig, path: str) -> bytes:
    """Mux an H.264 stream; return its deterministic bytes (also written)."""
    buf = io.BytesIO()
    container = av.open(buf, "w", format="mp4")
    stream = container.add_stream("h264", rate=cfg.fps_num)
    stream.width = cfg.width
    stream.height = cfg.height
    stream.pix_fmt = "yuv420p"
    stream.options = {"g": str(cfg.gop), "preset": "ultrafast"}
    rng = np.random.default_rng(cfg.seed)
    tick = VIDEO_RATE // cfg.fps_num
    for i in range(cfg.frame_count):
        frame = _synthetic_frame(cfg, i, rng)
        frame.pts = i * tick
        for packet in stream.encode(frame):
            container.mux(packet)
    for packet in stream.encode():
        container.mux(packet)
    container.close()
    data = buf.getvalue()
    with open(path, "wb") as fh:
        fh.write(data)
    return data


def _synthetic_frame(cfg: GenerationConfig, index: int, rng) -> av.VideoFrame:
    """Bars + moving block; the block position makes every frame unique."""
    img = np.zeros((cfg.height, cfg.width, 3), dtype=np.uint8)
    band = cfg.width // 4
    for b in range(4):
        img[:, b * band:(b + 1) * band] = [(b * 60) % 256, (index * 8) % 256, 180]
    x = (index * 7) % max(1, cfg.width - 16)
    img[12:28, x:x + 16] = (255, 240, 200)
    return av.VideoFrame.from_ndarray(img, format="rgb24")


def render_audio_aac(cfg: GenerationConfig, path: str) -> bytes:
    """Synthesise a deterministic AAC stereo track and return its bytes."""
    buf = io.BytesIO()
    container = av.open(buf, "w", format="mp4")
    stream = container.add_stream("aac", rate=AUDIO_RATE)
    stream.layout = "stereo"
    stream.format = "fltp"
    duration = cfg.frame_count / cfg.fps_num
    t = np.linspace(0, duration, int(AUDIO_RATE * duration), endpoint=False)
    left = 0.25 * np.sin(2 * math.pi * 440 * t)
    right = 0.25 * np.sin(2 * math.pi * 660 * t + 0.5)
    frame_size = 1024
    for start in range(0, len(t), frame_size):
        block = np.stack(
            [left[start:start + frame_size], right[start:start + frame_size]]
        ).astype(np.float32)
        if block.shape[1] < frame_size:
            pad = frame_size - block.shape[1]
            block = np.pad(block, ((0, 0), (0, pad)))
        frame = av.AudioFrame.from_ndarray(block, format="fltp", layout="stereo")
        frame.sample_rate = AUDIO_RATE
        frame.pts = start
        for packet in stream.encode(frame):
            container.mux(packet)
    for packet in stream.encode(None):
        container.mux(packet)
    container.close()
    data = buf.getvalue()
    with open(path, "wb") as fh:
        fh.write(data)
    return data


def render_audio_wav(path: str, seconds: float = 3.0) -> bytes:
    """Second audio track rendered through soundfile (interleaved WAV)."""
    t = np.linspace(0, seconds, int(AUDIO_RATE * seconds), endpoint=False)
    track = np.stack(
        [
            0.2 * np.sin(2 * math.pi * 880 * t),
            0.2 * np.sin(2 * math.pi * 330 * t),
        ],
        axis=1,
    ).astype(np.float32)
    sf.write(path, track, AUDIO_RATE, subtype="PCM_16")
    with open(path, "rb") as fh:
        return fh.read()


def payload_for(index: int, size: int = 128) -> bytes:
    """Deterministic, non-uniform media payload for a relay packet."""
    return bytes(((PAYLOAD_FILL + index * 7) & 0xFF for _ in range(size)))


def make_edit_schedule(cfg: GenerationConfig) -> dict:
    """Multi-track edit points and transition boundaries in seconds."""
    fps = cfg.fps_num / cfg.fps_den
    return {
        "edits": [
            {"id": "E1", "track": "video", "start_s": 1.0, "end_s": 2.0},
            {"id": "E2", "track": "audio1", "start_s": 1.0, "end_s": 2.0},
            {"id": "E3", "track": "audio2", "start_s": 0.5, "end_s": 2.5},
        ],
        "transitions": [
            {"id": "T1", "at_s": 1.0, "in_track": "video", "out_track": "audio1",
             "duration_ms": 40},
            {"id": "T2", "at_s": 2.0, "in_track": "video", "out_track": "audio2",
             "duration_ms": 40},
        ],
        "frame_rate": fps,
    }


#: Anomaly identifiers used both at injection time and in the damage plan.
DAMAGE_TRUNCATED = "truncated_packet"
DAMAGE_DECODE = "decode_failure"
DAMAGE_DUPLICATE = "duplicate_timestamp"
DAMAGE_REVERSED = "reversed_timestamp"
DAMAGE_WRAP = "wraparound"


def build_relay_records(cfg: GenerationConfig) -> tuple[list[PacketRecord], list[dict]]:
    """Construct relay packets with injected anomalies and a damage plan.

    Returns the packet list (in transmission order) and the plan describing
    the expected byte/time windows.  Byte offsets are filled after writing, so
    the plan initially records packet indices and exact time ranges.
    """
    tick = VIDEO_RATE // cfg.fps_num
    records: list[PacketRecord] = []
    plan: list[dict] = []
    vfr = set(cfg.vfr_frames)
    # Frame 45 is dropped on the wire; frame 46 carries a corrupt payload.
    dropped_frame = 45
    corrupt_frame = 46
    duplicate_frame = 70
    # Two reversed neighbours near the end exercise the backwards path.
    reversed_frames = (78, 79)
    wrap_frame = 85

    for i in range(cfg.frame_count):
        if i == dropped_frame:
            plan.append(_plan(DAMAGE_TRUNCATED, "video", i, i, cfg, detail="missing"))
            continue
        pts = i * tick
        keyframe = i % cfg.gop == 0
        # 32-bit wraparound: emit a raw value just below the modulus boundary.
        wrapped = i == wrap_frame
        raw_pts = (pts - WRAP_MODULUS) if wrapped else pts
        if wrapped:
            plan.append(_plan(DAMAGE_WRAP, "video", i, i, cfg))
        record = PacketRecord(
            track="video",
            pts=raw_pts % WRAP_MODULUS,
            dts=raw_pts % WRAP_MODULUS,
            keyframe=keyframe,
            pts_wrapped=wrapped,
            payload=payload_for(i),
        )
        if i == corrupt_frame:
            # Flip payload bytes: decode-side corruption at a known offset.
            record.payload = bytes(b ^ 0xFF for b in record.payload)
            record.kind = "corrupt"
            plan.append(_plan(DAMAGE_DECODE, "video", i, i, cfg))
        records.append(record)
        if i in vfr:
            # VFR: an extra frame presented one half-tick later is normal
            # cadence variation, not a missing/damaged packet.
            records.append(PacketRecord(
                track="video", pts=pts + tick // 2, dts=pts + tick // 2,
                keyframe=False, kind="vfr", payload=payload_for(i, 64),
            ))
        if i == duplicate_frame:
            records.append(PacketRecord(
                track="video", pts=record.pts, dts=record.dts,
                keyframe=False, kind="duplicate", payload=payload_for(i, 64),
            ))
            plan.append(_plan(DAMAGE_DUPLICATE, "video", i, i, cfg))

    # Swap the two reversed neighbours in transmission order.
    idxs = [j for j, r in enumerate(records)
            if r.track == "video" and r.pts in (reversed_frames[0] * tick,
                                                reversed_frames[1] * tick)]
    if len(idxs) == 2:
        records[idxs[0]], records[idxs[1]] = records[idxs[1]], records[idxs[0]]
        plan.append(_plan(DAMAGE_REVERSED, "video",
                          reversed_frames[0], reversed_frames[1], cfg))

    _append_audio_track(records, "audio1", cfg, blocks=28)
    _append_audio_track(records, "audio2", cfg, blocks=14)
    return records, plan


def _append_audio_track(records: list[PacketRecord], name: str,
                        cfg: GenerationConfig, blocks: int) -> None:
    samples_per_block = AUDIO_RATE // 10  # 100 ms blocks
    seconds = cfg.frame_count / cfg.fps_num
    for b in range(blocks):
        start = b * samples_per_block
        if start / AUDIO_RATE >= seconds:
            break
        records.append(PacketRecord(
            track=name,
            pts=start,
            dts=start,
            keyframe=True,
            payload=payload_for(b + hash(name) % 97, 96),
        ))


def _plan(kind: str, track: str, frame0: int, frame1: int,
          cfg: GenerationConfig, detail: str | None = None) -> dict:
    fps = cfg.fps_num / cfg.fps_den
    return {
        "type": kind,
        "track": track,
        "frame_start": frame0,
        "frame_end": frame1,
        "time_start_s": frame0 / fps,
        "time_end_s": (frame1 + 1) / fps,
        "detail": detail or kind,
    }


def build_anchors(cfg: GenerationConfig) -> list[dict]:
    """Reference anchors; the second half switches to a faster node clock."""
    anchors = []
    fps = cfg.fps_num / cfg.fps_den
    for i in range(0, cfg.frame_count, 10):
        node_s = i / fps
        # First second nominal 1.0; afterwards node runs 0.5% slow.
        ref_s = node_s if node_s <= 1.0 else 1.0 + (node_s - 1.0) * 1.005
        anchors.append({
            "node_ticks": int(node_s * VIDEO_RATE),
            "node_rate": VIDEO_RATE,
            "ref_seconds": round(ref_s, 6),
        })
    return anchors


def generate_fixture(out_dir: str, cfg: GenerationConfig | None = None) -> dict:
    """Render media and the relay container; return the manifest dictionary."""
    cfg = cfg or GenerationConfig()
    os.makedirs(out_dir, exist_ok=True)
    video_bytes = render_video_mp4(cfg, os.path.join(out_dir, "media.mp4"))
    audio_bytes = render_audio_aac(cfg, os.path.join(out_dir, "audio1.m4a"))
    wav_bytes = render_audio_wav(os.path.join(out_dir, "audio2.wav"),
                                 cfg.frame_count / cfg.fps_num)
    records, plan = build_relay_records(cfg)
    header = {
        "format": "relaydiag-fixture",
        "version": 1,
        "video_rate": VIDEO_RATE,
        "audio_rate": AUDIO_RATE,
        "wrap_modulus": WRAP_MODULUS,
        "config": asdict(cfg),
        "tracks": [
            {"id": "video", "time_base": [1, VIDEO_RATE], "codec": "h264"},
            {"id": "audio1", "time_base": [1, AUDIO_RATE], "codec": "aac"},
            {"id": "audio2", "time_base": [1, AUDIO_RATE], "codec": "pcm16"},
        ],
        "edits": make_edit_schedule(cfg),
        "anchors": build_anchors(cfg),
        "damage_plan": plan,
        "media_sizes": {"mp4": len(video_bytes), "aac": len(audio_bytes),
                        "wav": len(wav_bytes)},
    }
    stream_path = os.path.join(out_dir, "stream.rlyd")
    write_container(stream_path, header, records)
    return {"stream_path": stream_path, "header": header,
            "packet_count": len(records), "out_dir": out_dir}
