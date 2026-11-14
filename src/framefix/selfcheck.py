"""Built-in end-to-end checks used by ``framefix selfcheck``."""

from __future__ import annotations

import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

import av
import numpy as np

from .compat import add_stream_like
from .errors import UnsupportedCodecError
from .models import PacketMeta, RepairEntry
from .reorder import ReorderBuffer
from .repair import repair_media
from .report import analyze_media
from .synth import encode_clip, synth_many


@dataclass(slots=True)
class Check:
    name: str
    passed: bool
    detail: str


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def _faststart(source: str, target: str) -> None:
    with av.open(source) as input_container, av.open(
        target, "w", options={"movflags": "+faststart"}
    ) as output_container:
        streams = {
            stream.index: add_stream_like(output_container, stream)
            for stream in input_container.streams
        }
        for packet in input_container.demux():
            if not packet.size:
                continue
            packet.stream = streams[packet.stream_index]
            output_container.mux(packet)


def _truncate_at(source: str, target: str, fraction: float) -> None:
    data = Path(source).read_bytes()
    Path(target).write_bytes(data[: int(len(data) * fraction)])


def _zero_frame_clip(source: str, target: str) -> None:
    data = Path(source).read_bytes()
    mdat = data.find(b"mdat")
    _require(mdat > 0, "faststart fixture has no mdat atom")
    Path(target).write_bytes(data[: mdat + 4])


def _unsupported_clip(target: str) -> None:
    with av.open(target, "w") as container:
        stream = container.add_stream("mpeg4", rate=10)
        stream.width = 64
        stream.height = 64
        stream.pix_fmt = "yuv420p"
        image = np.full((64, 64, 3), 80, dtype=np.uint8)
        frame = av.VideoFrame.from_ndarray(image, format="rgb24")
        frame.pts = 0
        for packet in stream.encode(frame):
            container.mux(packet)
        for packet in stream.encode():
            container.mux(packet)


def _run_checks(work: Path) -> list[Check]:
    checks: list[Check] = []

    def run(name: str, function: Callable[[], str]) -> None:
        try:
            detail = function()
            checks.append(Check(name, True, detail))
        except Exception as exc:
            checks.append(Check(name, False, f"{type(exc).__name__}: {exc}"))

    def pipeline_check() -> str:
        fixtures = work / "fixtures"
        synth_many(str(fixtures), ["clean", "bframes", "broken"], frames=72, width=160, height=90)
        clean = analyze_media(str(fixtures / "clean.mp4"))
        bframes = analyze_media(str(fixtures / "bframes.mp4"))
        broken = analyze_media(str(fixtures / "broken.mp4"))
        _require(clean.status_code == 0 and clean.plan.fixed_entries == 0, "clean scan unhealthy")
        _require(bframes.status_code == 0, "bframes scan failed")
        _require(bframes.classifier.b_frames > 0, "bframes fixture has no B frames")
        _require(bframes.classifier.type_mismatch == 0, "bframes type mismatch")
        _require(broken.plan.fixed_entries > 0, "broken fixture has no timestamp anomalies")
        return f"clean={clean.plan.decoded_frames} b={bframes.classifier.b_frames} broken={broken.plan.fixed_entries}"

    def repair_check() -> str:
        source = work / "fixtures" / "broken.mp4"
        output = work / "out" / "broken.fixed.mp4"
        report = work / "out" / "broken.report.json"
        outcome = repair_media(str(source), str(output), str(report))
        _require(outcome.exit_code == 0, f"repair exit {outcome.exit_code}")
        _require(bool(outcome.report["pixel_digest_match"]), "pixel digest mismatch")
        _require(output.exists() and report.exists(), "repair artifacts missing")
        rescan = analyze_media(str(output))
        _require(rescan.status_code == 0, "repaired rescan failed")
        _require(rescan.plan.fixed_entries == 0, "repaired file still has timestamp anomalies")
        _require(rescan.max_jump_ms <= 250, "repaired jump remains above threshold")
        return f"passes={outcome.passes} frames={rescan.plan.decoded_frames}"

    def pure_i_check() -> str:
        path = work / "pure_i.mp4"
        encode_clip(str(path), frames=30, width=160, height=90, bframes=0, gop=1)
        result = analyze_media(str(path))
        _require(result.status_code == 0, f"pure I exit {result.status_code}")
        _require(result.classifier.i_frames == 30, "pure I count mismatch")
        _require(result.classifier.b_frames == 0, "pure I contains B frames")
        return f"i={result.classifier.i_frames} gops={result.classifier.gop_count}"

    def truncation_check() -> str:
        source = work / "fixtures" / "bframes.mp4"
        fast = work / "bframes.fast.mp4"
        truncated = work / "truncated.mp4"
        _faststart(str(source), str(fast))
        _truncate_at(str(fast), str(truncated), 0.55)
        result = analyze_media(str(truncated))
        _require(result.status_code == 3, f"truncated exit {result.status_code}")
        _require(result.plan.decoded_frames > 0, "truncated file yielded no partial frames")
        return f"partial_frames={result.plan.decoded_frames}"

    def zero_frame_check() -> str:
        source = work / "bframes.fast.mp4"
        zero = work / "zero.mp4"
        _zero_frame_clip(str(source), str(zero))
        result = analyze_media(str(zero))
        _require(result.status_code == 3, f"zero-frame exit {result.status_code}")
        _require(result.plan.decoded_frames == 0, "zero-frame fixture decoded frames")
        return "frames=0"

    def unsupported_check() -> str:
        path = work / "unsupported.mp4"
        _unsupported_clip(str(path))
        try:
            analyze_media(str(path))
        except UnsupportedCodecError:
            return "mpeg4 rejected with exit 2"
        raise AssertionError("unsupported codec was not rejected")

    def reorder_check() -> str:
        buffer = ReorderBuffer(window=3)
        times = [(0, 0), (2, 2), (2, 3), (1, 1)]
        for dts, pts in times:
            packet = PacketMeta(
                seq=dts + pts,
                stream_index=0,
                pts=pts,
                dts=dts,
                duration=1,
                time_base=__import__("fractions").Fraction(1, 10),
                is_keyframe=False,
                is_discard=False,
                size=1,
            )
            list(buffer.push(packet))
        list(buffer.flush())
        _require(buffer.summary.duplicate_dts == 1, "duplicate DTS not detected")
        _require(buffer.summary.regressed_dts == 1, "regressed DTS not detected")
        _require(buffer.summary.max_buffer_depth <= 3, "reorder window exceeded")
        return "duplicate=1 regressed=1 bounded"

    run("streaming-pipeline", pipeline_check)
    run("repair-and-verify", repair_check)
    run("pure-i-frames", pure_i_check)
    run("truncated-input", truncation_check)
    run("zero-frame-input", zero_frame_check)
    run("unsupported-codec", unsupported_check)
    run("reorder-window", reorder_check)
    return checks


def run_selfcheck() -> int:
    with tempfile.TemporaryDirectory(prefix="framefix-selfcheck-") as temporary:
        checks = _run_checks(Path(temporary))
    for check in checks:
        status = "PASS" if check.passed else "FAIL"
        print(f"[{status}] {check.name}: {check.detail}")
    passed = sum(check.passed for check in checks)
    print(f"selfcheck: {passed}/{len(checks)} passed")
    return 0 if passed == len(checks) else 3
