"""Generate deterministic, local-only demo media with injected PTS defects."""

from __future__ import annotations

from fractions import Fraction
from pathlib import Path
import av
import numpy as np


WIDTH = 320
HEIGHT = 180


def _template_stream(output: av.container.OutputContainer, source: av.stream.Stream):
    clone = getattr(output, "add_stream_from_template", None)
    if clone is not None:
        return clone(source)
    return output.add_stream(template=source)


def _round_div(numerator: int, denominator: int) -> int:
    return (numerator + denominator // 2) // denominator


def _frame_image(index: int) -> av.VideoFrame:
    image = np.zeros((HEIGHT, WIDTH, 3), dtype=np.uint8)
    image[:, :, 0] = (index * 11) % 255
    image[:, :, 1] = 28
    image[:, :, 2] = 96
    x = (index * 9) % (WIDTH - 56)
    y = 40 + ((index * 3) % 60)
    image[y:y + 44, x:x + 52] = (245, 210, 40)
    image[12:40, 16:120] = (40, 180, 220)
    return av.VideoFrame.from_ndarray(image, format="rgb24")


def _encode_video(
    path: Path,
    *,
    frame_count: int,
    fps: Fraction,
    gop: int,
    all_i: bool = False,
    raw: bool = False,
) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    output = av.open(str(path), "w", format="h264" if raw else "mp4")
    try:
        stream = output.add_stream("libx264", rate=fps)
        stream.width = WIDTH
        stream.height = HEIGHT
        stream.pix_fmt = "yuv420p"
        bframes = 0 if all_i else 2
        stream.options = {
            "preset": "veryfast",
            "g": str(1 if all_i else gop),
            "bf": str(bframes),
            "x264-params": (
                f"bframes={bframes}:b-pyramid=none:scenecut=0:"
                f"keyint={1 if all_i else gop}:min-keyint={1 if all_i else gop}:"
                f"open-gop=0"
            ),
        }
        for index in range(frame_count):
            frame = _frame_image(index)
            frame.pts = index
            for packet in stream.encode(frame):
                output.mux(packet)
        for packet in stream.encode(None):
            output.mux(packet)
    finally:
        output.close()


def _source_pts_slots(source: Path) -> tuple[list[int], int, Fraction]:
    with av.open(str(source)) as container:
        stream = container.streams.video[0]
        time_base = Fraction(stream.time_base)
        packet_pts = sorted(
            int(packet.pts) for packet in container.demux(stream)
            if packet.size and packet.pts is not None
        )
    return packet_pts, len(packet_pts), time_base


def _anomalous_values(frame_count: int, interval: int) -> list[int]:
    values = [index * interval for index in range(frame_count)]
    if frame_count < 4:
        return values

    def at(numerator: int, denominator: int) -> int:
        position = frame_count * numerator // denominator
        return min(frame_count - 1, max(1, position))

    duplicates = [at(13, 100), at(14, 100), at(27, 100), at(40, 100)]
    rollbacks = [at(33, 100), at(46, 100)]
    rounding = [at(52, 100), at(53, 100), at(54, 100)]
    jump_position = at(55, 100)
    after_rounding = [at(62, 100), at(66, 100), at(70, 100), at(74, 100)]
    late_duplicates = [at(80, 100), at(81, 100), at(87, 100)]

    for position in duplicates + late_duplicates:
        values[position] = values[position - 1]
    for position in rollbacks:
        values[position] = values[position - 1] - interval * 7
    for position in rounding:
        values[position] = values[position - 1] + 1
    for index in range(jump_position, frame_count):
        values[index] += interval * 30
    for position in after_rounding:
        values[position] = values[position - 1] + 1
    return values


def _remux_with_pts(
    source: Path,
    destination: Path,
    values: list[int],
    *,
    fmt: str = "mp4",
    dts_shift: int = 0,
) -> None:
    destination.parent.mkdir(parents=True, exist_ok=True)
    with av.open(str(source)) as incoming:
        input_stream = incoming.streams.video[0]
        packet_pts = sorted(
            int(packet.pts) for packet in incoming.demux(input_stream)
            if packet.size and packet.pts is not None
        )
        slot_by_pts = {pts: slot for slot, pts in enumerate(packet_pts)}
        incoming.seek(0)
        options = {"movflags": "+faststart"} if fmt == "mp4" else {}
        output = av.open(str(destination), "w", format=fmt, options=options)
        try:
            output_stream = _template_stream(output, input_stream)
            for packet in incoming.demux(input_stream):
                if not packet.size:
                    continue
                if packet.pts is not None:
                    slot = slot_by_pts[int(packet.pts)]
                    packet.pts = values[min(slot, len(values) - 1)]
                if packet.dts is not None and dts_shift:
                    packet.dts -= dts_shift
                packet.stream = output_stream
                output.mux(packet)
        finally:
            output.close()


def _remux_vfr(source: Path, destination: Path, frame_count: int, interval: int) -> None:
    deltas = [interval, interval + interval // 4,
              interval + interval // 2, interval * 2]
    values = [0]
    for index in range(1, frame_count):
        values.append(values[-1] + deltas[index % len(deltas)])
    _remux_with_pts(source, destination, values)


def _remux_timebase(source: Path, destination: Path, time_base: Fraction) -> None:
    with av.open(str(source)) as incoming:
        input_stream = incoming.streams.video[0]
        output = av.open(
            str(destination),
            "w",
            format="mp4",
            options={"video_track_timescale": str(time_base.denominator)},
        )
        try:
            output_stream = _template_stream(output, input_stream)
            for packet in incoming.demux(input_stream):
                if not packet.size:
                    continue
                packet.stream = output_stream
                output.mux(packet)
        finally:
            output.close()


def _empty_mp4(path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    output = av.open(str(path), "w", format="mp4")
    try:
        output.add_stream("libx264", rate=25)
        output.start_encoding()
    finally:
        output.close()


def _truncate_copy(source: Path, destination: Path) -> None:
    data = source.read_bytes()
    destination.write_bytes(data[:max(1, len(data) * 4 // 5)])


def make_fixtures(
    out: str | Path,
    *,
    fps: Fraction = Fraction(25, 1),
    seconds: Fraction = Fraction(6, 1),
    gop: int = 12,
) -> list[Path]:
    """Encode the fixture suite and return every generated path."""

    if fps <= 0 or seconds <= 0 or gop < 1:
        raise ValueError("fps and seconds must be positive and gop must be >= 1")
    directory = Path(out)
    directory.mkdir(parents=True, exist_ok=True)
    frame_count = max(1, int(round(seconds * fps)))

    clean = directory / "clean.mp4"
    broken = directory / "broken.mp4"
    broken_mkv = directory / "broken.mkv"
    raw = directory / "raw.h264"
    single = directory / "single_frame.mp4"
    all_i = directory / "all_i.mp4"
    vfr = directory / "vfr.mp4"
    tb90000 = directory / "tb90000.mp4"
    truncated = directory / "truncated.mp4"
    empty = directory / "empty.mp4"

    _encode_video(clean, frame_count=frame_count, fps=fps, gop=gop)
    packet_pts, count, time_base = _source_pts_slots(clean)
    del packet_pts
    interval = max(1, _round_div(
        time_base.denominator * fps.denominator,
        time_base.numerator * fps.numerator,
    ))
    anomalous = _anomalous_values(count, interval)
    _remux_with_pts(clean, broken, anomalous, dts_shift=interval * 12)
    _remux_with_pts(
        clean, broken_mkv, anomalous, fmt="matroska", dts_shift=interval * 12
    )
    _encode_video(raw, frame_count=frame_count, fps=fps, gop=gop, raw=True)
    _encode_video(single, frame_count=1, fps=fps, gop=gop)
    _encode_video(all_i, frame_count=frame_count, fps=fps, gop=1, all_i=True)
    _remux_vfr(clean, vfr, count, interval)
    _remux_timebase(clean, tb90000, Fraction(1, 90000))
    _truncate_copy(broken, truncated)
    _empty_mp4(empty)

    return [
        clean, broken, broken_mkv, raw, single, all_i, vfr, tb90000, truncated, empty
    ]
