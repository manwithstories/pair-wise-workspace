"""Timeline (EDL) model, validation, and keyframe sampling."""

import json
from dataclasses import dataclass, field

from .easing import get_easing
from .errors import BlendModeError, TimelineError

BLEND_MODES = (
    "normal", "multiply", "screen", "overlay",
    "add", "difference", "darken", "lighten",
)

_KEYFRAME_FIELDS = ("x", "y", "scale_x", "scale_y", "rotate", "opacity")


@dataclass
class Keyframe:
    t: float
    values: dict
    easing: object = None  # easing spec applying to the segment after this key


@dataclass
class Layer:
    id: str
    asset: str
    start: float
    duration: float
    blend: str = "normal"
    opacity: float = 1.0
    anchor: tuple = (0.5, 0.5)
    width: int = 0   # declared source size; 0 means "trust the asset"
    height: int = 0
    keyframes: list = field(default_factory=list)

    @property
    def end(self):
        return self.start + self.duration

    def active(self, t):
        return self.start <= t < self.end


@dataclass
class AudioClip:
    asset: str
    start: float = 0.0
    gain: float = 1.0


@dataclass
class Timeline:
    width: int
    height: int
    duration: float
    layers: list
    audio: list


def _num(value, location, allow_zero=True):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise TimelineError("expected a number, got %r" % (value,), location)
    value = float(value)
    if allow_zero:
        ok = value >= 0.0
    else:
        ok = value > 0.0
    if not ok:
        raise TimelineError(
            "must be %s than zero, got %r"
            % ("greater" if not allow_zero else "not less", value), location)
    return value


def _any_num(value, location):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise TimelineError("expected a number, got %r" % (value,), location)
    return float(value)


def _parse_keyframes(raw, layer_start, layer_end, location):
    if raw is None:
        raw = []
    if not isinstance(raw, list):
        raise TimelineError("keyframes must be a list", location)
    keys = []
    for i, item in enumerate(raw):
        loc = "%s.keyframes[%d]" % (location, i)
        if not isinstance(item, dict):
            raise TimelineError("keyframe must be an object", loc)
        if "t" not in item:
            raise TimelineError("missing required field 't'", loc)
        t = _num(item["t"], loc + ".t")
        if not layer_start <= t <= layer_end:
            raise TimelineError(
                "keyframe t=%g lies outside the layer interval [%g, %g]"
                % (t, layer_start, layer_end), loc + ".t")
        values = {}
        for name in _KEYFRAME_FIELDS:
            if name in item:
                values[name] = _any_num(item[name], "%s.%s" % (loc, name))
        if "opacity" in values and not 0.0 <= values["opacity"] <= 1.0:
            raise TimelineError("opacity must be within [0, 1]", loc + ".opacity")
        easing = item.get("easing", "linear")
        get_easing(easing, location=loc + ".easing")  # validate eagerly
        keys.append(Keyframe(t=t, values=values, easing=easing))
    keys.sort(key=lambda k: k.t)
    for prev, cur in zip(keys, keys[1:]):
        if cur.t == prev.t:
            raise TimelineError(
                "duplicate keyframe time t=%g" % cur.t, location + ".keyframes")
    return keys


def _parse_layer(raw, index):
    loc = "layers[%d]" % index
    if not isinstance(raw, dict):
        raise TimelineError("layer must be an object", loc)
    for name in ("id", "asset", "start", "duration"):
        if name not in raw:
            raise TimelineError("missing required field %r" % name, loc)
    layer_id = raw["id"]
    if not isinstance(layer_id, str) or not layer_id:
        raise TimelineError("layer id must be a non-empty string", loc + ".id")
    asset = raw["asset"]
    if not isinstance(asset, str) or not asset:
        raise TimelineError("asset must be a non-empty string", loc + ".asset")
    start = _num(raw["start"], loc + ".start")
    duration = _num(raw["duration"], loc + ".duration", allow_zero=False)
    blend = raw.get("blend", "normal")
    if blend not in BLEND_MODES:
        raise BlendModeError(
            "unknown blend mode %r (expected one of %s)"
            % (blend, list(BLEND_MODES)), loc + ".blend")
    opacity = raw.get("opacity", 1.0)
    opacity = _num(opacity, loc + ".opacity")
    if not 0.0 <= opacity <= 1.0:
        raise TimelineError("opacity must be within [0, 1]", loc + ".opacity")
    anchor = raw.get("anchor", [0.5, 0.5])
    if (not isinstance(anchor, (list, tuple)) or len(anchor) != 2
            or not all(isinstance(v, (int, float)) for v in anchor)):
        raise TimelineError("anchor must be [ax, ay] of numbers", loc + ".anchor")
    width = int(raw.get("width", 0))
    height = int(raw.get("height", 0))
    if width < 0 or height < 0:
        raise TimelineError("declared size must not be negative", loc)
    keyframes = _parse_keyframes(
        raw.get("keyframes"), start, start + duration, loc)
    return Layer(
        id=layer_id, asset=asset, start=start, duration=duration,
        blend=blend, opacity=opacity, anchor=(float(anchor[0]), float(anchor[1])),
        width=width, height=height, keyframes=keyframes)


def _parse_audio(raw, index):
    loc = "audio[%d]" % index
    if not isinstance(raw, dict):
        raise TimelineError("audio clip must be an object", loc)
    if "asset" not in raw:
        raise TimelineError("missing required field 'asset'", loc)
    asset = raw["asset"]
    if not isinstance(asset, str) or not asset:
        raise TimelineError("asset must be a non-empty string", loc + ".asset")
    start = _num(raw.get("start", 0.0), loc + ".start")
    gain = _num(raw.get("gain", 1.0), loc + ".gain")
    return AudioClip(asset=asset, start=start, gain=gain)


def validate(data):
    """Validate a decoded JSON document and return a Timeline."""
    if not isinstance(data, dict):
        raise TimelineError("timeline root must be an object")
    canvas = data.get("canvas")
    if not isinstance(canvas, dict):
        raise TimelineError("missing required object 'canvas'", "canvas")
    width = canvas.get("width")
    height = canvas.get("height")
    for name, val in (("width", width), ("height", height)):
        if not isinstance(val, int) or isinstance(val, bool) or val <= 0:
            raise TimelineError(
                "canvas %s must be a positive integer, got %r" % (name, val),
                "canvas." + name)
    duration = data.get("duration")
    if duration is None:
        raise TimelineError("missing required field 'duration'", "duration")
    duration = _num(duration, "duration", allow_zero=False)
    raw_layers = data.get("layers")
    if not isinstance(raw_layers, list) or not raw_layers:
        raise TimelineError("'layers' must be a non-empty list", "layers")
    layers = [_parse_layer(item, i) for i, item in enumerate(raw_layers)]
    seen = set()
    for layer in layers:
        if layer.id in seen:
            raise TimelineError("duplicate layer id %r" % layer.id, "layers")
        seen.add(layer.id)
        if layer.end > duration + 1e-9:
            raise TimelineError(
                "layer %r ends at %g, beyond timeline duration %g"
                % (layer.id, layer.end, duration), "layers")
    raw_audio = data.get("audio", [])
    if not isinstance(raw_audio, list):
        raise TimelineError("'audio' must be a list", "audio")
    audio = [_parse_audio(item, i) for i, item in enumerate(raw_audio)]
    for i, clip in enumerate(audio):
        if clip.start > duration + 1e-9:
            raise TimelineError(
                "audio clip starts at %g, beyond timeline duration %g"
                % (clip.start, duration), "audio[%d].start" % i)
    return Timeline(width=width, height=height, duration=duration,
                    layers=layers, audio=audio)


def load_timeline(path):
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except FileNotFoundError:
        raise TimelineError("timeline file not found: %s" % path)
    except json.JSONDecodeError as exc:
        raise TimelineError("timeline is not valid JSON: %s" % exc)
    return validate(data)


_DEFAULTS = {"x": 0.0, "y": 0.0, "scale_x": 1.0, "scale_y": 1.0,
             "rotate": 0.0, "opacity": 1.0}


def sample_keyframes(keyframes, t):
    """Sample animated values at local time t (seconds, layer-relative)."""
    if not keyframes:
        return dict(_DEFAULTS)
    base = dict(_DEFAULTS)
    seg_start = 0.0
    seg_easing = "linear"
    nxt = None
    for key in keyframes:
        if key.t <= t:
            base.update(key.values)
            seg_start = key.t
            seg_easing = key.easing
        else:
            nxt = key
            break
    if nxt is None or nxt.t == seg_start:
        return base
    u = (t - seg_start) / (nxt.t - seg_start)
    eased = get_easing(seg_easing)(min(max(u, 0.0), 1.0))
    out = dict(base)
    for name, target in nxt.values.items():
        out[name] = base.get(name, _DEFAULTS[name]) + (
            target - base.get(name, _DEFAULTS[name])) * eased
    return out
