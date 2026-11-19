"""统一时间基准、错误分类、轨道状态与清单数据模型。

所有模块共享本文件定义的时间基准（微秒整数）与错误模型，
不允许在其他模块各自发明时间表示或退出码。
"""
from __future__ import annotations

import enum
import hashlib
import json
import os
import tempfile
from dataclasses import asdict, dataclass, field
from pathlib import Path

# ---------------------------------------------------------------- 常量

US_PER_SEC = 1_000_000
SCHEMA_VERSION = 1
MANIFEST_KIND = "fielddeliver-manifest"

# 处理规模上限（本地多核工作站可承载的素材），可被清单 limits 覆盖。
DEFAULT_LIMITS: dict[str, int] = {
    "max_file_bytes": 2 * 1024 * 1024 * 1024,   # 单文件 2 GiB
    "max_total_bytes": 16 * 1024 * 1024 * 1024,  # 总输入 16 GiB
    "max_tracks": 64,
    "max_concurrency": 8,
    "max_cache_bytes": 512 * 1024 * 1024,        # 并发缓存 512 MiB
    "chunk_bytes": 4 * 1024 * 1024,             # 分块读取 4 MiB
    "max_temp_bytes": 2 * 1024 * 1024 * 1024,
}


# ---------------------------------------------------------------- 枚举

class ExitCode(enum.IntEnum):
    OK = 0
    INTERNAL = 1
    INVALID_INPUT = 2
    UNRECOVERABLE = 3


class ErrorCategory(enum.Enum):
    """错误分类。severity 决定是否阻断下游与最终退出码。"""

    MANIFEST_MISSING_FIELD = ("missing_field", ExitCode.INVALID_INPUT)
    MANIFEST_BAD_VALUE = ("bad_value", ExitCode.INVALID_INPUT)
    PATH_OUTSIDE_ROOT = ("path_outside_root", ExitCode.INVALID_INPUT)
    INPUT_TOO_LARGE = ("input_too_large", ExitCode.INVALID_INPUT)
    DEPENDENCY_MISSING = ("dependency_missing", ExitCode.INVALID_INPUT)
    DEPENDENCY_CYCLE = ("dependency_cycle", ExitCode.INVALID_INPUT)
    COLOR_CONFLICT = ("color_conflict", ExitCode.INVALID_INPUT)

    PACKET_LOSS_RECOVERED = ("packet_loss_recovered", ExitCode.OK)
    TIMESTAMP_REPAIRED = ("timestamp_repaired", ExitCode.OK)
    DUPLICATE_FRAME = ("duplicate_frame", ExitCode.OK)
    INDEX_STALE = ("index_stale", ExitCode.OK)
    EMPTY_AUDIO = ("empty_audio", ExitCode.OK)

    TIMESTAMP_GAP = ("timestamp_gap", ExitCode.UNRECOVERABLE)
    TRUNCATED_STREAM = ("truncated_stream", ExitCode.UNRECOVERABLE)
    PACKET_LOSS_UNRECOVERED = ("packet_loss_unrecovered", ExitCode.UNRECOVERABLE)
    DECODE_FAILURE = ("decode_failure", ExitCode.UNRECOVERABLE)
    UPSTREAM_FAILED = ("upstream_failed", ExitCode.UNRECOVERABLE)

    INTERNAL = ("internal", ExitCode.INTERNAL)

    def __init__(self, code: str, exit_code: ExitCode) -> None:
        self.code = code
        self.exit_code = exit_code


class TrackState(enum.Enum):
    PENDING = "pending"
    RUNNING = "running"
    DONE = "done"
    SKIPPED = "skipped"        # 上游失败导致阻止提交
    FAILED = "failed"


class TrackKind(enum.Enum):
    VIDEO = "video"
    AUDIO = "audio"


class VideoCodec(enum.Enum):
    H264 = "h264"
    H265 = "h265"
    AV1 = "av1"

    @property
    def pyav_format(self) -> str:
        """裸码流的 PyAV 容器格式；AV1 用 ivf 承载时间戳。"""
        return {
            VideoCodec.H264: "h264",
            VideoCodec.H265: "hevc",
            VideoCodec.AV1: "ivf",
        }[self]

    @property
    def extension(self) -> str:
        return {VideoCodec.H264: ".h264", VideoCodec.H265: ".hevc",
                VideoCodec.AV1: ".ivf"}[self]


class ColorStrategy(enum.Enum):
    INHERIT = "inherit"        # 继承参考轨/清单默认
    OVERRIDE = "override"      # 明确覆盖
    ERROR = "error"            # 冲突即报错


class PaddingPolicy(enum.Enum):
    SILENCE = "silence"        # 静音补齐
    TRUNCATE = "truncate"      # 截断到整齐边界
    REJECT = "reject"          # 不接受非整齐边界 -> 输入无效


# ---------------------------------------------------------------- 错误载体

@dataclass(slots=True)
class Issue:
    """可定位的单条问题。byte_offset 用于现场定位损坏片段。"""
    category: ErrorCategory
    track: str
    message: str
    byte_offset: int | None = None
    pts_us: int | None = None
    recovered: bool | None = None
    detail: dict = field(default_factory=dict)

    def to_dict(self) -> dict:
        d = asdict(self)
        d["category"] = self.category.code
        d["severity_exit"] = int(self.category.exit_code)
        return d


class PipelineError(Exception):
    """带退出码的流水线错误，cli.py 统一捕获。"""

    def __init__(self, category: ErrorCategory, message: str,
                 track: str = "-", byte_offset: int | None = None) -> None:
        super().__init__(message)
        self.category = category
        self.message = message
        self.track = track
        self.byte_offset = byte_offset

    @property
    def exit_code(self) -> ExitCode:
        return self.category.exit_code


# ---------------------------------------------------------------- 清单模型

@dataclass(slots=True)
class ColorDecl:
    primaries: str | None = None
    transfer: str | None = None
    space: str | None = None
    range: str | None = None
    strategy: ColorStrategy = ColorStrategy.INHERIT

    @classmethod
    def from_dict(cls, d: dict) -> "ColorDecl":
        return cls(
            primaries=d.get("primaries"), transfer=d.get("transfer"),
            space=d.get("space"), range=d.get("range"),
            strategy=ColorStrategy(d.get("strategy", "inherit")),
        )

    def to_dict(self) -> dict:
        return {"primaries": self.primaries, "transfer": self.transfer,
                "space": self.space, "range": self.range,
                "strategy": self.strategy.value}


@dataclass(slots=True)
class TrackDecl:
    track_id: str
    kind: TrackKind
    path: str
    index_path: str | None = None
    # 视频
    codec: VideoCodec | None = None
    frame_rate: float | None = None
    time_base: tuple[int, int] | None = None
    gop_size: int | None = None
    pts_wrap_modulus: int | None = None     # 声明的 PTS 回绕周期（帧号单位）
    # 音频
    sample_rate: int | None = None
    channels: int | None = None
    channel_layout: str | None = None
    target_sample_rate: int | None = None
    target_layout: str | None = None
    padding_policy: PaddingPolicy = PaddingPolicy.SILENCE
    # 同步：相对全局时间线起点的偏移（微秒）
    sync_offset_us: int = 0
    depends_on: list[str] = field(default_factory=list)
    color: ColorDecl = field(default_factory=ColorDecl)
    # 现场工程师标注的已知丢包事件（字节偏移, 长度, 可恢复性）
    known_losses: list[dict] = field(default_factory=list)

    @classmethod
    def from_dict(cls, track_id: str, d: dict) -> "TrackDecl":
        kind = TrackKind(d["kind"])
        tb = d.get("time_base")
        return cls(
            track_id=track_id, kind=kind, path=d["path"],
            index_path=d.get("index_path"),
            codec=VideoCodec(d["codec"]) if d.get("codec") else None,
            frame_rate=d.get("frame_rate"),
            time_base=(tb[0], tb[1]) if tb else None,
            gop_size=d.get("gop_size"),
            pts_wrap_modulus=d.get("pts_wrap_modulus"),
            sample_rate=d.get("sample_rate"),
            channels=d.get("channels"),
            channel_layout=d.get("channel_layout"),
            target_sample_rate=d.get("target_sample_rate"),
            target_layout=d.get("target_layout"),
            padding_policy=PaddingPolicy(d.get("padding_policy", "silence")),
            sync_offset_us=int(d.get("sync_offset_us", 0)),
            depends_on=list(d.get("depends_on", [])),
            color=ColorDecl.from_dict(d.get("color", {})),
            known_losses=list(d.get("known_losses", [])),
        )

    def to_dict(self) -> dict:
        d = asdict(self)
        d["kind"] = self.kind.value
        d["codec"] = self.codec.value if self.codec else None
        d["padding_policy"] = self.padding_policy.value
        d["color"] = self.color.to_dict()
        return d


@dataclass(slots=True)
class Manifest:
    project: str
    root: str                      # 清单所在目录，所有路径限定其下
    tracks: dict[str, TrackDecl]
    color_default: ColorDecl
    limits: dict[str, int]
    reference_video: str | None
    global_frame_rate: float | None
    source_hash: str = ""

    @classmethod
    def from_dict(cls, doc: dict, root: str) -> "Manifest":
        if doc.get("kind") != MANIFEST_KIND:
            raise PipelineError(
                ErrorCategory.MANIFEST_BAD_VALUE,
                f"manifest kind 必须为 {MANIFEST_KIND}")
        tracks = {tid: TrackDecl.from_dict(tid, td)
                  for tid, td in doc.get("tracks", {}).items()}
        limits = dict(DEFAULT_LIMITS)
        limits.update(doc.get("limits", {}))
        return cls(
            project=doc.get("project", "unnamed"),
            root=root, tracks=tracks,
            color_default=ColorDecl.from_dict(doc.get("color_default", {})),
            limits=limits,
            reference_video=doc.get("reference_video"),
            global_frame_rate=doc.get("global_frame_rate"),
            source_hash=doc.get("source_hash", ""),
        )

    def to_dict(self) -> dict:
        return {
            "kind": MANIFEST_KIND,
            "schema_version": SCHEMA_VERSION,
            "project": self.project,
            "reference_video": self.reference_video,
            "global_frame_rate": self.global_frame_rate,
            "color_default": self.color_default.to_dict(),
            "limits": self.limits,
            "source_hash": self.source_hash,
            "tracks": {tid: t.to_dict() for tid, t in self.tracks.items()},
        }


# ---------------------------------------------------------------- 处理结果模型

@dataclass(slots=True)
class FrameRecord:
    """修复后时间线上的一帧（或一个显式缺口）。"""
    index: int                     # 修复后帧序号
    byte_offset: int               # 裸码流字节偏移；缺口为 -1
    size: int
    is_keyframe: bool
    pts_us: int                    # 修复后全局 PTS（微秒）
    raw_pts: int | None = None     # 原始帧号/PTS
    is_gap: bool = False           # 显式缺口：不伪造帧


@dataclass(slots=True)
class VideoInfo:
    width: int
    height: int
    frame_rate: float
    duration_us: int
    packet_count: int
    frame_count: int
    keyframe_count: int
    gaps: int = 0


@dataclass(slots=True)
class AudioInfo:
    sample_rate: int
    channels: int
    layout: str
    frames: int                    # 原始样本帧数
    duration_us: int
    target_rate: int
    target_channels: int
    target_layout: str
    output_frames: int             # 归一后样本帧数
    padded_samples: int = 0
    channel_map: list[int] = field(default_factory=list)


@dataclass(slots=True)
class TrackResult:
    track_id: str
    kind: TrackKind
    state: TrackState
    issues: list[Issue] = field(default_factory=list)
    video: VideoInfo | None = None
    audio: AudioInfo | None = None
    outputs: dict[str, str] = field(default_factory=dict)
    latency_ms: list[float] = field(default_factory=list)  # 阶段延迟
    checkpoints: list[str] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "track_id": self.track_id, "kind": self.kind.value,
            "state": self.state.value,
            "issues": [i.to_dict() for i in self.issues],
            "video": asdict(self.video) if self.video else None,
            "audio": asdict(self.audio) if self.audio else None,
            "outputs": self.outputs,
            "checkpoints": self.checkpoints,
        }


# ---------------------------------------------------------------- 工具函数

def frames_to_us(frame: int, frame_rate: float) -> int:
    return round(frame * US_PER_SEC / frame_rate)


def us_to_frames(us: int, frame_rate: float) -> float:
    return us * frame_rate / US_PER_SEC


def sha256_file(path: str | os.PathLike, chunk: int = 4 * 1024 * 1024) -> str:
    """分块哈希，避免一次性读入超大文件。"""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            block = f.read(chunk)
            if not block:
                break
            h.update(block)
    return h.hexdigest()


def atomic_write_bytes(path: str | os.PathLike, data: bytes) -> None:
    """临时文件 + 原子替换；同目录 rename 在 macOS/POSIX 上为原子操作。"""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".fd-", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def atomic_write_json(path: str | os.PathLike, obj: dict | list) -> None:
    atomic_write_bytes(path, json.dumps(
        obj, ensure_ascii=False, indent=2, sort_keys=True).encode("utf-8"))


def read_json(path: str | os.PathLike) -> dict:
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def resolve_within_root(root: str, rel: str) -> Path:
    """把清单内相对路径限定在 root 下，拒绝任何逃逸（含符号链接目标）。"""
    root_p = Path(root).resolve()
    candidate = (root_p / rel).resolve()
    if root_p != candidate and root_p not in candidate.parents:
        raise PipelineError(
            ErrorCategory.PATH_OUTSIDE_ROOT,
            f"路径 {rel!r} 解析到清单根目录之外: {candidate}", track=rel)
    return candidate
