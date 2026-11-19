"""自测样例生成：成功、突发丢包、截断、异常时间戳与空音轨。

纯本地生成，不引用任何外部文件。视频用 PyAV 编码裸码流，
音频用 soundfile 生成带同步脉冲的 WAV，便于后期用互相关实测同步误差。
所有随机由 seed 驱动，保证可复现。
"""
from __future__ import annotations

import os
from fractions import Fraction
from pathlib import Path

import av
import numpy as np
import soundfile as sf

from .model import (ColorDecl, ColorStrategy, Manifest, PaddingPolicy,
                    TrackDecl, TrackKind, VideoCodec, atomic_write_json,
                    resolve_within_root)

NANOS = 1_000_000

# 同步闪光帧序号；对应时刻 SYNC_FLASH_FRAME / frame_rate 秒，
# 音频同步脉冲也放在同一时刻，供互相关实测音画同步。
SYNC_FLASH_FRAME = 2
SYNC_PULSE_S = SYNC_FLASH_FRAME / 30.0


# ---------------------------------------------------------------- 视频生成

def _encode_video(path: str, codec: VideoCodec, frames: int,
                  rate: int, width: int, height: int, gop: int,
                  rng: np.random.Generator) -> None:
    fmt = codec.pyav_format
    container = av.open(path, "w", format=fmt)
    stream = container.add_stream(
        "libx264" if codec is VideoCodec.H264
        else "libx265" if codec is VideoCodec.H265 else "av1",
        rate=rate)
    stream.width = width
    stream.height = height
    stream.pix_fmt = "yuv420p"
    stream.time_base = Fraction(1, rate)
    if codec is VideoCodec.AV1:
        stream.options = {"usage": "realtime", "rc": "cqp", "g": str(gop)}
    else:
        stream.options = {"g": str(gop), "preset": "ultrafast"}

    for i in range(frames):
        img = np.zeros((height, width, 3), dtype=np.uint8)
        # 移动亮条 + 帧号纹理，保证帧间有差异、可被编码。
        x = int((i * 7) % max(1, width - 20))
        img[:, x:x + 16, 0] = 200
        img[:, :, 1] = (i * 9) % 255
        # 同步闪光：固定第 2 帧全帧高亮，与音频同步脉冲同一时刻(2/rate 秒)。
        if i == SYNC_FLASH_FRAME:
            img[:, :, :] = 235
        noise = rng.integers(0, 24, size=(height, width, 1), dtype=np.int16)
        img = np.clip(img.astype(np.int16) + noise, 0, 255).astype(np.uint8)
        frame = av.VideoFrame.from_ndarray(img, format="rgb24")
        frame.pts = i
        for pkt in stream.encode(frame):
            container.mux(pkt)
    for pkt in stream.encode(None):
        container.mux(pkt)
    container.close()


def _packet_offsets(path: str, codec: VideoCodec) -> list[tuple[int, int, bool]]:
    fh = open(path, "rb")
    out: list[tuple[int, int, bool]] = []
    try:
        container = av.open(fh, format=codec.pyav_format)
        stream = container.streams.video[0]
        for pkt in container.demux(stream):
            if pkt.pos is not None and pkt.size > 0:
                out.append((int(pkt.pos), int(pkt.size),
                            bool(pkt.is_keyframe)))
        container.close()
    finally:
        fh.close()
    return out


def _parse_ivf(raw: bytes) -> tuple[bytearray, list[list]]:
    """解析 ivf：返回(32B 文件头, [[pts, payload], ...])。"""
    pos = 32
    frames: list[list] = []
    while pos + 12 <= len(raw):
        size = int.from_bytes(raw[pos:pos + 4], "little")
        pts = int.from_bytes(raw[pos + 4:pos + 12], "little")
        frames.append([pts, raw[pos + 12:pos + 12 + size]])
        pos += 12 + size
    return bytearray(raw[:32]), frames


def _write_ivf(path: str, header: bytearray, frames: list[list]) -> None:
    header[24:28] = len(frames).to_bytes(4, "little")
    out = bytearray(header)
    for pts, payload in frames:
        out += len(payload).to_bytes(4, "little")
        out += int(pts).to_bytes(8, "little") + bytes(payload)
    Path(path).write_bytes(out)


def inject_ivf_packet_loss(path: str, count: int,
                           rng: np.random.Generator) -> list[dict]:
    """ivf 帧级丢包：仅丢 GOP 尾部、不被后续帧引用的 inter 帧。

    ivf 带长度前缀且 AV1 有帧间引用，不能像 annex-B 那样直接删字节；
    删除被引用的 P 帧会导致整链解码失败，故只选每 GOP 末尾帧。
    保留原 PTS 从而在时间线上形成可推断的短缺口。
    """
    raw = Path(path).read_bytes()
    header, frames = _parse_ivf(raw)
    key_idx = {i for i, (pts, _) in enumerate(frames) if pts % 10 == 0}
    # 每个 GOP 的最后一帧（在下一关键帧之前）即安全可丢帧。
    gop_tails = [i for i in range(len(frames)) if i not in key_idx
                 and (i + 1 in key_idx or i == len(frames) - 1)]
    if not gop_tails:
        return []
    take = min(count, len(gop_tails))
    chosen = sorted(rng.choice(gop_tails, size=take, replace=False).tolist())
    events = [{"byte_offset": -1, "size": len(frames[i][1]),
               "recoverable": True, "frame_pts": int(frames[i][0])}
              for i in chosen]
    kept = [f for i, f in enumerate(frames) if i not in set(chosen)]
    _write_ivf(path, header, kept)
    return events


def inject_packet_loss(path: str, codec: VideoCodec, count: int,
                       rng: np.random.Generator,
                       avoid_keyframes: bool = True) -> list[dict]:
    """删除完整 inter 包模拟突发丢包，返回已知丢包事件清单。"""
    if codec is VideoCodec.AV1:
        return inject_ivf_packet_loss(path, count, rng)
    offsets = _packet_offsets(path, codec)
    candidates = [(o, s) for (o, s, key) in offsets
                  if not (avoid_keyframes and key)]
    if len(candidates) <= count:
        chosen = candidates
    else:
        idxs = rng.choice(len(candidates), size=count, replace=False)
        chosen = [candidates[i] for i in sorted(idxs)]
    chosen.sort()
    data = Path(path).read_bytes()
    # 从后往前删，避免偏移失效。
    events = []
    for off, size in reversed(chosen):
        events.append({"byte_offset": off, "size": size, "recoverable": True})
        data = data[:off] + data[off + size:]
    Path(path).write_bytes(data)
    events.reverse()
    return events


def truncate_tail(path: str, codec: VideoCodec,
                  drop_bytes: int) -> dict:
    """截断码流尾部，制造不可恢复的截断事件。

    ivf：把最后一帧的 payload 截去一半，长度前缀与实际内容不符，
    触发解析/解码错误；annex-B：直接截去尾部 drop_bytes。
    """
    size = Path(path).stat().st_size
    if codec is VideoCodec.AV1:
        header, frames = _parse_ivf(Path(path).read_bytes())
        if frames:
            last_pts, last_payload = frames[-1]
            cut = max(1, len(last_payload) // 2)
            frames[-1] = [last_pts, last_payload[:len(last_payload) - cut]]
            _write_ivf(path, header, frames)
        new_size = Path(path).stat().st_size
        return {"byte_offset": new_size, "size": size - new_size,
                "recoverable": False}
    new_size = max(1, size - drop_bytes)
    Path(path).write_bytes(Path(path).read_bytes()[:new_size])
    return {"byte_offset": new_size, "size": size - new_size,
            "recoverable": False}


# ---------------------------------------------------------------- 音频生成

def make_sync_pulse(duration_s: float, rate: int, pulse_at_s: float,
                    rng: np.random.Generator,
                    freq: int = 1000) -> np.ndarray:
    """带单个尖锐同步脉冲的单声道信号，脉冲位置即音画对齐基准。"""
    n = int(duration_s * rate)
    x = (0.02 * rng.standard_normal(n)).astype(np.float32)
    center = int(pulse_at_s * rate)
    width = max(8, rate // 1000)
    env = np.hanning(width * 2)
    tone = np.sin(2 * np.pi * freq * np.arange(width * 2) / rate)
    lo = max(0, center - width)
    hi = min(n, center + width)
    x[lo:hi] += (0.8 * env * tone)[:hi - lo].astype(np.float32)
    return x


def write_wav(path: str, data: np.ndarray, rate: int) -> None:
    sf.write(path, data, rate, subtype="PCM_16")


def build_audio_track(out_dir: Path, track_id: str, rate: int,
                      channels: int, layout: str, duration_s: float,
                      pulse_at_s: float,
                      rng: np.random.Generator) -> tuple[str, np.ndarray]:
    mono = make_sync_pulse(duration_s, rate, pulse_at_s, rng)
    if channels == 1:
        data = mono[:, None]
    elif layout == "stereo":
        # 同步脉冲在两声道等幅出现，其余内容略有差异。
        other = make_sync_pulse(duration_s, rate, pulse_at_s, rng, freq=800)
        data = np.stack([mono, 0.6 * other], axis=1)
    else:
        cols = [mono]
        for _ in range(channels - 1):
            cols.append(0.3 * make_sync_pulse(
                duration_s, rate, pulse_at_s, rng, freq=600))
        data = np.stack(cols, axis=1)
    rel = f"media/{track_id}.wav"
    p = out_dir / rel
    p.parent.mkdir(parents=True, exist_ok=True)
    write_wav(str(p), data, rate)
    return rel, data


# ---------------------------------------------------------------- 时间戳异常

def inject_timestamp_anomaly(path: str, codec: VideoCodec,
                             wrap_modulus: int) -> int:
    """对 ivf(AV1) 重写容器 PTS 制造一次回绕；annex-B 无 PTS 仅声明模数。

    模拟编码器重启：wrap_modulus 之后的帧 PTS 从 0 重新计数（单次回绕），
    probe 按声明模数把它们还原回单调序列。
    """
    if codec is not VideoCodec.AV1:
        return wrap_modulus
    header, frames = _parse_ivf(Path(path).read_bytes())
    wrapped = [[(pts if pts < wrap_modulus else pts - wrap_modulus), payload]
               for pts, payload in frames]
    _write_ivf(path, header, wrapped)
    return wrap_modulus


# ---------------------------------------------------------------- demo 装配

def generate_demo(out_root: str, seed: int) -> Manifest:
    rng = np.random.default_rng(seed)
    out = Path(out_root).resolve()
    (out / "media").mkdir(parents=True, exist_ok=True)
    (out / "index").mkdir(parents=True, exist_ok=True)

    rate, gop, frames = 30, 10, 60          # 2 秒素材
    w, h = 160, 90
    # 三种编码各一机位；AV1 不可用时回退 H.265。
    codecs = [VideoCodec.H264, VideoCodec.H265, VideoCodec.AV1]
    tracks: dict[str, TrackDecl] = {}
    reference = "cam_a"

    cam_specs = [
        ("cam_a", codecs[0], 0, "bt709", ColorStrategy.OVERRIDE),
        ("cam_b", codecs[1], 1_000, "bt2020", ColorStrategy.INHERIT),
        ("cam_c", codecs[2], -1_500, None, ColorStrategy.INHERIT),
    ]
    for tid, codec, sync_off, color_space, strat in cam_specs:
        rel = f"media/{tid}{codec.extension}"
        path = out / rel
        try:
            _encode_video(str(path), codec, frames, rate, w, h, gop, rng)
        except av.FFmpegError:
            codec = VideoCodec.H265
            rel = f"media/{tid}{codec.extension}"
            path = out / rel
            _encode_video(str(path), codec, frames, rate, w, h, gop, rng)

        known_losses: list[dict] = []
        wrap = None
        if tid == "cam_b":
            # 突发丢包：删 3 个 inter 包（全部可恢复，纯成功样例）。
            known_losses = inject_packet_loss(str(path), codec, 3, rng)
        if tid == "cam_c":
            # 顺序：先 PTS 回绕，再可恢复丢包，最后制造不可恢复截断。
            if codec is VideoCodec.AV1:
                wrap = inject_timestamp_anomaly(str(path), codec, 64)
            known_losses = inject_packet_loss(str(path), codec, 1, rng)
            offs = _packet_offsets(str(path), codec)
            tail = (offs[-1][1] + 20) if offs else 64
            known_losses.append(truncate_tail(str(path), codec, tail))

        tracks[tid] = TrackDecl(
            track_id=tid, kind=TrackKind.VIDEO, path=rel,
            codec=codec, frame_rate=float(rate), gop_size=gop,
            sync_offset_us=sync_off, pts_wrap_modulus=wrap,
            known_losses=known_losses,
            color=ColorDecl(space=color_space, primaries=color_space,
                            strategy=strat))

    # 音频：分轨、不同采样率/布局/偏移；含一条空音轨边界样例。
    audio_specs = [
        ("aud_master", 48000, 2, "stereo", SYNC_PULSE_S, 0, 48000, "stereo",
         PaddingPolicy.SILENCE, ["cam_a"]),
        ("aud_lav", 44100, 1, "mono", SYNC_PULSE_S, 1_000, 16000, "mono",
         PaddingPolicy.SILENCE, ["cam_a"]),
        ("aud_empty", 48000, 1, "mono", SYNC_PULSE_S, 0, 48000, "mono",
         PaddingPolicy.SILENCE, ["cam_b"]),
    ]
    for tid, sr, ch, layout, pulse_s, off, tgt_sr, tgt_layout, pad, deps in \
            audio_specs:
        rel = f"media/{tid}.wav"
        p = out / rel
        if tid == "aud_empty":
            write_wav(str(p), np.zeros((0, ch), np.float32), sr)
        else:
            data = build_audio_track(out, tid, sr, ch, layout,
                                     frames / rate, pulse_s, rng)[1]
            write_wav(str(p), data, sr)
        tracks[tid] = TrackDecl(
            track_id=tid, kind=TrackKind.AUDIO, path=rel,
            sample_rate=sr, channels=ch, channel_layout=layout,
            target_sample_rate=tgt_sr, target_layout=tgt_layout,
            padding_policy=pad, sync_offset_us=off, depends_on=deps)

    manifest = Manifest(
        project=f"festival-demo-seed{seed}", root=str(out), tracks=tracks,
        color_default=ColorDecl(primaries="bt709", transfer="bt709",
                                space="bt709", range="tv"),
        limits={
            "max_file_bytes": 512 * 1024 * 1024,
            "max_total_bytes": 4 * 1024 * 1024 * 1024,
            "max_tracks": 32, "max_concurrency": 4,
            "max_cache_bytes": 256 * 1024 * 1024,
            "chunk_bytes": 1 * 1024 * 1024,
            "max_temp_bytes": 1 * 1024 * 1024 * 1024,
        },
        reference_video=reference, global_frame_rate=float(rate))
    doc = manifest.to_dict()
    atomic_write_json(out / "manifest.json", doc)
    return manifest
