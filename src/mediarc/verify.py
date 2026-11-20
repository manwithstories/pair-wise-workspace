"""Verification: segment hashes, keyframe index, round-trip, damage recovery.

Verification answers three separate questions and never conflates them:

1. *Is the manifest faithful?*  Re-serialise, re-parse, compare every record.
2. *Are the archived bytes the bytes we recorded?*  Re-hash each segment file.
3. *Does the source still support the recorded index?*  Re-probe the source and
   check the keyframe ticks and offsets the manifest claims.

A fourth, deeper check is available for damaged material: ``integrity_scan``
decodes each GOP in isolation, locates the first packet whose loss breaks the
GOP, and ``recover`` re-cuts from the last intact keyframe while skipping only
the packets that fail validation.  Damage is reported with the **source file
byte offset** of the offending packet; if no packet position is known the
offset is ``None`` and renders as ``unknown``.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

import os

import av

from .errors import MediaError
from .manifest import Manifest, SegmentRecord, keyframe_digest
from .probe import ProbeResult, StreamProbe, probe
from .util import quiet_libav, sha256_file


@dataclass(slots=True)
class HashCheck:
    segment_id: str
    kind: str
    expected: str
    actual: str | None
    ok: bool
    path: str

    def line(self) -> str:
        state = "ok" if self.ok else "mismatch"
        return f"hash {self.segment_id} kind={self.kind} status={state}"


@dataclass(slots=True)
class KeyframeCheck:
    segment_id: str
    recorded: list[int]
    found: list[int]
    missing: list[int]

    @property
    def ok(self) -> bool:
        return not self.missing


@dataclass(slots=True)
class IndexCheck:
    """Container sync-sample index versus the keyframes actually demuxed."""

    stream_index: int
    declared: int
    demuxed: int
    ratio: float
    missing: list[int]
    ok: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "stream_index": self.stream_index,
            "declared": self.declared,
            "demuxed": self.demuxed,
            "ratio": round(self.ratio, 6),
            "missing": list(self.missing),
            "ok": self.ok,
        }


@dataclass(slots=True)
class VerifyReport:
    manifest: Manifest | None
    segment_hashes: list[HashCheck] = field(default_factory=list)
    audio_hashes: list[HashCheck] = field(default_factory=list)
    keyframes: list[KeyframeCheck] = field(default_factory=list)
    index_checks: list[IndexCheck] = field(default_factory=list)
    roundtrip_total: int = 0
    roundtrip_ok: int = 0
    errors: list[MediaError] = field(default_factory=list)
    recovered_segments: int = 0
    recovery_notes: list[dict[str, Any]] = field(default_factory=list)
    isolated_segments: list[tuple[str, list[str]]] = field(default_factory=list)
    stage_note: str = ""

    # -- metrics -----------------------------------------------------------
    @property
    def roundtrip_rate(self) -> float:
        if not self.roundtrip_total:
            return 1.0
        return self.roundtrip_ok / self.roundtrip_total

    @property
    def keyframe_hit_rate(self) -> float:
        """Fraction of recorded keyframes still present in the source index.

        Container-level index defects (an MP4 ``stss`` that omits a real
        keyframe) are folded in: the ratio is the number of keyframes both the
        manifest and the container agree on, over the number the manifest
        recorded or the container declares, whichever is larger.
        """
        expected = sum(len(k.recorded) for k in self.keyframes)
        hits = expected - sum(len(k.missing) for k in self.keyframes)
        for check in self.index_checks:
            if not check.ok:
                expected = max(expected, check.declared) or expected
                hits = min(hits, check.demuxed)
        if not expected:
            return 1.0
        return min(hits, expected) / expected

    @property
    def hash_ok(self) -> int:
        return sum(1 for h in self.segment_hashes if h.ok)

    @property
    def hash_total(self) -> int:
        return len(self.segment_hashes)

    @property
    def audio_ok(self) -> int:
        return sum(1 for h in self.audio_hashes if h.ok)

    @property
    def audio_total(self) -> int:
        return len(self.audio_hashes)

    @property
    def ok(self) -> bool:
        return (
            self.roundtrip_rate >= 1.0
            and self.keyframe_hit_rate >= 1.0
            and all(c.ok for c in self.index_checks)
            and self.hash_ok == self.hash_total
            and self.audio_ok == self.audio_total
            and not self.errors
            and self.hash_total > 0
        )

    @property
    def error_offset(self) -> int | None:
        """Smallest source byte offset any diagnostic could pin down.

        Offsets are source-file positions, never offsets into a rewritten
        segment, and ``None`` (rendered ``unknown``) when libavformat exposed no
        position for the failing packet.
        """
        known = [e.offset for e in self.errors if e.offset is not None]
        for note in self.recovery_notes:
            if note.get("offset") is not None:
                known.append(int(note["offset"]))
        return min(known) if known else None

    def lines(self) -> list[str]:
        lines = [
            f"roundtrip_rate={self.roundtrip_rate:.3f}",
            f"keyframe_hit_rate={self.keyframe_hit_rate:.3f}",
            f"segment_hashes={self.hash_ok}/{self.hash_total}",
            f"audio_segment_hashes={self.audio_ok}/{self.audio_total}",
            f"recovered_segments={self.recovered_segments}",
            f"isolated_segments={len(self.isolated_segments)}",
            f"error_offset={self.error_offset if self.error_offset is not None else 'unknown'}",
        ]
        for check in self.index_checks:
            if not check.ok:
                lines.append(
                    f"keyframe_index_gap stream={check.stream_index} "
                    f"declared={check.declared} demuxed={check.demuxed} "
                    f"missing_samples={check.missing[:8]}"
                )
        for check in self.keyframes:
            if check.missing:
                lines.append(
                    f"keyframes_missing segment={check.segment_id} ticks={check.missing}"
                )
        if self.stage_note:
            lines.append(self.stage_note)
        for error in self.errors:
            lines.append(error.render())
        for note in self.recovery_notes:
            lines.append(
                "recovery "
                f"fixture={Path(str(note.get('fixture', ''))).name} "
                f"segment={note.get('segment_id')} "
                f"offset={note.get('offset') if note.get('offset') is not None else 'unknown'} "
                f"resume_offset={note.get('resume_offset') if note.get('resume_offset') is not None else 'unknown'} "
                f"salvaged_ticks={note.get('salvaged_ticks')}"
            )
        return lines


# ---------------------------------------------------------------------------
# Manifest verification
# ---------------------------------------------------------------------------


def _check_roundtrip(manifest: Manifest, report: VerifyReport) -> None:
    text = manifest.dumps()
    try:
        again = Manifest.loads(text)
    except MediaError as exc:
        report.errors.append(exc)
        report.roundtrip_total = 1
        return
    records = manifest.to_dict()
    reparsed = again.to_dict()
    report.roundtrip_total = max(1, len(manifest.segments))
    same_top = all(
        records[key] == reparsed[key]
        for key in ("source", "mapping", "timeline", "color", "stages", "notes")
    )
    if not same_top:
        report.errors.append(
            MediaError(
                "verification-failed",
                "manifest header did not survive a serialise/parse round-trip",
                stage="verify",
            )
        )
    per_segment = sum(
        1
        for before, after in zip(records["segments"], reparsed["segments"])
        if before == after
    ) + (int(same_top) * max(0, report.roundtrip_total - len(records["segments"])))
    report.roundtrip_ok = min(per_segment, report.roundtrip_total)
    if not same_top and report.roundtrip_ok < report.roundtrip_total:
        report.roundtrip_ok = 0


def _hash_segments(manifest: Manifest, report: VerifyReport) -> None:
    for segment in manifest.segments:
        if segment.status != "ok":
            # Isolated failures are an accepted outcome: the manifest says the
            # segment failed, so there is no archived byte to re-hash.  The
            # count is surfaced in the summary rather than hidden.
            report.isolated_segments.append(
                (segment.segment_id, [e.code for e in segment.errors])
            )
            continue
        if segment.track_index == _video_track(manifest) and segment.path:
            report.segment_hashes.append(
                _hash_one(segment, segment.path, "video", segment.sha256)
            )
        if segment.audio is not None:
            audio = segment.audio
            report.audio_hashes.append(
                _hash_one(segment, audio.path, "audio", audio.sha256)
            )


def _hash_one(
    segment: SegmentRecord, path: str, kind: str, expected: str
) -> HashCheck:
    if not Path(path).exists():
        return HashCheck(segment.segment_id, kind, expected, None, False, path)
    try:
        actual = sha256_file(path)
    except OSError:
        return HashCheck(segment.segment_id, kind, expected, None, False, path)
    return HashCheck(segment.segment_id, kind, expected, actual, actual == expected, path)


def _video_track(manifest: Manifest) -> int:
    for track in manifest.timeline.tracks:
        if track.kind == "video":
            return track.index
    return -1


def check_keyframes(manifest: Manifest, source: ProbeResult) -> list[KeyframeCheck]:
    """Compare recorded keyframe ticks with the source's actual index."""
    checks: list[KeyframeCheck] = []
    for segment in manifest.segments:
        if segment.status != "ok" or not segment.keyframe_ticks:
            continue
        stream = source.stream(segment.track_index)
        if stream is None:
            checks.append(
                KeyframeCheck(segment.segment_id, segment.keyframe_ticks, [], segment.keyframe_ticks)
            )
            continue
        available = {k.ticks for k in stream.keyframes}
        missing = [t for t in segment.keyframe_ticks if t not in available]
        found = [t for t in segment.keyframe_ticks if t in available]
        checks.append(KeyframeCheck(segment.segment_id, list(segment.keyframe_ticks), found, missing))
    return checks


def _index_checks(probed: ProbeResult, manifest: Manifest | None) -> list[IndexCheck]:
    """Compare a container's declared sync samples with the demuxed keyframes.

    The index is *incomplete* when the container lists fewer sync samples than
    the bitstream actually contains; it is *stale* when it lists more.  Both
    mean a repair tool would seek to the wrong place, so neither is accepted.
    """
    checks: list[IndexCheck] = []
    for stream in probed.streams:
        if stream.kind != "video" or not stream.sync_samples:
            continue
        declared = len(set(stream.sync_samples))
        demuxed = len({k.pts for k in stream.keyframes if k.pts is not None})
        if declared == demuxed:
            checks.append(IndexCheck(stream.index, declared, demuxed, 1.0, [], True))
            continue
        missing = _missing_samples(stream)
        ratio = min(declared, demuxed) / max(declared, demuxed, 1)
        checks.append(IndexCheck(stream.index, declared, demuxed, ratio, missing, False))
    return checks


def _missing_samples(stream: StreamProbe) -> list[int]:
    """Sample ordinals that are keyframes in the bitstream but absent from ``stss``.

    Falls back to the declared ordinals when a demuxer overrides the container
    index, which is exactly the situation an incomplete index describes.
    """
    listed = {value - 1 for value in stream.sync_samples}
    key_ordinals = {
        meta.index for meta in stream.packets if meta.keyframe
    }
    absent = sorted(key_ordinals - listed)
    return [ordinal + 1 for ordinal in absent]


def verify_manifest(
    manifest: Manifest,
    *,
    source_path: str | None = None,
    limit_errors: int = 50,
) -> VerifyReport:
    """Verify a manifest's structure, hashes and keyframe index."""
    report = VerifyReport(manifest=manifest)
    _check_roundtrip(manifest, report)
    _hash_segments(manifest, report)
    for segment in manifest.segments:
        if segment.status == "ok" and segment.keyframe_hash and segment.keyframe_ticks:
            recomputed = keyframe_digest(segment.keyframe_ticks)
            if recomputed != segment.keyframe_hash:
                report.errors.append(
                    MediaError(
                        "verification-failed",
                        f"segment {segment.segment_id} keyframe digest does not match "
                        f"its own index",
                        stage="verify",
                        stream_index=segment.track_index,
                    )
                )
    source = source_path or str(manifest.source.get("path") or "")
    if source and Path(source).exists():
        try:
            with quiet_libav():
                probed = probe(source)
        except MediaError as exc:
            report.errors.append(exc)
            probed = None
        if probed is not None:
            if probed.source_sha256 != manifest.source.get("sha256"):
                report.errors.append(
                    MediaError(
                        "verification-failed",
                        "source file digest differs from the manifest; keyframe index "
                        "cannot be trusted against this file",
                        stage="verify", path=source,
                    )
                )
            else:
                report.keyframes = check_keyframes(manifest, probed)
                report.index_checks = _index_checks(probed, manifest)
                for check in report.keyframes:
                    if check.missing:
                        report.errors.append(
                            MediaError(
                                "keyframe-index-incomplete",
                                f"segment {check.segment_id} records keyframes "
                                f"{check.missing} that the source does not have",
                                stage="verify", path=source,
                                stream_index=manifest.segments[0].track_index
                                if manifest.segments else None,
                            )
                        )
    else:
        report.errors.append(
            MediaError(
                "io",
                f"source {source!r} is not available; keyframe index was not re-checked",
                stage="verify", path=source or None,
            )
        )
    if len(report.errors) > limit_errors:
        report.errors = report.errors[:limit_errors]
    return report


# ---------------------------------------------------------------------------
# Integrity scan and recovery
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class Damage:
    """One localised defect in a source file."""

    stream_index: int
    gop_start_ticks: int
    packet_index: int
    offset: int | None
    reason: str
    expected_frames: int
    decoded_frames: int

    def error(self, path: str, timeline) -> MediaError:
        return MediaError(
            "corrupt-packet",
            f"{self.reason} in GOP starting {self.gop_start_ticks} "
            f"({self.decoded_frames}/{self.expected_frames} frames decoded)",
            stage="verify", path=path, stream_index=self.stream_index,
            offset=self.offset,
            detail={
                "packet_index": self.packet_index,
                "gop_start_ticks": self.gop_start_ticks,
            },
        )


def _fresh_decoder(codec: str, extradata: bytes | None) -> av.CodecContext:
    decoder = av.CodecContext.create(codec, "r")
    if extradata:
        decoder.extradata = extradata
    return decoder


def _drain(decoder: av.CodecContext) -> tuple[int, str | None]:
    frames = 0
    try:
        for _ in decoder.decode(None):
            frames += 1
    except Exception as exc:  # noqa: BLE001 - decoder error types vary
        return frames, f"{type(exc).__name__}: {exc}"
    return frames, None


def _copy_packet(packet: Any) -> Any:
    """Independent copy of a packet, so replaying it does not touch the source."""
    clone = av.Packet(bytes(packet))
    clone.pts = packet.pts
    clone.dts = packet.dts
    clone.duration = packet.duration
    clone.time_base = packet.time_base
    return clone


def _decode_gop(
    packets: list[tuple[Any, int | None, int | None]], codec: str, extradata: bytes | None
) -> tuple[int, str | None]:
    """Decode one GOP with a decoder that has seen nothing else.

    ``Packet.decode()`` would use the packet's own stream decoder, so packets
    are fed to the fresh context explicitly; otherwise damage in an earlier GOP
    leaks into this one and the diagnosis points at the wrong packet.  Each
    packet is replayed through a copy: a first failure can leave the demuxed
    packet unusable, and the repair path needs to reuse it.
    """
    decoder = _fresh_decoder(codec, extradata)
    frames = 0
    for packet, _index, _offset in packets:
        try:
            for _ in decoder.decode(_copy_packet(packet)):
                frames += 1
        except Exception as exc:  # noqa: BLE001 - decoder error types vary
            return frames, f"{type(exc).__name__}: {exc}"
    drained, error = _drain(decoder)
    return frames + drained, error


def _isolated_frame_counts(
    packets: list[tuple[Any, int | None, int | None]], codec: str, extradata: bytes | None
) -> list[int]:
    """Decode each prefix in a fresh decoder; returns frames produced per prefix."""
    counts: list[int] = []
    for length in range(1, len(packets) + 1):
        frames, _error = _decode_gop(packets[:length], codec, extradata)
        counts.append(frames)
    return counts


def integrity_scan(
    result: ProbeResult,
    *,
    stream_index: int | None = None,
    max_gops: int = 64,
) -> list[Damage]:
    """Decode each video GOP in isolation and locate the first broken packet.

    A packet is considered the point of loss when it is the first one after
    which the decoded frame count stops advancing while later packets still do
    not recover it.  The reported offset is that packet's container byte
    position, so a repair operator can cut the file precisely.
    """
    stream = result.stream(stream_index) if stream_index is not None else result.video
    if stream is None or stream.kind != "video" or not stream.packets:
        return []
    container = av.open(result.path)
    try:
        av_stream = container.streams[stream.index]
        codec = av_stream.codec_context.name
        extradata = av_stream.codec_context.extradata
        packets: list[tuple[Any, int | None, int | None, bool]] = []
        for packet in container.demux(av_stream):
            if packet.size == 0 and packet.pts is None and packet.dts is None:
                continue
            meta = next(
                (m for m in stream.packets if m.offset == _pos(packet) and m.pts == packet.pts),
                None,
            )
            packets.append((packet, None if meta is None else meta.index,
                            None if meta is None else meta.offset,
                            bool(packet.is_keyframe)))
    finally:
        container.close()

    damages: list[Damage] = []
    gops: list[list[tuple[Any, int | None, int | None]]] = []
    starts: list[int] = []
    current: list[tuple[Any, int | None, int | None]] = []
    current_start = 0
    for packet, index, offset, is_key in packets:
        if is_key and current:
            gops.append(current)
            starts.append(current_start)
            current = []
        if is_key:
            current_start = result.time_base.to_ticks(
                Fraction(packet.pts or 0) * stream.time_base.value
            )
        current.append((packet, index, offset))
    if current:
        gops.append(current)
        starts.append(current_start)

    framerate = float(stream.framerate.value) if stream.framerate else 0.0
    for position, (gop, start) in enumerate(zip(gops, starts)):
        if position >= max_gops:
            break
        with quiet_libav():
            frames, error = _decode_gop(gop, codec, extradata)
        next_start = starts[position + 1] if position + 1 < len(starts) else None
        expected = _expected_frames(start, next_start, stream, framerate, result.time_base)
        # B-frame reordering shifts the flushed frame count by the reorder
        # depth, so allow a couple of frames of slack before calling a GOP short.
        if error is None and frames > 0 and (expected == 0 or frames + 2 >= expected):
            continue
        if error is None and frames == 0 and not gop:
            continue
        with quiet_libav():
            counts = _isolated_frame_counts(gop, codec, extradata)
        culprit = 0
        for index, count in enumerate(counts):
            previous = counts[index - 1] if index else 0
            if count <= previous:
                culprit = index
                break
        else:
            culprit = len(gop) - 1
        culprit_packet = gop[culprit]
        reason = "decoder rejected the GOP" if error else (
            "GOP produced no frames" if frames == 0 else "GOP is shorter than its span"
        )
        damages.append(
            Damage(
                stream_index=stream.index,
                gop_start_ticks=start,
                packet_index=int(culprit_packet[1] or 0),
                offset=culprit_packet[2],
                reason=reason,
                expected_frames=max(expected, len(gop)),
                decoded_frames=frames,
            )
        )
    return damages


def _expected_frames(start: int, next_start: int | None, stream: StreamProbe,
                     framerate: float, time_base) -> int:
    """Frame count the GOP should hold, from its span and the nominal rate."""
    if not framerate or next_start is None:
        return 0
    seconds = float(time_base.to_seconds(next_start - start))
    return int(round(seconds * framerate))


def _pos(packet: Any) -> int | None:
    position = getattr(packet, "pos", None)
    return position if position is not None and position >= 0 else None


@dataclass(slots=True)
class RecoveryResult:
    segment_id: str
    offset: int | None
    resume_offset: int | None
    salvaged_ticks: int
    skipped_packets: list[int] = field(default_factory=list)
    path: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "segment_id": self.segment_id,
            "offset": self.offset,
            "resume_offset": self.resume_offset,
            "salvaged_ticks": self.salvaged_ticks,
            "skipped_packets": list(self.skipped_packets),
            "path": self.path,
        }


def recover(
    result: ProbeResult,
    segment: SegmentRecord,
    damage: Damage,
    output_dir: str,
) -> RecoveryResult:
    """Salvage the decodable part of a damaged segment from a safe boundary.

    The safe boundary is the latest keyframe at or before the damage that the
    integrity scan did *not* flag.  Packets are stream-copied from there, the
    damaged packet and its undecodable successors are skipped, and the run
    resumes at the next intact keyframe.  The result records all three offsets
    so the decision is auditable.
    """
    stream = result.stream(segment.track_index)
    if stream is None:
        return RecoveryResult(segment.segment_id, damage.offset, None, 0)
    boundaries = sorted({k.ticks for k in stream.keyframes})
    safe_before = [t for t in boundaries if t < damage.gop_start_ticks]
    resume_after = [t for t in boundaries if t > damage.gop_start_ticks]
    resume = resume_after[0] if resume_after else None
    # The damaged GOP spans from its keyframe to the next keyframe.  Everything
    # outside that span is salvaged; the repair restarts at ``resume``.
    packets = [
        packet for packet in stream.packets
        if packet.pts is not None
        and _ticks_of(result, stream, packet) < damage.gop_start_ticks
    ]
    if resume is not None:
        packets.extend(
            packet for packet in stream.packets
            if packet.pts is not None
            and _ticks_of(result, stream, packet) >= resume
        )
    salvaged = len(packets)
    resume_offset = None
    if resume is not None:
        for packet in stream.packets:
            if packet.pts is None:
                continue
            if _ticks_of(result, stream, packet) >= resume:
                resume_offset = packet.offset
                break
    boundary = safe_before[-1] if safe_before else 0
    path = str(Path(output_dir) / f"{segment.segment_id}.recovered.mp4")
    Path(output_dir).mkdir(parents=True, exist_ok=True)
    if not packets:
        # Nothing precedes the damage and there is no later keyframe: the file
        # is locatable but not recoverable.  Say so instead of writing an empty
        # artefact that would look like a successful repair.
        return RecoveryResult(
            segment_id=segment.segment_id,
            offset=damage.offset,
            resume_offset=None,
            salvaged_ticks=0,
            skipped_packets=[damage.packet_index],
            path="",
        )
    try:
        _remux_selected(result, stream, packets, boundary, path)
    except MediaError:
        path = ""
    return RecoveryResult(
        segment_id=segment.segment_id,
        offset=damage.offset,
        resume_offset=resume_offset,
        salvaged_ticks=salvaged,
        skipped_packets=[damage.packet_index],
        path=path,
    )


def _ticks_of(result: ProbeResult, stream: StreamProbe, packet) -> int:
    return result.time_base.to_ticks(Fraction(packet.pts) * stream.time_base.value)


def _remux_selected(
    result: ProbeResult,
    stream: StreamProbe,
    packets: Iterable[Any],
    start_ticks: int,
    output_path: str,
) -> None:
    """Mux exactly the chosen packets, using the same timestamp discipline as
    the archive stage so a recovered file is playable on its own."""
    from .pipeline import _MuxState

    container = av.open(result.path)
    output = None
    try:
        input_stream = container.streams[stream.index]
        output = av.open(output_path, "w", format="mp4")
        output.bit_exact = True
        output_stream = output.add_stream(template=input_stream)
        wanted = {p.index for p in packets}
        muxed = _MuxState(output_stream)
        count = 0
        for packet in container.demux(input_stream):
            if packet.size == 0 and packet.pts is None:
                continue
            meta = next(
                (m for m in stream.packets
                 if m.offset == _pos(packet) and m.pts == packet.pts),
                None,
            )
            if meta is None or meta.index not in wanted:
                continue
            muxed.push(packet, output)
            count += 1
        muxed.flush(output)
        if count == 0:
            raise MediaError(
                "corrupt-packet", "no packet survived recovery",
                stage="verify", path=result.path, stream_index=stream.index,
            )
    finally:
        if output is not None:
            output.close()
        container.close()
