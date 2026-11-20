"""Pipeline orchestration: parse -> transform -> verify, staged on disk.

Each stage writes a JSON record holding:

* ``input_sha256`` - digest of everything the stage consumed,
* ``output_sha256`` - digest of what it produced,
* the output payload itself.

On ``--resume`` a stage is reused only when its recorded input digest equals the
current one.  A changed input raises ``stage-hash-mismatch`` instead of silently
mixing stale artefacts with fresh ones.

Failure isolation happens at the segment level: a segment that cannot be
archived is recorded with its locatable error and the remaining segments still
archive, so one damaged GOP never costs a whole master.
"""

from __future__ import annotations

import os
import shutil
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

import av

from .audio import AudioSegment, AudioSettings, extract_segments
from .color import ColorRecord, resolve as resolve_color
from .errors import MediaError, bridge
from .manifest import (
    Manifest,
    SegmentRecord,
    StageRecord,
    keyframe_digest,
    stage_input_digest,
)
from .probe import ProbeResult, StreamProbe, probe
from .timeline import Segment, Timeline, Track, contiguous_segments
from .util import atomic_write_json, iter_packets, load_json, quiet_libav, sha256_file

STAGE_DIR = "stages"
SEGMENT_DIR = "segments"


@dataclass(slots=True)
class Limits:
    max_file_bytes: int = 512 * 1024 * 1024
    max_segment_seconds: float = 120.0
    jobs: int = 2

    def validate(self) -> None:
        if self.max_file_bytes <= 0:
            raise MediaError("limit-exceeded", "max file size must be positive", stage="archive")
        if self.max_segment_seconds <= 0:
            raise MediaError("limit-exceeded", "segment duration must be positive", stage="archive")
        if not 1 <= self.jobs <= 8:
            raise MediaError(
                "limit-exceeded", "parallel jobs must be between 1 and 8", stage="archive"
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "max_file_bytes": self.max_file_bytes,
            "max_segment_seconds": self.max_segment_seconds,
            "jobs": self.jobs,
        }


@dataclass(slots=True)
class StageStore:
    """On-disk stage records; the basis of safe resume."""

    root: Path
    resume: bool = False

    def prepare(self) -> None:
        if not self.resume and self.root.exists():
            shutil.rmtree(self.root)
        (self.root / STAGE_DIR).mkdir(parents=True, exist_ok=True)
        (self.root / SEGMENT_DIR).mkdir(parents=True, exist_ok=True)

    def path(self, name: str) -> Path:
        return self.root / STAGE_DIR / f"{name}.json"

    def load(self, name: str, expected_input: str) -> dict[str, Any] | None:
        if not self.resume:
            return None
        record_path = self.path(name)
        if not record_path.exists():
            return None
        payload = load_json(record_path, stage="archive")
        if not isinstance(payload, dict):
            raise MediaError(
                "manifest-corrupt", f"stage record {name} is not an object",
                stage="archive", path=str(record_path),
            )
        recorded = payload.get("input_sha256")
        if recorded != expected_input:
            raise MediaError(
                "stage-hash-mismatch",
                f"stage {name} was produced from {str(recorded)[:16]}, current input "
                f"hashes to {expected_input[:16]}",
                stage="archive", path=str(record_path),
                detail={"recorded": recorded, "current": expected_input},
            )
        output = payload.get("output")
        if not isinstance(output, dict):
            return None
        recorded_output = payload.get("output_sha256")
        if recorded_output:
            actual = stage_input_digest(output=output)
            if actual != recorded_output:
                raise MediaError(
                    "stage-hash-mismatch",
                    f"stage {name} record is internally inconsistent: its output "
                    f"hashes to {actual[:16]}, the record says {recorded_output[:16]}",
                    stage="archive", path=str(record_path),
                    detail={"recorded": recorded_output, "actual": actual},
                )
        return output

    def save(self, name: str, input_sha: str, output: dict[str, Any]) -> None:
        """Persist a stage; the output digest always covers the stored payload."""
        atomic_write_json(
            self.path(name),
            {
                "stage": name,
                "input_sha256": input_sha,
                "output_sha256": stage_input_digest(output=output),
                "output": output,
            },
        )


@dataclass(slots=True)
class ArchiveReport:
    manifest: Manifest
    errors: list[MediaError] = field(default_factory=list)
    recovered_segments: int = 0
    stage_status: dict[str, str] = field(default_factory=dict)
    resumed_stages: list[str] = field(default_factory=list)

    @property
    def healthy(self) -> int:
        return len(self.manifest.healthy_segments())

    @property
    def failed(self) -> int:
        return len(self.manifest.failed_segments())

    def summary_lines(self) -> list[str]:
        lines = [
            f"archive source={self.manifest.source['path']} "
            f"format={self.manifest.source['format']}",
            f"segments_total={len(self.manifest.segments)} "
            f"segments_ok={self.healthy} segments_failed={self.failed}",
            f"recovered_segments={self.recovered_segments}",
        ]
        for name, status in self.stage_status.items():
            suffix = " resumed=1" if name in self.resumed_stages else ""
            lines.append(f"stage {name} status={status}{suffix}")
        offset = min((e.offset for e in self.errors if e.offset is not None), default=None)
        lines.append("error_offset=" + (str(offset) if offset is not None else "unknown"))
        return lines


# ---------------------------------------------------------------------------
# Timeline construction
# ---------------------------------------------------------------------------


def _stream_end_seconds(stream: StreamProbe) -> float:
    last = stream.packets[-1]
    if last.pts is None:
        return 0.0
    end = Fraction(last.pts) * stream.time_base.value
    if last.duration:
        end += Fraction(last.duration) * stream.time_base.value
    return float(end)


def build_timeline(
    result: ProbeResult,
    limits: Limits,
    *,
    cut_every_seconds: float | None = None,
    transitions: Iterable[str] = (),
) -> tuple[Timeline, list[MediaError]]:
    """Tracks and segments in the probe's integer tick base.

    Video segments split on keyframes plus the duration ceiling; audio segments
    split on the same tick positions so the two stay aligned.
    """
    tb = result.time_base
    rate = tb.rate.value
    errors: list[MediaError] = []
    mapped = [s for s in result.streams if result.mapping.role_of(s.index) is not None]
    declared_end = tb.to_ticks(Fraction(result.duration_us or 0, 1_000_000))
    end_ticks = declared_end
    tracks: list[Track] = []
    per_stream_cuts: dict[int, list[int]] = {}
    for stream in mapped:
        if not stream.packets:
            errors.append(
                MediaError(
                    "missing-stream",
                    f"mapped {stream.kind} stream has no packets; isolated from this archive",
                    stage="timeline", path=result.path, stream_index=stream.index,
                )
            )
            continue
        stream_end = tb.to_ticks(Fraction(_stream_end_seconds(stream)))
        stream_start = _content_start_ticks(result, stream)
        end_ticks = max(end_ticks, stream_end)
        tracks.append(
            Track(
                index=stream.index,
                kind=stream.kind,
                codec=stream.codec,
                time_base=stream.time_base,
                start_ticks=stream_start,
                duration_ticks=max(0, stream_end - stream_start),
                language=stream.language,
            )
        )
        per_stream_cuts[stream.index] = _cut_points(
            stream, stream_start, stream_end, limits, cut_every_seconds, rate
        )
    segments: list[Segment] = []
    for stream in mapped:
        if stream.index not in per_stream_cuts:
            continue
        track = next(t for t in tracks if t.index == stream.index)
        end = track.start_ticks + (track.duration_ticks or 0)
        segments.extend(
            _segments_for(
                stream, track.start_ticks, end, per_stream_cuts[stream.index]
            )
        )
    timeline = Timeline(tb, end_ticks, tracks, segments)
    timeline.transitions = _parse_transitions(transitions, segments, errors, result.path)
    return timeline, errors


def _parse_transitions(
    specs: Iterable[str],
    segments: list[Segment],
    errors: list[MediaError],
    path: str,
) -> list[Any]:
    """Parse ``FROM:TO:START_MS:END_MS`` cross-track overlap declarations.

    A transition is the only way two segments may overlap; declaring one that
    does not match the material is an error against *both* segments, which the
    caller then isolates, leaving every other segment archivable.
    """
    from .timeline import Transition

    by_id = {segment.segment_id: segment for segment in segments}
    parsed: list[Transition] = []
    for spec in specs:
        parts = spec.split(":")
        if len(parts) != 4:
            raise MediaError(
                "transition-mismatch",
                f"transition {spec!r} must be FROM:TO:START_MS:END_MS",
                stage="timeline", path=path,
            )
        source, target, start_ms, end_ms = parts
        if source not in by_id or target not in by_id:
            errors.append(
                MediaError(
                    "transition-mismatch",
                    f"transition {spec!r} references an unknown segment",
                    stage="timeline", path=path, detail={"segments": [source, target]},
                )
            )
            continue
        try:
            start = int(start_ms) * 1_000
            end = int(end_ms) * 1_000
        except ValueError:
            raise MediaError(
                "transition-mismatch",
                f"transition {spec!r} has non-numeric bounds",
                stage="timeline", path=path,
            ) from None
        parsed.append(Transition(f"t{len(parsed):02d}", source, target, start, end))
    return parsed


def _content_start_ticks(result: ProbeResult, stream: StreamProbe) -> int:
    """First presentation tick the stream actually carries.

    ``start_time`` is the container's view and can be zero for a stream whose
    packets all begin later (a delayed audio track, for example); segments must
    start where the material does, otherwise the first segment is empty.
    """
    for meta in stream.packets:
        if meta.pts is None:
            continue
        return result.time_base.to_ticks(Fraction(meta.pts) * stream.time_base.value)
    return result.time_base.to_ticks(
        Fraction(stream.start_raw or 0) * stream.time_base.value
    )


def _cut_points(
    stream: StreamProbe,
    start: int,
    end: int,
    limits: Limits,
    cut_every_seconds: float | None,
    rate: int,
) -> list[int]:
    ceiling = int(limits.max_segment_seconds * rate)
    if cut_every_seconds:
        ceiling = min(ceiling, int(cut_every_seconds * rate))
    if stream.kind == "video" and stream.keyframes:
        cuts = sorted({k.ticks for k in stream.keyframes if start < k.ticks < end})
    else:
        cuts = []
    bounds = [start, *cuts, end]
    enforced: list[int] = [bounds[0]]
    for bound in bounds[1:]:
        while bound - enforced[-1] > ceiling:
            enforced.append(enforced[-1] + ceiling)
        if bound > enforced[-1]:
            enforced.append(bound)
    return enforced[1:-1]


def _segments_for(
    stream: StreamProbe, start: int, end: int, cuts: list[int]
) -> list[Segment]:
    segments = contiguous_segments(
        stream.index, cuts, end,
        start_ticks=start,
        source_origin=0,
        prefix=f"{stream.kind[0]}{stream.index:02d}_",
    )
    for segment in segments:
        segment.mapping = {
            "kind": stream.kind,
            "codec": stream.codec,
            "role": "video" if stream.kind == "video" else "audio",
        }
    return segments


# ---------------------------------------------------------------------------
# Segment transforms
# ---------------------------------------------------------------------------


def _color_values(record: ColorRecord) -> list[tuple[str, int]]:
    values: list[tuple[str, int]] = []
    for name, attr in (
        ("primaries", "color_primaries"),
        ("transfer", "color_trc"),
        ("space", "colorspace"),
        ("range", "color_range"),
    ):
        entry = record.fields.get(name)
        if entry is not None and entry.value is not None and entry.supported:
            values.append((attr, int(entry.value)))
    return values


def _color_tags(record: ColorRecord) -> dict[str, str]:
    tags: dict[str, str] = {}
    for name, key in (
        ("primaries", "COLOR_PRIMARIES"),
        ("transfer", "COLOR_TRANSFER"),
        ("space", "COLOR_SPACE"),
        ("range", "COLOR_RANGE"),
    ):
        entry = record.fields.get(name)
        if entry is not None and entry.label:
            tags[key] = entry.label
    return tags


def remux_video_segment(
    result: ProbeResult,
    stream: StreamProbe,
    segment: Segment,
    output_path: str,
    color: ColorRecord,
) -> SegmentRecord:
    """Stream-copy one segment to MP4, preserving colour and source offsets."""
    rate = result.time_base.rate.value
    start_ticks = segment.source_start_ticks if segment.source_start_ticks is not None else segment.start_ticks
    end_ticks = segment.source_end_ticks if segment.source_end_ticks is not None else segment.end_ticks
    start_seconds = float(Fraction(start_ticks) / rate)
    end_seconds = float(Fraction(end_ticks) / rate)
    with quiet_libav():
        return _remux_video_segment(
            result, stream, segment, output_path, color,
            start_ticks, end_ticks, start_seconds, end_seconds,
        )


def _remux_video_segment(
    result: ProbeResult,
    stream: StreamProbe,
    segment: Segment,
    output_path: str,
    color: ColorRecord,
    start_ticks: int,
    end_ticks: int,
    start_seconds: float,
    end_seconds: float,
) -> SegmentRecord:
    rate = result.time_base.rate.value
    container = av.open(result.path)
    output = None
    try:
        input_stream = container.streams[stream.index]
        packets = iter_packets(container, input_stream, Fraction(start_ticks) / rate)
        skipped: list[MediaError] = []
        cache: dict[str, Any] = {}
        output = av.open(output_path, "w", format="mp4")
        output.bit_exact = True
        output_stream = output.add_stream(template=input_stream)
        for attr, value in _color_values(color):
            try:
                setattr(output_stream.codec_context, attr, value)
            except Exception:  # noqa: BLE001 - best-effort metadata carry
                pass
        for key, value in _color_tags(color).items():
            output_stream.metadata[key] = value
        keyframes: list[int] = []
        count = 0
        muxed = _MuxState(output_stream)
        for packet in packets:
            if packet.pts is None and packet.dts is None:
                continue
            pts = packet.pts if packet.pts is not None else packet.dts
            when = float(Fraction(pts) * input_stream.time_base)
            duration = float(Fraction(packet.duration or 0) * input_stream.time_base)
            if when + duration <= start_seconds + 1e-9:
                continue
            if when >= end_seconds - 1e-9:
                break
            if not _packet_decodes(input_stream, packet, cache):
                # A damaged packet must not be copied into an archived segment
                # just because its container framing is intact.
                skipped.append(
                    MediaError(
                        "corrupt-packet",
                        f"packet at {when:.3f}s does not decode and was not archived",
                        stage="archive", path=result.path,
                        stream_index=stream.index,
                        offset=_packet_offset(packet),
                        detail={"segment_id": segment.segment_id},
                    )
                )
                continue
            source_pts = packet.pts
            muxed.push(packet, output)
            count += 1
            if packet.is_keyframe and source_pts is not None:
                keyframes.append(
                    result.time_base.to_ticks(
                        Fraction(source_pts) * stream.time_base.value
                    )
                )
        muxed.flush(output)
        if count == 0:
            raise MediaError(
                "corrupt-packet",
                f"segment {segment.segment_id} contains no muxable packets",
                stage="archive", path=result.path, stream_index=stream.index,
            )
    finally:
        if output is not None:
            output.close()
        container.close()
    if skipped:
        latest = skipped[-1]
        latest.detail.setdefault("segment_id", segment.segment_id)
        if os.path.exists(output_path):
            os.unlink(output_path)
        raise latest
    return SegmentRecord(
        segment_id=segment.segment_id,
        track_index=stream.index,
        start_ticks=segment.start_ticks,
        end_ticks=segment.end_ticks,
        path=output_path,
        sha256=sha256_file(output_path),
        keyframe_ticks=keyframes,
        keyframe_hash=keyframe_digest(keyframes),
        audio=None,
    )


def _packet_offset(packet: Any) -> int | None:
    position = getattr(packet, "pos", None)
    return position if position is not None and position >= 0 else None


def _packet_decodes(stream, packet, cache: dict[str, Any]) -> bool:
    """True when the packet can be submitted to a fresh decoder without error.

    Video packets are checked one by one with a decoder that has seen the
    preceding packets of the same GOP (so reference frames resolve), while a
    decode error on a packet is attributed to that packet.  Audio is checked by
    the audio stage itself, which decodes every sample it archives.
    """
    if stream.codec_context.type != "video":
        return True
    decoder = cache.get("decoder")
    if packet.is_keyframe or decoder is None:
        codec_context = stream.codec_context
        decoder = av.CodecContext.create(codec_context.name, "r")
        if codec_context.extradata:
            decoder.extradata = codec_context.extradata
        cache["decoder"] = decoder
    clone = av.Packet(bytes(packet))
    clone.pts = packet.pts
    clone.dts = packet.dts
    clone.duration = packet.duration
    clone.time_base = packet.time_base
    try:
        with quiet_libav():
            for _ in decoder.decode(clone):
                pass
    except Exception:  # noqa: BLE001 - any decoder rejection is damage
        cache["decoder"] = None
        return False
    return True


class _MuxState:
    """Rebase timestamps to zero and guarantee monotonic DTS for the muxer.

    Matroska stores no decode timestamps, so libavformat synthesises them for
    reordered video and leaves the first few packets unset.  Handing those to
    the MP4 muxer as "DTS = PTS" produces a regression the muxer rejects.  This
    class holds the leading unset packets until it has seen a real DTS and a
    nominal frame duration, back-fills them, then shifts the whole segment so
    its first DTS is zero.  Every packet is muxed exactly once and in order.
    """

    def __init__(self, output_stream) -> None:
        self.output_stream = output_stream
        self.base: int | None = None
        self.last_dts: int | None = None
        self.frame_duration: int | None = None
        self.leading: list[Any] = []
        self.settled = False

    def push(self, packet, output) -> None:
        if packet.dts is None and not self.settled:
            self.leading.append(packet)
            return
        if not self.settled:
            self._backfill(packet.dts or packet.pts or 0)
        self._emit(packet, output)

    def _backfill(self, first_dts: int) -> None:
        self.settled = True
        duration = self.frame_duration or 1
        total = len(self.leading)
        for position, packet in enumerate(self.leading):
            packet.dts = first_dts - (total - position) * duration
        self.leading = []

    def flush(self, output) -> None:
        if not self.settled and self.leading:
            # No packet ever carried a DTS (audio or intra-only video): PTS is
            # the correct DTS, and segment order is already presentation order.
            self.settled = True
            for packet in self.leading:
                packet.dts = packet.pts if packet.pts is not None else 0
            pending, self.leading = self.leading, []
            for packet in pending:
                self._emit(packet, output)
            return
        # Packets buffered while waiting cannot remain after a settled stream.
        for packet in self.leading:
            packet.dts = self.last_dts if self.last_dts is not None else (packet.pts or 0)
            self._emit(packet, output)
        self.leading = []

    def _emit(self, packet, output) -> None:
        if packet.duration and packet.duration > 0:
            self.frame_duration = self.frame_duration or packet.duration
        dts = packet.dts if packet.dts is not None else packet.pts
        pts = packet.pts if packet.pts is not None else dts
        if dts is None:
            return
        if pts is not None and pts < dts:
            pts = dts
        if self.base is None:
            self.base = dts
        dts -= self.base
        pts -= self.base
        if self.last_dts is not None and dts <= self.last_dts:
            dts = self.last_dts + 1
            if pts < dts:
                pts = dts
        self.last_dts = dts
        packet.pts = pts
        packet.dts = dts
        packet.stream = self.output_stream
        output.mux(packet)


def _archive_segment(
    result: ProbeResult,
    segment: Segment,
    settings: AudioSettings,
    color: ColorRecord,
    segment_dir: Path,
) -> tuple[SegmentRecord | None, MediaError | None]:
    stream = result.stream(segment.track_index)
    if stream is None:
        return None, MediaError(
            "type-mismatch",
            f"segment {segment.segment_id} references a stream the source lacks",
            stage="archive", path=result.path, stream_index=segment.track_index,
        )
    try:
        if stream.kind == "video":
            return remux_video_segment(
                result, stream, segment, str(segment_dir / f"{segment.segment_id}.mp4"), color
            ), None
        # Audio segments are produced elsewhere, in one decode pass per stream;
        # this dispatcher only handles video.
        return None, MediaError(
            "type-mismatch",
            f"segment {segment.segment_id} has unsupported kind {stream.kind}",
            stage="archive", path=result.path, stream_index=stream.index,
        )
    except MediaError as exc:
        return None, exc
    except Exception as exc:  # noqa: BLE001 - keep the other segments archiving
        return None, bridge(
            "corrupt-packet", exc, stage="archive",
            path=result.path, stream_index=stream.index,
        )


def _failed_record(segment: Segment, error: MediaError | None) -> SegmentRecord:
    return SegmentRecord(
        segment_id=segment.segment_id,
        track_index=segment.track_index,
        start_ticks=segment.start_ticks,
        end_ticks=segment.end_ticks,
        path="",
        sha256="",
        status="failed",
        errors=[] if error is None else [error],
    )


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def archive(
    input_path: str,
    manifest_path: str,
    *,
    rate: int = 48_000,
    layout: str = "stereo",
    lufs: float | None = None,
    mapping: Iterable[str] = (),
    work_dir: str = ".tmp/work",
    resume: bool = False,
    limits: Limits | None = None,
    cut_every_seconds: float | None = None,
    transitions: Iterable[str] = (),
) -> ArchiveReport:
    """Run the archive pipeline; isolated failures do not abort the run."""
    limits = limits or Limits()
    limits.validate()
    settings = AudioSettings(rate=rate, layout=layout, target_lufs=lufs)
    settings.validate()
    try:
        os.path.getsize(input_path)
    except OSError as exc:
        raise MediaError(
            "io", f"cannot read source: {exc}", stage="probe", path=input_path
        ) from exc

    store = StageStore(Path(work_dir), resume=resume)
    store.prepare()
    segment_dir = Path(work_dir) / SEGMENT_DIR
    resumed: list[str] = []

    # -- stage: probe ------------------------------------------------------
    with quiet_libav():
        return _archive_inner(
            input_path, manifest_path,
            rate=rate, layout=layout, lufs=lufs, mapping=mapping,
            work_dir=work_dir, resume=resume, limits=limits,
            cut_every_seconds=cut_every_seconds, transitions=transitions,
            store=store, segment_dir=segment_dir, settings=settings,
            resumed=resumed,
        )


def _archive_inner(
    input_path: str,
    manifest_path: str,
    *,
    rate: int,
    layout: str,
    lufs: float | None,
    mapping: Iterable[str],
    work_dir: str,
    resume: bool,
    limits: Limits,
    cut_every_seconds: float | None,
    transitions: Iterable[str],
    store: StageStore,
    segment_dir: Path,
    settings: AudioSettings,
    resumed: list[str],
) -> ArchiveReport:
    source_sha = sha256_file(input_path)
    probe_input = stage_input_digest(
        path=os.path.abspath(input_path),
        sha256=source_sha,
        mapping=list(mapping),
        max_file_bytes=limits.max_file_bytes,
    )
    cached = store.load("probe", probe_input)
    if cached is not None:
        result = ProbeResult.from_dict(cached)
        resumed.append("probe")
    else:
        result = probe(
            input_path, mapping,
            limits={"max_file_bytes": limits.max_file_bytes},
        )
        store.save("probe", probe_input, result.to_dict())

    # -- stage: timeline ---------------------------------------------------
    timeline_input = stage_input_digest(
        probe=result.source_sha256,
        max_segment_seconds=limits.max_segment_seconds,
        cut_every_seconds=cut_every_seconds,
        transitions=list(transitions),
    )
    cached = store.load("timeline", timeline_input)
    if cached is not None:
        timeline = Timeline.from_dict(cached["timeline"])
        timeline_errors = [MediaError.from_dict(e) for e in cached.get("errors", [])]
        resumed.append("timeline")
    else:
        timeline, timeline_errors = build_timeline(
            result, limits,
            cut_every_seconds=cut_every_seconds,
            transitions=transitions,
        )
        store.save(
            "timeline", timeline_input,
            {
                "timeline": timeline.to_dict(),
                "errors": [e.to_dict() for e in timeline_errors],
            },
        )
    isolated_timeline = _isolate_timeline_errors(timeline_errors, timeline)
    all_timeline_errors = timeline_errors + timeline.validate(path=result.path)
    all_timeline_errors.extend(isolated_timeline)

    # -- stage: color ------------------------------------------------------
    color: ColorRecord | None = None
    if result.video is not None:
        color = resolve_color(result.video, path=result.path)

    manifest = Manifest.create(result, timeline, color=color)
    report = ArchiveReport(manifest=manifest, resumed_stages=resumed)
    report.errors.extend(all_timeline_errors)
    if color is not None:
        report.errors.extend(color.warnings)
    report.errors.extend(result.errors)
    report.stage_status["probe"] = "ok" if not result.errors else "partial"
    report.stage_status["timeline"] = "partial" if all_timeline_errors else "ok"
    report.stage_status["color"] = "partial" if color and color.warnings else "ok"

    # -- stage: transform -------------------------------------------------
    failed_ids: set[str] = set()
    for error in all_timeline_errors:
        named = error.detail.get("segment_id")
        if isinstance(named, str):
            failed_ids.add(named)
        elif isinstance(named, (list, tuple)):
            failed_ids.update(str(x) for x in named)
    transform_input = stage_input_digest(
        timeline=stage_input_digest(timeline=timeline.to_dict()),
        rate=rate, layout=layout, lufs=lufs,
        source=result.source_sha256,
        isolated=sorted(str(x) for x in failed_ids),
    )
    cached = store.load("transform", transform_input)
    if cached is not None:
        records = [SegmentRecord.from_dict(r) for r in cached.get("segments", [])]
        report.recovered_segments = int(cached.get("recovered_segments", 0))
        resumed.append("transform")
    else:
        records, recovered = _run_transform(
            result, timeline, settings, color, segment_dir, limits, failed_ids
        )
        report.recovered_segments = recovered
        store.save(
            "transform", transform_input,
            {
                "segments": [r.to_dict() for r in records],
                "recovered_segments": recovered,
            },
        )
    manifest.segments = records
    failed = [r for r in records if r.status != "ok"]
    report.errors.extend(e for r in failed for e in r.errors)
    report.stage_status["transform"] = "partial" if failed else "ok"

    # -- stage: archive (manifest on disk) --------------------------------
    probe_output = stage_input_digest(probe=result.source_sha256)
    timeline_output = stage_input_digest(timeline=timeline.to_dict())
    # Paths are a property of the work directory, not of the archive; excluding
    # them keeps the stage digest stable across machines and job counts.
    transform_output = stage_input_digest(
        segments=[_digest_view(r) for r in records]
    )
    manifest.record_stage(StageRecord(
        "probe", report.stage_status["probe"], probe_input, probe_output,
        errors=list(result.errors),
        detail={"streams": len(result.streams), "format": result.format_name},
    ))
    manifest.record_stage(StageRecord(
        "timeline", report.stage_status["timeline"], timeline_input, timeline_output,
        upstream_sha256=probe_output,
        errors=all_timeline_errors,
        detail={"tracks": len(timeline.tracks), "segments": len(timeline.segments)},
    ))
    manifest.record_stage(StageRecord(
        "transform", report.stage_status["transform"], transform_input, transform_output,
        upstream_sha256=timeline_output,
        errors=[e for r in failed for e in r.errors],
        detail={
            "segments": len(records),
            "failed": len(failed),
            "recovered_segments": report.recovered_segments,
            "isolated": sorted(s.segment_id for s in failed),
        },
    ))
    manifest.record_stage(StageRecord(
        "archive",
        "ok" if records else "failed",
        transform_output,
        stage_input_digest(manifest=manifest.to_dict()),
        upstream_sha256=transform_output,
        detail={"segments": len(records), "failed": len(failed)},
    ))
    report.stage_status["archive"] = manifest.stage("archive").status
    manifest.write(manifest_path)
    if not records:
        raise MediaError(
            "verification-failed", "no segment could be archived",
            stage="archive", path=result.path,
        )
    return report


def _digest_view(record: SegmentRecord) -> dict[str, Any]:
    """Segment record without local filesystem paths, for stage digests."""
    payload = record.to_dict()
    payload["path"] = ""
    if isinstance(payload.get("audio"), dict):
        payload["audio"]["path"] = ""
    payload.pop("status", None)
    return payload


def _isolate_timeline_errors(
    errors: list[MediaError], timeline: Timeline
) -> list[MediaError]:
    """Decide which segments a timeline error condemns.

    Errors that name their segments (an undeclared overlap) isolate exactly
    those; an error that cannot be attributed to one segment is recorded but
    isolates nothing, so a healthy track is never dropped on a guess.
    """
    stamped: list[MediaError] = []
    for error in errors:
        named = error.detail.get("segment_ids")
        if named:
            error.detail.setdefault("segment_id", list(named))
            stamped.append(error)
            continue
        if error.code in ("transition-overlap", "transition-mismatch"):
            stamped.append(error)
            continue
        candidates = [
            s for s in timeline.segments
            if error.stream_index is None or s.track_index == error.stream_index
        ]
        if len(candidates) == 1:
            error.detail.setdefault("segment_id", candidates[0].segment_id)
        stamped.append(error)
    return stamped


def _run_transform(
    result: ProbeResult,
    timeline: Timeline,
    settings: AudioSettings,
    color: ColorRecord | None,
    segment_dir: Path,
    limits: Limits,
    failed_ids: set[str],
) -> tuple[list[SegmentRecord], int]:
    from .color import ColorRecord as _ColorRecord

    colors = color or _ColorRecord(stream_index=-1)
    tasks = [s for s in timeline.segments if s.segment_id not in failed_ids]
    skipped = [s for s in timeline.segments if s.segment_id in failed_ids]
    by_stream: dict[int, list] = {}
    for segment in tasks:
        by_stream.setdefault(segment.track_index, []).append(segment)
    # Audio for one stream is cut in a single decode pass, so a long master is
    # read once per audio stream rather than once per segment.
    audio_records: dict[str, AudioSegment] = {}
    for index, segments in by_stream.items():
        stream = result.stream(index)
        if stream is None or stream.kind != "audio":
            continue
        outputs = [
            str(segment_dir / f"{segment.segment_id}.wav") for segment in segments
        ]
        for produced in extract_segments(result, stream, segments, settings, outputs):
            audio_records[produced.segment_id] = produced
    video_tasks = [
        segment for segment in tasks
        if (result.stream(segment.track_index) or None) is not None
        and result.stream(segment.track_index).kind == "video"
    ]
    if limits.jobs == 1 or len(video_tasks) <= 1:
        outcomes = [
            _archive_segment(result, segment, settings, colors, segment_dir)
            for segment in video_tasks
        ]
    else:
        with ThreadPoolExecutor(max_workers=limits.jobs) as pool:
            outcomes = list(
                pool.map(
                    lambda seg: _archive_segment(result, seg, settings, colors, segment_dir),
                    video_tasks,
                )
            )
    records: list[SegmentRecord] = []
    recovered = 0
    for segment, (record, error) in zip(video_tasks, outcomes):
        if record is None:
            records.append(_failed_record(segment, error))
            continue
        records.append(record)
    for segment in tasks:
        if result.stream(segment.track_index).kind != "audio":
            continue
        produced = audio_records.get(segment.segment_id)
        if produced is None:
            records.append(_failed_record(
                segment,
                MediaError(
                    "audio-decode",
                    f"no audio artefact was produced for {segment.segment_id}",
                    stage="audio", path=result.path, stream_index=segment.track_index,
                ),
            ))
            continue
        if produced.error is not None:
            records.append(_failed_record(segment, produced.error))
            continue
        records.append(
            SegmentRecord(
                segment_id=segment.segment_id,
                track_index=segment.track_index,
                start_ticks=segment.start_ticks,
                end_ticks=segment.end_ticks,
                path=produced.path,
                sha256=produced.sha256,
                audio=produced,
            )
        )
    for segment in skipped:
        error = MediaError(
            "edit-out-of-range",
            f"segment {segment.segment_id} was isolated by timeline validation",
            stage="timeline",
            path=result.path,
            stream_index=segment.track_index,
            detail={"segment_id": segment.segment_id},
        )
        records.append(_failed_record(segment, error))
    records.sort(key=lambda r: (r.track_index, r.start_ticks))
    return records, recovered
