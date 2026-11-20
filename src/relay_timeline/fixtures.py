"""Deterministic local fixture generation.

All media is synthesized in-process (no external assets): moving
synthetic RGB frames, a sine-based PCM track, event markers, HLS/DASH
manifests and MPEG-TS segments.  Timestamps are written in raw 90 kHz
ticks modulo a configurable 33-bit-style modulus so wrap scenarios are
exercised exactly as on a real edge node.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av
import numpy as np


@dataclass(slots=True)
class FixtureSpec:
    out_dir: Path
    fmt: str = "hls"
    duration: float = 12.0
    target_bitrate: int = 1_800_000
    drift_ppm: float = 0.0
    wrap_at: int = 1 << 33
    vfr: bool = False
    fault: str | None = None
    seed: int = 42
    width: int = 640
    height: int = 360
    fps: int = 30
    sample_rate: int = 48_000
    seg_seconds: float = 2.0


@dataclass(slots=True)
class FixtureResult:
    manifest: Path
    scenario: dict
    events: list[dict] = field(default_factory=list)


# Deterministic x264 encoder startup delay in 90 kHz ticks measured for the
# stream configuration used by _mux_segment (two-frame reorder buffer).
ENCODER_DELAY_TICKS = 6750


def _make_frame(idx: int, w: int, h: int, seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed + idx)
    rgb = np.zeros((h, w, 3), dtype=np.uint8)
    rgb[:, :, 0] = np.linspace(20, 220, w, dtype=np.uint8)[None, :]
    rgb[:, :, 1] = np.linspace(20, 200, h, dtype=np.uint8)[:, None]
    x = (idx * 9) % max(1, w - 80)
    y = (idx * 5) % max(1, h - 60)
    rgb[y : y + 60, x : x + 80] = (200, 60, 90)
    noise = rng.integers(0, 12, size=(h, w, 1), dtype=np.int16)
    return np.clip(rgb.astype(np.int16) + noise, 0, 255).astype(np.uint8)


def _segment_frame_ticks(spec: FixtureSpec, segment_ticks: int) -> list[int]:
    """Whole-tick local PTS for one segment, always starting at 0.

    Frames are emitted only while the *encoded* PTS stays inside the
    segment.  The x264 encoder adds a deterministic startup delay
    (:data:`ENCODER_DELAY_TICKS`), so generation stops early enough that
    the final output frame never crosses the next segment boundary.  The
    VFR cycle (9000 ticks) divides the 2 s segment (180000 ticks) exactly.
    """
    base = 90_000 // spec.fps
    pattern = (base, base + base // 4, base - base // 4) if spec.vfr \
        else (base,)
    limit = segment_ticks - ENCODER_DELAY_TICKS
    ticks: list[int] = []
    pos = 0
    k = 0
    while pos < limit:
        ticks.append(pos)
        pos += pattern[k % len(pattern)]
        k += 1
    return ticks


def _make_audio(spec: FixtureSpec, n_samples: int) -> np.ndarray:
    t = np.arange(n_samples) / spec.sample_rate
    wave = 0.22 * np.sin(2 * np.pi * 440 * t)
    wave += 0.08 * np.sin(2 * np.pi * 880 * t)
    return (wave * 32767).astype(np.int16)


def _mux_segment(
    path: Path,
    frames: list[tuple[np.ndarray, int]],
    audio: np.ndarray,
    audio_start_pts: int,
    spec: FixtureSpec,
) -> None:
    container = av.open(str(path), mode="w", format="mpegts")
    # No fixed frame rate: each frame carries its own PTS so VFR presentation
    # times survive encoding instead of being rewritten to a CFR grid.
    vs = container.add_stream("h264")
    vs.width, vs.height = spec.width, spec.height
    vs.pix_fmt = "yuv420p"
    vs.time_base = Fraction(1, 90_000)
    video_bps = int(spec.target_bitrate * 0.83)
    vs.options = {
        "preset": "veryfast",
        "b": str(video_bps),
        "maxrate": str(int(video_bps * 1.06)),
        "bufsize": str(video_bps * 2),
        "x264-params": "keyint=30:min-keyint=30:scenecut=0",
    }
    a_s = container.add_stream("aac", rate=spec.sample_rate)
    a_s.time_base = Fraction(1, spec.sample_rate)
    # Audio runs on its own node clock: a linear ppm offset is applied to
    # audio PTS only, leaving the video/VFR timeline untouched.  The audio
    # assembler regresses audio PTS against the video reference to recover
    # this drift in ppm.
    audio_stretch = 1.0 + spec.drift_ppm / 1_000_000.0
    for rgb, raw_pts in frames:
        vf = av.VideoFrame.from_ndarray(rgb, format="rgb24")
        vf.pts = raw_pts
        vf.time_base = Fraction(1, 90_000)
        for pkt in vs.encode(vf):
            container.mux(pkt)
    block = 1024
    for off in range(0, len(audio), block):
        chunk = audio[off : off + block]
        af = av.AudioFrame.from_ndarray(
            chunk.reshape(1, -1), format="s16", layout="mono"
        )
        af.sample_rate = spec.sample_rate
        af.pts = int(round((audio_start_pts + off) * audio_stretch))
        af.time_base = Fraction(1, spec.sample_rate)
        for pkt in a_s.encode(af):
            container.mux(pkt)
    for pkt in vs.encode(None):
        container.mux(pkt)
    for pkt in a_s.encode(None):
        container.mux(pkt)
    container.close()


def _write_hls(out: Path, n: int, dur: float, bitrate: int) -> Path:
    lines = [
        "#EXTM3U", "#EXT-X-VERSION:3",
        f"#X-DECLARED-BITRATE:{bitrate}",
        f"#EXT-X-TARGETDURATION:{int(round(dur))}",
        "#EXT-X-MEDIA-SEQUENCE:0",
    ]
    for i in range(n):
        lines.append(f"#EXTINF:{dur:.3f},")
        lines.append(f"seg-{i}.ts")
    lines.append("#EXT-X-ENDLIST")
    manifest = out / "playlist.m3u8"
    manifest.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return manifest


def _write_dash(out: Path, n: int, dur: float, bitrate: int) -> Path:
    duration_ticks = int(dur * 90_000)
    seg_timeline = "".join(f'<S d="{duration_ticks}"/>' for _ in range(n))
    xml = (
        '<?xml version="1.0" encoding="UTF-8"?>'
        f'<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" '
        f'mediaPresentationDuration="PT{dur * n:.3f}S">'
        '<Period><AdaptationSet mimeType="video/mp2t">'
        f'<Representation id="v0" bandwidth="{bitrate}">'
        '<SegmentTemplate timescale="90000" startNumber="0" '
        'media="seg-$Number$.ts">'
        f"<SegmentTimeline>{seg_timeline}</SegmentTimeline>"
        "</SegmentTemplate></Representation></AdaptationSet></Period></MPD>"
    )
    manifest = out / "manifest.mpd"
    manifest.write_text(xml, encoding="utf-8")
    return manifest


def build_fixture(spec: FixtureSpec) -> FixtureResult:
    """Synthesize every file described by ``spec``; return manifest path."""
    spec.out_dir.mkdir(parents=True, exist_ok=True)
    modulus = spec.wrap_at
    boundary = int(spec.seg_seconds * 90_000)
    # The first segment is written near the top of the 33-bit modulus so
    # its last PTS sits just below ``modulus``; the second segment starts
    # at a low PTS, which the decoder's WrapUnroller expands as a wrap.
    # Within one MPEG-TS container PTS stays monotonic (a muxer requirement).
    start_raw = modulus - boundary - 3000
    n_segments = int(round(spec.duration / spec.seg_seconds))
    samples_per = int(spec.sample_rate * spec.seg_seconds)
    events: list[dict] = []
    for i in range(n_segments):
        local_ticks = _segment_frame_ticks(spec, boundary)
        frame_offset = i * len(_segment_frame_ticks(spec, boundary))
        # Every segment is muxed on a local clock starting at 0, so the
        # encoder applies the identical reorder delay to each and adjacent
        # segments' first decoded PTS differ by exactly one boundary once
        # the composer restores the global (sequence-based) position.
        # seg-0 is later lifted near the modulus to trigger the wrap.
        def local_pts(tick: int, seg_index: int = i) -> int:
            if seg_index == 0:
                return (start_raw + tick) % modulus
            return tick % modulus

        frames = [
            (_make_frame(frame_offset + k,
                         spec.width, spec.height, spec.seed),
             local_pts(tick))
            for k, tick in enumerate(local_ticks)
        ]
        audio = _make_audio(spec, samples_per)
        _mux_segment(spec.out_dir / f"seg-{i}.ts", frames, audio,
                     i * samples_per, spec)
    events = [
        {"time_s": 1.0, "label": "PROGRAM_START"},
        {"time_s": 4.5, "label": "SCENE_MARKER"},
        {"time_s": 9.0, "label": "AD_BREAK"},
    ]
    (spec.out_dir / "events.json").write_text(
        json.dumps(events, indent=2), encoding="utf-8"
    )
    fault_segment = n_segments // 2
    if spec.fault == "truncated":
        p = spec.out_dir / f"seg-{fault_segment}.ts"
        p.write_bytes(p.read_bytes()[: max(1, p.stat().st_size * 2 // 5)])
    elif spec.fault == "zero":
        (spec.out_dir / f"seg-{fault_segment}.ts").write_bytes(b"")
    elif spec.fault == "missing":
        (spec.out_dir / f"seg-{fault_segment}.ts").unlink()
    if spec.fmt == "hls":
        manifest = _write_hls(spec.out_dir, n_segments,
                              spec.seg_seconds, spec.target_bitrate)
    else:
        manifest = _write_dash(spec.out_dir, n_segments,
                               spec.seg_seconds, spec.target_bitrate)
    scenario = {
        "format": spec.fmt,
        "modulus": modulus,
        "drift_ppm": spec.drift_ppm,
        "vfr": spec.vfr,
        "fault": spec.fault,
        "fault_segment": fault_segment if spec.fault else None,
        "target_bitrate": spec.target_bitrate,
        "segment_count": n_segments,
        "nominal_fps": spec.fps,
        "sample_rate": spec.sample_rate,
        "width": spec.width,
        "height": spec.height,
        # seg-0 begins ``boundary + 3000`` ticks below the modulus and spans
        # a full boundary, so it always crosses the modulus once.
        "wrap_expected": True,
    }
    (spec.out_dir / "scenario.json").write_text(
        json.dumps(scenario, indent=2), encoding="utf-8"
    )
    return FixtureResult(manifest=manifest, scenario=scenario, events=events)
