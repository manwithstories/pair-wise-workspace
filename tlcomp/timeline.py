"""Project validation, fractional frame timing, and keyframe interpolation."""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

from .errors import ProjectError

BLEND_MODES = ("normal", "multiply", "screen", "overlay", "add")
EASINGS = ("linear", "ease-in-out")
ANIMATED_PROPERTIES = ("position", "scale", "rotation", "opacity")


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _number(value: Any, label: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ProjectError(f"{label} must be a number")
    result = float(value)
    if not math.isfinite(result):
        raise ProjectError(f"{label} must be finite")
    return result


def _integer(value: Any, label: str) -> int:
    if not _is_int(value):
        raise ProjectError(f"{label} must be an integer")
    return value


def _pair(value: Any, label: str, *, positive: bool = False) -> tuple[float, float]:
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        parsed = (_number(value, label), _number(value, label))
    elif isinstance(value, (list, tuple)) and len(value) == 2:
        parsed = (_number(value[0], label), _number(value[1], label))
    else:
        raise ProjectError(f"{label} must be a number or a two-element array")
    if positive and (parsed[0] <= 0.0 or parsed[1] <= 0.0):
        raise ProjectError(f"{label} must be greater than zero")
    return parsed


@dataclass(frozen=True)
class Fps:
    value: Fraction

    @classmethod
    def parse(cls, raw: Any) -> "Fps":
        if isinstance(raw, bool) or raw is None:
            raise ProjectError("fps must be a positive fraction")
        try:
            if isinstance(raw, int):
                value = Fraction(raw, 1)
            elif isinstance(raw, float):
                value = Fraction(str(raw)).limit_denominator(1_000_000)
            elif isinstance(raw, str):
                text = raw.strip()
                if "/" in text:
                    numerator, denominator = text.split("/", 1)
                    value = Fraction(int(numerator), int(denominator))
                elif text:
                    value = Fraction(text).limit_denominator(1_000_000)
                else:
                    raise ValueError("empty fps")
            else:
                raise ValueError("unsupported fps type")
        except (ValueError, ZeroDivisionError) as exc:
            raise ProjectError(f"invalid fps: {raw!r}") from exc
        if value <= 0:
            raise ProjectError("fps must be greater than zero")
        return cls(value)

    @property
    def seconds_per_frame(self) -> Fraction:
        return 1 / self.value

    def pts_seconds(self, frame: int) -> Fraction:
        return frame * self.seconds_per_frame

    def pts_ms(self, frame: int) -> float:
        return float(self.pts_seconds(frame) * 1000)

    def sample_index(self, frame: int, sample_rate: int) -> int:
        exact = self.pts_seconds(frame) * sample_rate
        quotient, remainder = divmod(exact.numerator, exact.denominator)
        if remainder * 2 >= exact.denominator:
            quotient += 1
        return quotient


@dataclass(frozen=True)
class Keyframe:
    frame: int
    easing: str
    values: dict[str, Any]


def _parse_property(name: str, raw: Any, label: str) -> Any:
    if name == "position":
        return _pair(raw, label)
    if name == "scale":
        return _pair(raw, label, positive=True)
    if name == "rotation":
        return _number(raw, label)
    if name == "opacity":
        value = _number(raw, label)
        if not 0.0 <= value <= 1.0:
            raise ProjectError(f"{label} must be between 0 and 1")
        return value
    raise ProjectError(f"unknown animated property {name!r}")


def _parse_keyframes(raw: Any) -> tuple[Keyframe, ...]:
    if raw is None:
        return ()
    if not isinstance(raw, list):
        raise ProjectError("keyframes must be an array")
    parsed: list[Keyframe] = []
    last_frame: int | None = None
    for index, item in enumerate(raw):
        label = f"keyframes[{index}]"
        if not isinstance(item, dict):
            raise ProjectError(f"{label} must be an object")
        if "frame" not in item:
            raise ProjectError(f"{label}.frame is required")
        frame = _integer(item["frame"], f"{label}.frame")
        if last_frame is not None and frame <= last_frame:
            raise ProjectError("keyframes must be strictly ordered by frame")
        last_frame = frame
        easing = item.get("easing", item.get("ease", "linear"))
        if easing not in EASINGS:
            raise ProjectError(f"{label}.easing must be one of {EASINGS}")
        values: dict[str, Any] = {}
        for name in ANIMATED_PROPERTIES:
            if name in item:
                values[name] = _parse_property(name, item[name], f"{label}.{name}")
        if not values:
            raise ProjectError(f"{label} must contain an animated property")
        parsed.append(Keyframe(frame, easing, values))
    if parsed:
        animated = set(parsed[0].values)
        for keyframe in parsed[1:]:
            if set(keyframe.values) != animated:
                raise ProjectError("every keyframe must provide the same animated fields")
        for keyframe in parsed:
            if any(name not in keyframe.values for name in animated):
                raise ProjectError("keyframe field is missing")
    return tuple(parsed)


def _ease(t: float, easing: str) -> float:
    if easing == "ease-in-out":
        return t * t * (3.0 - 2.0 * t)
    return t


def _interpolate(left: Any, right: Any, t: float) -> Any:
    if isinstance(left, tuple):
        return tuple(a + (b - a) * t for a, b in zip(left, right))
    return left + (right - left) * t


@dataclass(frozen=True)
class LayerState:
    position: tuple[float, float]
    scale: tuple[float, float]
    rotation: float
    opacity: float


@dataclass(frozen=True)
class Layer:
    id: str
    path: Path
    in_frame: int
    out_frame: int
    blend: str
    anchor: tuple[float, float] | None
    position: tuple[float, float]
    scale: tuple[float, float]
    rotation: float
    opacity: float
    keyframes: tuple[Keyframe, ...]

    def is_active(self, frame: int) -> bool:
        return self.in_frame <= frame <= self.out_frame

    def state_at(self, frame: int) -> LayerState:
        values: dict[str, Any] = {
            "position": self.position,
            "scale": self.scale,
            "rotation": self.rotation,
            "opacity": self.opacity,
        }
        if self.keyframes:
            if frame <= self.keyframes[0].frame:
                values.update(self.keyframes[0].values)
            elif frame >= self.keyframes[-1].frame:
                values.update(self.keyframes[-1].values)
            else:
                for left, right in zip(self.keyframes, self.keyframes[1:]):
                    if left.frame <= frame < right.frame:
                        t = (frame - left.frame) / (right.frame - left.frame)
                        t = _ease(t, left.easing)
                        for name, value in left.values.items():
                            values[name] = _interpolate(value, right.values[name], t)
                        break
        return LayerState(
            position=values["position"],
            scale=values["scale"],
            rotation=values["rotation"],
            opacity=values["opacity"],
        )


@dataclass(frozen=True)
class AudioSpec:
    path: Path
    cues: tuple[int, ...]
    threshold: float
    search_ms: float


@dataclass(frozen=True)
class Timeline:
    project_path: Path
    width: int
    height: int
    background: tuple[int, int, int, int]
    fps: Fps
    frame_count: int
    layers: tuple[Layer, ...]
    audio: AudioSpec | None

    def __post_init__(self) -> None:
        if self.frame_count <= 0:
            raise ProjectError("frame_count must be greater than zero")

    def render_frames(self) -> Iterable[int]:
        return range(self.frame_count)

    def active_layers(self, frame: int) -> Iterable[Layer]:
        return (layer for layer in self.layers if layer.is_active(frame))

    def pts_ms(self, frame: int) -> float:
        return self.fps.pts_ms(frame)


def _color(raw: Any) -> tuple[int, int, int, int]:
    if not isinstance(raw, (list, tuple)) or len(raw) not in (3, 4):
        raise ProjectError("canvas.background must be RGB or RGBA")
    result: list[int] = []
    for index, item in enumerate(raw):
        value = _integer(item, f"canvas.background[{index}]")
        if not 0 <= value <= 255:
            raise ProjectError("canvas.background values must be between 0 and 255")
        result.append(value)
    if len(result) == 3:
        result.append(255)
    return tuple(result)  # type: ignore[return-value]


def _resolve(base: Path, raw: Any, label: str) -> Path:
    if not isinstance(raw, str) or not raw:
        raise ProjectError(f"{label}.path must be a non-empty string")
    path = Path(raw)
    return path if path.is_absolute() else base / path


def _parse_layer(raw: Any, index: int, base: Path, canvas: tuple[int, int]) -> Layer:
    label = f"layers[{index}]"
    if not isinstance(raw, dict):
        raise ProjectError(f"{label} must be an object")
    layer_id = raw.get("id", f"layer-{index}")
    if not isinstance(layer_id, str) or not layer_id:
        raise ProjectError(f"{label}.id must be a non-empty string")
    layer_type = raw.get("type", "image")
    if layer_type != "image":
        raise ProjectError(f"{label}.type must be 'image'")
    for field in ("in", "out"):
        if field not in raw:
            raise ProjectError(f"{label}.{field} is required")
    in_frame = _integer(raw["in"], f"{label}.in")
    out_frame = _integer(raw["out"], f"{label}.out")
    if out_frame < in_frame:
        raise ProjectError(f"{label}.out must be greater than or equal to in")
    blend = raw.get("blend", "normal")
    if blend not in BLEND_MODES:
        raise ProjectError(f"{label}.blend must be one of {BLEND_MODES}")
    transform = raw.get("transform", {})
    if not isinstance(transform, dict):
        raise ProjectError(f"{label}.transform must be an object")
    anchor_raw = transform.get("anchor", raw.get("anchor"))
    anchor = None if anchor_raw is None else _pair(anchor_raw, f"{label}.anchor")
    position = _pair(transform.get("position", [canvas[0] / 2, canvas[1] / 2]), f"{label}.position")
    scale = _pair(transform.get("scale", [1, 1]), f"{label}.scale", positive=True)
    rotation = _number(transform.get("rotation", 0), f"{label}.rotation")
    opacity_raw = raw.get("opacity", transform.get("opacity", 1.0))
    opacity = _number(opacity_raw, f"{label}.opacity")
    if not 0.0 <= opacity <= 1.0:
        raise ProjectError(f"{label}.opacity must be between 0 and 1")
    keyframes = _parse_keyframes(raw.get("keyframes"))
    return Layer(
        id=layer_id,
        path=_resolve(base, raw.get("path"), label),
        in_frame=in_frame,
        out_frame=out_frame,
        blend=blend,
        anchor=anchor,
        position=position,
        scale=scale,
        rotation=rotation,
        opacity=opacity,
        keyframes=keyframes,
    )


def _parse_audio(raw: Any, base: Path, frame_count: int) -> AudioSpec | None:
    if raw is None:
        return None
    if not isinstance(raw, dict):
        raise ProjectError("audio must be an object")
    cues_raw = raw.get("cues")
    if not isinstance(cues_raw, list):
        raise ProjectError("audio.cues must be an array")
    cues: list[int] = []
    for index, item in enumerate(cues_raw):
        if isinstance(item, dict):
            item = item.get("frame")
        frame = _integer(item, f"audio.cues[{index}]")
        if not 0 <= frame < frame_count:
            raise ProjectError(f"cue out of range: {frame}")
        cues.append(frame)
    threshold = _number(raw.get("threshold", 0.5), "audio.threshold")
    if not 0.0 < threshold <= 1.0:
        raise ProjectError("audio.threshold must be in (0, 1]")
    search_ms = _number(raw.get("search_ms", 50.0), "audio.search_ms")
    if search_ms < 0:
        raise ProjectError("audio.search_ms must be non-negative")
    return AudioSpec(
        path=_resolve(base, raw.get("path"), "audio"),
        cues=tuple(cues),
        threshold=threshold,
        search_ms=search_ms,
    )


def load_timeline(path: str | Path) -> Timeline:
    project_path = Path(path)
    try:
        with project_path.open("r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except OSError as exc:
        raise ProjectError(f"cannot read project {project_path}: {exc}") from exc
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise ProjectError(f"invalid project JSON: {exc}") from exc
    if not isinstance(raw, dict):
        raise ProjectError("project root must be an object")
    fps = Fps.parse(raw.get("fps"))
    frame_count = _integer(raw.get("frame_count"), "frame_count")
    if frame_count <= 0:
        raise ProjectError("frame_count must be greater than zero")
    canvas = raw.get("canvas")
    if not isinstance(canvas, dict):
        raise ProjectError("canvas must be an object")
    width = _integer(canvas.get("width"), "canvas.width")
    height = _integer(canvas.get("height"), "canvas.height")
    if width <= 0 or height <= 0:
        raise ProjectError("canvas dimensions must be greater than zero")
    background = _color(canvas.get("background", [0, 0, 0, 0]))
    layers_raw = raw.get("layers")
    if not isinstance(layers_raw, list):
        raise ProjectError("layers must be an array")
    layers = tuple(
        _parse_layer(item, index, project_path.parent, (width, height))
        for index, item in enumerate(layers_raw)
    )
    if len({layer.id for layer in layers}) != len(layers):
        raise ProjectError("layer ids must be unique")
    audio = _parse_audio(raw.get("audio"), project_path.parent, frame_count)
    return Timeline(
        project_path=project_path,
        width=width,
        height=height,
        background=background,
        fps=fps,
        frame_count=frame_count,
        layers=layers,
        audio=audio,
    )
