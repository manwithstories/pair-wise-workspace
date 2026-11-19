"""PCM/WAV 块读取与损坏分类。

输出全部为 float64、值域 [-1.0, 1.0]，单声道形状 (N,)，多声道 (N, C)。
损坏分类（error_code）：
    truncated_wav      WAV 头被截断，无法解析
    data_length_mismatch  数据区长度与头部声明不符（给出截断字节位置）
    illegal_channels   非法声道数（0 或超过上限）
    empty_block        数据区为空（零帧）
    unsupported_layout 位深/格式不受支持（定位到轨道与源块）
同一块的“内容哈希”取源文件原始字节的 SHA-256，供幂等去重与恢复核验。
"""

from dataclasses import dataclass
import hashlib
import wave
from pathlib import Path

import numpy as np
import soundfile as sf

from .errors import MediaError

MAX_CHANNELS = 32
SUPPORTED_WIDTHS = (1, 2, 3, 4)
_PCM_SIGNED = {1: np.uint8, 2: np.int16, 3: np.int32, 4: np.int32}
_PCM_DIVISOR = {1: 128.0, 2: 32768.0, 3: 2147483648.0, 4: 2147483648.0}


@dataclass
class BlockData:
    block_id: str
    path: str
    samples: np.ndarray           # float64, shape (frames, channels)
    sample_rate: int
    channels: int
    width: int                    # 采样位深字节数
    content_hash: str             # 源文件原始字节 SHA-256（64 hex）
    declared_frames: int          # 头部声明帧数
    actual_frames: int            # 实际解析帧数
    truncated: bool               # 数据区短于声明
    truncate_offset: int | None   # 截断发生的文件字节偏移（若有）
    is_pcm: bool

    @property
    def length_mismatch(self) -> bool:
        return self.declared_frames != self.actual_frames


def _hash_file(path: Path) -> tuple[str, bytes]:
    try:
        raw = path.read_bytes()
    except FileNotFoundError as exc:
        raise MediaError(
            f"source block missing: {path}",
            error_code="block_not_found",
            byte_offset=0,
        ) from exc
    return hashlib.sha256(raw).hexdigest(), raw


def _decode_pcm(raw: bytes, rate: int, channels: int, width: int,
                block_id: str) -> np.ndarray:
    """无 WAV 头的裸 PCM，小端；宽度 1/2/3/4 字节。"""
    if channels <= 0 or channels > MAX_CHANNELS:
        raise MediaError(
            f"illegal channel count {channels}",
            error_code="illegal_channels",
            source_block=block_id,
            channels=channels,
        )
    if width not in SUPPORTED_WIDTHS:
        raise MediaError(
            f"unsupported sample width {width}",
            error_code="unsupported_layout",
            source_block=block_id,
            byte_offset=0,
        )
    frame_bytes = width * channels
    usable = len(raw) // frame_bytes * frame_bytes
    if usable == 0:
        raise MediaError(
            "empty PCM block", error_code="empty_block", source_block=block_id
        )
    payload = raw[:usable]
    if width == 3:
        # 24bit 小端 -> 符号扩展到 int32。
        b = np.frombuffer(payload, dtype=np.uint8).reshape(-1, 3)
        i32 = (b[:, 0].astype(np.int32)
               | (b[:, 1].astype(np.int32) << 8)
               | (b[:, 2].astype(np.int32) << 16))
        i32 = np.where(i32 & 0x800000, i32 - 0x1000000, i32)
        integer = i32
    else:
        integer = np.frombuffer(
            payload, dtype=np.dtype(_PCM_SIGNED[width]).newbyteorder("<")
        )
        if width == 1:  # 8bit PCM 为无符号，中点 128。
            integer = integer.astype(np.int16) - 128
    frames = integer.reshape(-1, channels)
    return frames.astype(np.float64) / _PCM_DIVISOR[width]


def _scan_wav(path: Path, raw: bytes, block_id: str) -> tuple[wave._wave_params, int, int]:
    """返回 (params, data_chunk_offset, declared_data_bytes)。"""
    # wave 对 nChannels=0 直接抛错，先手工读取 fmt 字段做精确分类。
    if len(raw) >= 24 and raw[:4] == b"RIFF" and raw[8:12] == b"WAVE":
        n_channels = int.from_bytes(raw[22:24], "little")
        if n_channels <= 0 or n_channels > MAX_CHANNELS:
            raise MediaError(
                f"illegal channel count {n_channels}",
                error_code="illegal_channels",
                source_block=block_id,
                channels=n_channels,
                byte_offset=22,
            )
        framerate = int.from_bytes(raw[24:28], "little")
        if framerate == 0:
            raise MediaError(
                "zero sample rate in WAV header",
                error_code="zero_sample_rate",
                source_block=block_id,
                byte_offset=24,
            )
    try:
        with wave.open(str(path), "rb") as wf:
            params = wf.getparams()
    except wave.Error as exc:
        raise MediaError(
            f"WAV header unreadable: {exc}",
            error_code="truncated_wav",
            source_block=block_id,
            byte_offset=len(raw),
        ) from exc
    except EOFError as exc:
        raise MediaError(
            "WAV header truncated before data chunk",
            error_code="truncated_wav",
            source_block=block_id,
            byte_offset=len(raw),
        ) from exc
    # 手工扫描 RIFF 块以定位 data chunk 的文件偏移与声明长度。
    if len(raw) < 12 or raw[:4] != b"RIFF" or raw[8:12] != b"WAVE":
        raise MediaError(
            "not a RIFF/WAVE container",
            error_code="truncated_wav",
            source_block=block_id,
            byte_offset=min(len(raw), 12),
        )
    pos = 12
    data_offset = data_declared = -1
    while pos + 8 <= len(raw):
        chunk_id = raw[pos:pos + 4]
        chunk_size = int.from_bytes(raw[pos + 4:pos + 8], "little")
        body = pos + 8
        if chunk_id == b"data":
            data_offset, data_declared = body, chunk_size
            break
        pos = body + chunk_size + (chunk_size & 1)
    if data_offset < 0:
        raise MediaError(
            "data chunk missing",
            error_code="truncated_wav",
            source_block=block_id,
            byte_offset=len(raw),
        )
    return params, data_offset, data_declared


def read_block(path: str | Path, block_id: str, *,
               pcm_rate: int | None = None, pcm_channels: int | None = None,
               pcm_width: int | None = None) -> BlockData:
    """读取并校验单个源块，损坏即抛 :class:`MediaError`。"""
    p = Path(path)
    content_hash, raw = _hash_file(p)

    if pcm_rate is not None:
        if pcm_width is None or pcm_channels is None:
            raise MediaError(
                "raw PCM block requires pcm_rate/pcm_channels/pcm_width",
                error_code="unsupported_layout",
                source_block=block_id,
            )
        samples = _decode_pcm(raw, pcm_rate, pcm_channels, pcm_width, block_id)
        frames = samples.shape[0]
        return BlockData(
            block_id=block_id, path=str(p), samples=samples, sample_rate=pcm_rate,
            channels=pcm_channels, width=pcm_width, content_hash=content_hash,
            declared_frames=frames, actual_frames=frames, truncated=False,
            truncate_offset=None, is_pcm=True,
        )

    params, data_offset, declared_bytes = _scan_wav(p, raw, block_id)
    if params.nchannels <= 0 or params.nchannels > MAX_CHANNELS:
        raise MediaError(
            f"illegal channel count {params.nchannels}",
            error_code="illegal_channels",
            source_block=block_id,
            channels=params.nchannels,
            byte_offset=22,
        )
    if params.framerate <= 0:
        raise MediaError(
            "zero sample rate in WAV header",
            error_code="zero_sample_rate",
            source_block=block_id,
        )
    if params.sampwidth not in SUPPORTED_WIDTHS:
        raise MediaError(
            f"unsupported sample width {params.sampwidth}",
            error_code="unsupported_layout",
            source_block=block_id,
            byte_offset=data_offset,
        )
    frame_bytes = params.nchannels * params.sampwidth
    declared_frames = declared_bytes // frame_bytes
    available = max(0, len(raw) - data_offset)
    actual_frames = available // frame_bytes

    if declared_frames == 0:
        raise MediaError(
            "empty WAV block",
            error_code="empty_block",
            source_block=block_id,
            byte_offset=data_offset,
        )
    if actual_frames == 0:
        # 头部声明有帧，但数据区不足一个完整采样帧：传输截断块。
        raise MediaError(
            "truncated block: data region shorter than one sample frame",
            error_code="truncated_wav",
            source_block=block_id,
            byte_offset=len(raw),
            declared_frames=declared_frames,
            actual_frames=0,
            content_hash=content_hash,
        )

    truncated = available < declared_bytes
    truncate_offset = data_offset + available if truncated else None
    # soundfile 独立往返解析；截断时它按可读帧数解码。
    try:
        samples, sr = sf.read(str(p), dtype="float64", always_2d=True)
    except RuntimeError as exc:
        raise MediaError(
            f"WAV decode failed: {exc}",
            error_code="truncated_wav",
            source_block=block_id,
            byte_offset=len(raw),
        ) from exc
    if sr != params.framerate or samples.shape[1] != params.nchannels:
        raise MediaError(
            "roundtrip header/decode disagreement",
            error_code="data_length_mismatch",
            source_block=block_id,
            byte_offset=data_offset,
        )
    if samples.shape[0] != actual_frames:
        # 读取器与手工扫描不一致也归入长度不符。
        actual_frames = samples.shape[0]
        truncated = True
        truncate_offset = data_offset + actual_frames * frame_bytes
    if truncated:
        raise MediaError(
            "data region shorter than header declaration",
            error_code="data_length_mismatch",
            source_block=block_id,
            byte_offset=truncate_offset,
            declared_frames=declared_frames,
            actual_frames=actual_frames,
            content_hash=content_hash,
        )
    return BlockData(
        block_id=block_id, path=str(p), samples=samples,
        sample_rate=params.framerate, channels=params.nchannels,
        width=params.sampwidth, content_hash=content_hash,
        declared_frames=declared_frames, actual_frames=actual_frames,
        truncated=False, truncate_offset=None, is_pcm=False,
    )
