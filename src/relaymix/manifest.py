"""清单解析：轨道、块顺序、偏移与交叠区间。

清单为 UTF-8 JSON，全部时间量以主采样率下的整数采样刻度（tick）描述。
校验失败统一抛 :class:`relaymix.errors.ManifestError`（退出码 2），拒绝：
负长度、未知轨道、非单调块序、越界区间、零采样率。
"""

from dataclasses import dataclass, field
import json
from pathlib import Path

from .errors import ManifestError

POLICY_ABORT = "abort"
POLICY_SILENCE = "silence"
VALID_POLICIES = (POLICY_ABORT, POLICY_SILENCE)


@dataclass(frozen=True)
class BlockSpec:
    """一个源块在主时间线上的摆放位置。"""

    block_id: str
    path: str
    start_tick: int          # 块首采样对应的主刻度
    length_ticks: int        # 块在主刻度上占据的长度（可由解码结果覆核）
    order: int               # 轨道内序号，从 0 开始
    # 原始 PCM（无 WAV 头）时在清单中显式声明格式；WAV 块留空。
    pcm_rate: int | None = None
    pcm_channels: int | None = None
    pcm_width: int | None = None


@dataclass(frozen=True)
class TrackSpec:
    name: str
    # 显式声道映射：源声道 i -> 输出声道 channel_map[i]，-1 表示丢弃。
    channel_map: tuple[int, ...]
    blocks: tuple[BlockSpec, ...]


@dataclass(frozen=True)
class OverlapSpec:
    """清单显式声明的交叠区间，仅做越界与轨道名校验。"""

    track_a: str
    track_b: str
    start_tick: int
    length_ticks: int


@dataclass(frozen=True)
class Manifest:
    master_rate: int
    output_start: int
    output_end: int
    frame_size: int               # 固定导出窗口（采样数）
    output_channels: int
    bad_block_policy: str
    tracks: tuple[TrackSpec, ...]
    overlaps: tuple[OverlapSpec, ...]
    raw: dict

    @property
    def output_length(self) -> int:
        return self.output_end - self.output_start

    def track(self, name: str) -> TrackSpec:
        for t in self.tracks:
            if t.name == name:
                return t
        raise ManifestError(
            f"unknown track: {name}", error_code="unknown_track", track=name
        )


def _require_int(obj: dict, key: str, *, min_value: int | None = None) -> int:
    if key not in obj:
        raise ManifestError(f"missing field: {key}", error_code="missing_field", field=key)
    value = obj[key]
    if not isinstance(value, int) or isinstance(value, bool):
        raise ManifestError(
            f"field {key} must be an integer", error_code="bad_field_type", field=key
        )
    if min_value is not None and value < min_value:
        raise ManifestError(
            f"field {key}={value} must be >= {min_value}",
            error_code="value_out_of_range",
            field=key,
            value=value,
        )
    return value


def _parse_block(obj: dict, order: int, out_end: int) -> BlockSpec:
    if not isinstance(obj, dict):
        raise ManifestError("block entry must be an object", error_code="bad_block")
    block_id = obj.get("block_id")
    path = obj.get("path")
    if not isinstance(block_id, str) or not block_id:
        raise ManifestError("block_id missing", error_code="bad_block")
    if not isinstance(path, str) or not path:
        raise ManifestError(
            "block path missing", error_code="bad_block", source_block=block_id
        )
    start = _require_int(obj, "start_tick", min_value=0)
    length = _require_int(obj, "length_ticks", min_value=0)
    if length == 0:
        raise ManifestError(
            "zero-length block rejected",
            error_code="negative_or_zero_length",
            source_block=block_id,
        )
    if start + length > out_end:
        raise ManifestError(
            f"block {block_id} exceeds output range",
            error_code="range_out_of_bounds",
            source_block=block_id,
            start_tick=start,
            length_ticks=length,
            output_end=out_end,
        )
    pcm_rate = obj.get("pcm_rate")
    pcm_channels = obj.get("pcm_channels")
    pcm_width = obj.get("pcm_width")
    pcm_fields = (pcm_rate, pcm_channels, pcm_width)
    if any(f is not None for f in pcm_fields):
        if not all(isinstance(f, int) for f in pcm_fields):
            raise ManifestError(
                "pcm_rate/pcm_channels/pcm_width must all be integers",
                error_code="bad_field_type",
                source_block=block_id,
            )
        if pcm_rate <= 0:
            raise ManifestError(
                "zero or negative pcm_rate",
                error_code="zero_sample_rate",
                source_block=block_id,
            )
        if pcm_channels <= 0:
            raise ManifestError(
                "illegal pcm channel count",
                error_code="illegal_channels",
                source_block=block_id,
            )
        if pcm_width not in (1, 2, 3, 4):
            raise ManifestError(
                "pcm_width must be one of 1,2,3,4",
                error_code="bad_field_type",
                source_block=block_id,
            )
    return BlockSpec(
        block_id=block_id, path=path, start_tick=start, length_ticks=length,
        order=order, pcm_rate=pcm_rate, pcm_channels=pcm_channels, pcm_width=pcm_width,
    )


def _parse_track(obj: dict, out_end: int, out_channels: int) -> TrackSpec:
    if not isinstance(obj, dict):
        raise ManifestError("track entry must be an object", error_code="bad_track")
    name = obj.get("name")
    if not isinstance(name, str) or not name:
        raise ManifestError("track name missing", error_code="bad_track")
    mapping = obj.get("channel_map")
    if not isinstance(mapping, list) or not mapping:
        raise ManifestError(
            "channel_map must be a non-empty list",
            error_code="bad_channel_map",
            track=name,
        )
    for idx, ch in enumerate(mapping):
        if not isinstance(ch, int) or isinstance(ch, bool) or ch < -1 or ch >= out_channels:
            raise ManifestError(
                f"channel_map target {ch} out of range [0,{out_channels})",
                error_code="bad_channel_map",
                track=name,
                source_channel=idx,
            )
    blocks_raw = obj.get("blocks", [])
    if not isinstance(blocks_raw, list) or not blocks_raw:
        raise ManifestError(
            "track needs at least one block", error_code="bad_block", track=name
        )
    blocks: list[BlockSpec] = []
    for order, b in enumerate(blocks_raw):
        spec = _parse_block(b, order, out_end)
        # 非单调块序：起点必须严格递增（交叠由显式 overlaps 声明，仍禁倒退/同点）。
        if blocks and spec.start_tick <= blocks[-1].start_tick:
            raise ManifestError(
                f"non-monotonic block order at {spec.block_id}",
                error_code="non_monotonic_blocks",
                track=name,
                source_block=spec.block_id,
            )
        blocks.append(spec)
    return TrackSpec(name=name, channel_map=tuple(mapping), blocks=tuple(blocks))


def parse_manifest(path: str | Path) -> Manifest:
    """从 JSON 文件加载并校验清单。"""
    p = Path(path)
    try:
        data = json.loads(p.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise ManifestError(
            f"manifest not found: {p}", error_code="manifest_not_found"
        ) from exc
    except json.JSONDecodeError as exc:
        raise ManifestError(
            f"manifest JSON invalid: {exc.msg}",
            error_code="manifest_json_invalid",
            byte_offset=exc.pos,
        ) from exc
    return build_manifest(data)


def build_manifest(data: dict) -> Manifest:
    """对内存中的清单字典做完整校验。"""
    if not isinstance(data, dict):
        raise ManifestError("manifest root must be an object", error_code="bad_manifest")
    master_rate = _require_int(data, "master_rate", min_value=1)
    out_start = _require_int(data, "output_start", min_value=0)
    out_end = _require_int(data, "output_end", min_value=0)
    if out_end <= out_start:
        raise ManifestError(
            "output range has non-positive length",
            error_code="negative_or_zero_length",
            output_start=out_start,
            output_end=out_end,
        )
    frame_size = _require_int(data, "frame_size", min_value=1)
    out_channels = _require_int(data, "output_channels", min_value=1)
    policy = data.get("bad_block_policy", POLICY_ABORT)
    if policy not in VALID_POLICIES:
        raise ManifestError(
            f"bad_block_policy must be one of {VALID_POLICIES}",
            error_code="bad_policy",
            value=policy,
        )
    tracks_raw = data.get("tracks", [])
    if not isinstance(tracks_raw, list) or not tracks_raw:
        raise ManifestError("at least one track required", error_code="bad_track")
    names: set[str] = set()
    tracks: list[TrackSpec] = []
    for t in tracks_raw:
        spec = _parse_track(t, out_end, out_channels)
        if spec.name in names:
            raise ManifestError(
                f"duplicate track name: {spec.name}",
                error_code="duplicate_track",
                track=spec.name,
            )
        names.add(spec.name)
        tracks.append(spec)
    overlaps: list[OverlapSpec] = []
    for o in data.get("overlaps", []):
        if not isinstance(o, dict):
            raise ManifestError("overlap entry must be an object", error_code="bad_overlap")
        a, b = o.get("track_a"), o.get("track_b")
        start = _require_int(o, "start_tick", min_value=0)
        length = _require_int(o, "length_ticks", min_value=1)
        for name in (a, b):
            if not isinstance(name, str) or name not in names:
                raise ManifestError(
                    f"overlap references unknown track: {name}",
                    error_code="unknown_track",
                    track=name,
                )
        if start + length > out_end:
            raise ManifestError(
                "overlap exceeds output range",
                error_code="range_out_of_bounds",
                start_tick=start,
                length_ticks=length,
            )
        overlaps.append(
            OverlapSpec(track_a=a, track_b=b, start_tick=start, length_ticks=length)
        )
    return Manifest(
        master_rate=master_rate,
        output_start=out_start,
        output_end=out_end,
        frame_size=frame_size,
        output_channels=out_channels,
        bad_block_policy=policy,
        tracks=tuple(tracks),
        overlaps=tuple(overlaps),
        raw=data,
    )
