"""可复用混音图:声道映射、增益应用与块求和。

全部为无状态纯函数,离线与流式执行器逐块共享,保证两条路径
在相同 float64 精度下走完全一致的运算顺序。

声道映射规则(目标固定为双声道):
- mono:   自动复制到左右;
- stereo: 默认 0->L, 1->R,可显式交换;
- 多声道(>2): 必须为每个源声道显式指定去向 "L"/"R"/"L+R",
  全部源声道都要列出,禁止静默丢弃;
- 映射索引越界立即报 ConfigError。
"""

from __future__ import annotations

import numpy as np

from .types import ConfigError, TrackSpec

# 单源声道的合法去向
_ROUTES = frozenset({"L", "R", "L+R"})


def validate_channel_map(source_channels: int, target: tuple[int, int]) -> None:
    """校验 stereo 及以下的双索引映射(mono 走 map_to_stereo 专门分支)。"""

    left, right = target
    if left < 0 or right < 0:
        raise ConfigError("声道映射索引必须非负")
    if left >= source_channels or right >= source_channels:
        raise ConfigError(
            f"声道映射索引越界:源仅 {source_channels} 声道,映射为 {target}"
        )
    if source_channels > 2:
        covered = set(target)
        if covered != set(range(source_channels)):
            raise ConfigError(
                f"多声道输入({source_channels}ch)需要显式 downmix 路由,"
                f"双索引映射 {target} 丢失了声道 "
                f"{sorted(set(range(source_channels)) - covered)}"
            )


def validate_routes(source_channels: int, routes: dict[int, str]) -> None:
    """多声道 downmix:每个源声道必须有且仅有一条合法去向。"""

    if set(routes.keys()) != set(range(source_channels)):
        missing = sorted(set(range(source_channels)) - set(routes.keys()))
        raise ConfigError(
            f"多声道输入({source_channels}ch)必须为每个源声道指定去向,"
            f"缺少声道 {missing};合法取值为 L / R / L+R"
        )
    for idx, route in routes.items():
        if route not in _ROUTES:
            raise ConfigError(
                f"声道 {idx} 的去向 {route!r} 非法,合法取值为 L / R / L+R"
            )


def map_to_stereo(
    signal: np.ndarray,
    target: tuple[int, int],
    routes: dict[int, str] | None = None,
) -> np.ndarray:
    """把 (frames, source_channels) 映射为 (frames, 2),float64。"""

    arr = np.asarray(signal, dtype=np.float64)
    if arr.ndim == 1:
        arr = arr[:, None]
    frames, source_channels = arr.shape
    # mono:复制到双声道,任何指向唯一声道的映射都等价
    if source_channels == 1:
        out = np.zeros((frames, 2), dtype=np.float64)
        out[:, 0] = arr[:, 0]
        out[:, 1] = arr[:, 0]
        return out
    # 多声道:显式 downmix,保证不丢声道
    if source_channels > 2:
        table = routes or {}
        validate_routes(source_channels, table)
        out = np.zeros((frames, 2), dtype=np.float64)
        for idx, route in table.items():
            if route in ("L", "L+R"):
                out[:, 0] += arr[:, idx]
            if route in ("R", "L+R"):
                out[:, 1] += arr[:, idx]
        return out
    # stereo
    validate_channel_map(source_channels, target)
    left, right = target
    out = np.zeros((frames, 2), dtype=np.float64)
    out[:, 0] = arr[:, left]
    out[:, 1] = arr[:, right]
    return out


def apply_gain(signal: np.ndarray, static_gain: float, curve_gain: np.ndarray) -> np.ndarray:
    """静态增益 × 自动化曲线增益(逐采样)。"""

    gains = np.asarray(curve_gain, dtype=np.float64)
    if gains.ndim == 1:
        gains = gains[:, None]
    return np.asarray(signal, dtype=np.float64) * (float(static_gain) * gains)


def place_track(block: np.ndarray, contribution: np.ndarray, frame_offset: int) -> None:
    """把轨道块按时间线偏移就地叠加进总块,越界部分裁掉、不足处保留零。"""

    block_frames = block.shape[0]
    seg_frames = contribution.shape[0]
    start = frame_offset
    end = start + seg_frames
    if end <= 0 or start >= block_frames:
        return
    src_start = max(0, -start)
    dst_start = max(0, start)
    dst_end = min(block_frames, end)
    block[dst_start:dst_end] += contribution[src_start : src_start + (dst_end - dst_start)]


def count_clipped(signal: np.ndarray, threshold: float) -> int:
    """统计 |样本| >= threshold 的采样数(按声道任一边越界即计)。"""

    arr = np.asarray(signal, dtype=np.float64)
    if arr.size == 0:
        return 0
    if arr.ndim == 1:
        return int(np.count_nonzero(np.abs(arr) >= threshold))
    return int(np.count_nonzero(np.any(np.abs(arr) >= threshold, axis=1)))


def render_track_block(
    track: TrackSpec,
    stereo: np.ndarray,
    curve_gain: np.ndarray,
) -> np.ndarray:
    """单轨单块完整渲染:映射 -> 静态/自动化增益,返回可叠加的 (n,2)。"""

    mapped = map_to_stereo(stereo, track.channels, track.routes)
    return apply_gain(mapped, track.gain, curve_gain)


def sum_blocks(blocks: list[np.ndarray]) -> np.ndarray:
    """求和一组同形块(主要供测试与离线一次性路径使用)。"""

    if not blocks:
        raise ConfigError("sum_blocks 至少需要一个块")
    acc = np.zeros_like(blocks[0], dtype=np.float64)
    for b in blocks:
        acc += np.asarray(b, dtype=np.float64)
    return acc
