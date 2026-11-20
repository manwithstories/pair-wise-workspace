#!/usr/bin/env python3
"""Generate the full local fixture set for archive recovery acceptance.

Run with the project virtualenv::

    .venv/bin/python scripts/make_fixtures.py --out ./.tmp-fixtures

Every file is generated from seeded tones; no network or external media is
touched.  The output directory contains clean, damaged, truncated,
out-of-order and timestamp-collision playlists in both HLS and DASH form,
plus a missing-segment playlist.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from fixture_media import (  # noqa: E402
    corrupt_packet_region,
    encode_adts,
    encode_fragmented_mp4,
    flip_bytes,
    tone_stereo,
)

SEGMENT_SECONDS = 1.0
HLS_RATES = (44100, 48000, 44100, 48000, 44100, 48000)
DASH_RATE = 48000
SEGMENT_COUNT = 6
DASH_COUNT = 4


def _segment_name(index: int) -> str:
    return f"seg-{index:03d}.aac"


def write_hls_manifest(
    path: Path,
    entries: list[tuple[str, float]],
    *,
    representation: str = "r0",
    include_durations: bool = True,
    starts: list[float] | None = None,
) -> None:
    lines = [
        "#EXTM3U",
        "#EXT-X-VERSION:3",
        "#EXT-X-TARGETDURATION:1",
        f"#ARCHIVE-REP:{representation}",
        "#ARCHIVE-TIMEBASE:90000",
    ]
    for index, (ref, duration) in enumerate(entries):
        if starts is not None:
            lines.append(f"#ARCHIVE-START:{starts[index]:.9f}")
        if include_durations:
            lines.append(f"#EXTINF:{duration:.6f},")
        lines.append(ref)
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def write_dash_manifest(
    path: Path, entries: list[tuple[int, int]], *, timescale: int = DASH_RATE
) -> None:
    ns = "urn:mpeg:dash:schema:mpd:2011"
    s_tags = "".join(f'<S t="{t}" d="{d}"/>' for t, d in entries)
    xml = (
        f'<MPD xmlns="{ns}"><AdaptationSet>'
        f'<SegmentTemplate timescale="{timescale}" '
        f'media="dash-seg-$RepresentationID$-$Number$.m4s" startNumber="0">'
        f"<SegmentTimeline>{s_tags}</SegmentTimeline></SegmentTemplate>"
        f'<Representation id="r0"/></AdaptationSet></MPD>'
    )
    path.write_text(xml + "\n", encoding="utf-8")


def _build_hls_segments(out: Path) -> list[str]:
    refs: list[str] = []
    for index, rate in enumerate(HLS_RATES):
        ref = _segment_name(index)
        samples = tone_stereo(rate, SEGMENT_SECONDS, 330.0 + 55.0 * index)
        encode_adts(out / ref, samples, rate)
        refs.append(ref)
    return refs


def _copy_segments(out: Path, subdir: str, refs: list[str]) -> list[str]:
    """Materialize an isolated per-playlist copy of the referenced segments."""
    target = out / subdir
    target.mkdir(parents=True, exist_ok=True)
    copied: list[str] = []
    for ref in refs:
        data = (out / ref).read_bytes()
        local = Path(subdir) / ref
        (out / local).write_bytes(data)
        copied.append(str(local))
    return copied


def build_hls(out: Path) -> None:
    refs = _build_hls_segments(out)
    entries = [(ref, SEGMENT_SECONDS) for ref in refs]
    write_hls_manifest(out / "clean.m3u8", entries)

    # Damaged playlist gets an isolated segment copy so the clean fixtures
    # stay byte-identical to what the encoder produced.
    damaged_refs = _copy_segments(out, "damaged", refs)
    corrupt_packet_region(out / damaged_refs[2], keep_fraction=0.55)
    flip_bytes(out / damaged_refs[4])
    write_hls_manifest(
        out / "damaged.m3u8",
        [(ref, SEGMENT_SECONDS) for ref in damaged_refs],
    )
    text = (out / "damaged.m3u8").read_text()
    text = text.replace(
        f"#EXTINF:{SEGMENT_SECONDS:.6f},\n{damaged_refs[3]}", damaged_refs[3]
    )
    (out / "damaged.m3u8").write_text(text)

    # Truncated-only playlist used by the --strict fatal demo.
    trunc_refs = _copy_segments(out, "truncated", refs[:4])
    corrupt_packet_region(out / trunc_refs[1], keep_fraction=0.35)
    write_hls_manifest(out / "truncated.m3u8",
                       [(ref, SEGMENT_SECONDS) for ref in trunc_refs])

    # Out-of-order references: the playlist lists segment 4 before segment 1
    # while keeping each segment's original programme timestamp, so the
    # timeline planner sees a backwards jump rather than a simple re-index.
    order = [0, 4, 1, 2, 3, 5]
    reordered = [entries[i] for i in order]
    starts = [float(i) for i in order]
    write_hls_manifest(out / "reordered.m3u8", reordered, starts=starts)

    # Timestamp rounding collision: two segments with starts that differ by
    # less than one 90 kHz tick but are distinct rational times.
    collision_entries = list(entries[:3])
    write_hls_manifest(
        out / "collision.m3u8",
        collision_entries,
        starts=[0.0, 1.0, 1.0 + 1e-9],
    )

    # Missing segment: a reference whose file was never restored.
    missing = list(entries[:4])
    missing.append(("seg-missing.aac", SEGMENT_SECONDS))
    write_hls_manifest(out / "missing.m3u8", missing)


def build_dash(out: Path) -> None:
    refs: list[str] = []
    for index in range(DASH_COUNT):
        ref = f"dash-seg-r0-{index}.m4s"
        samples = tone_stereo(DASH_RATE, SEGMENT_SECONDS, 300.0 + 40.0 * index)
        encode_fragmented_mp4(out / ref, samples, DASH_RATE)
        refs.append(ref)
    tick = DASH_RATE
    timeline = [(index * tick, tick) for index in range(DASH_COUNT)]
    write_dash_manifest(out / "clean.mpd", timeline)

    # Isolated damaged copy: one S entry drops its t, one segment is truncated.
    subdir = Path("dash-damaged")
    (out / subdir).mkdir(parents=True, exist_ok=True)
    damaged_refs = []
    for ref in refs:
        local = str(subdir / ref)
        (out / local).write_bytes((out / ref).read_bytes())
        damaged_refs.append(local)
    corrupt_packet_region(out / damaged_refs[2], keep_fraction=0.5)
    ns = "urn:mpeg:dash:schema:mpd:2011"
    s_tags = (
        f'<S t="0" d="{tick}"/>'
        f'<S t="{tick}" d="{tick}"/>'
        f'<S d="{tick}"/>'
        f'<S t="{3 * tick}" d="{tick}"/>'
    )
    xml = (
        f'<MPD xmlns="{ns}"><AdaptationSet>'
        f'<SegmentTemplate timescale="{DASH_RATE}" '
        f'media="dash-damaged/dash-seg-$RepresentationID$-$Number$.m4s" '
        'startNumber="0">'
        f"<SegmentTimeline>{s_tags}</SegmentTimeline></SegmentTemplate>"
        f'<Representation id="r0"/></AdaptationSet></MPD>'
    )
    (out / "damaged.mpd").write_text(xml + "\n")

    # Distinct t values that collide at a coarser effective tick spacing.
    collision_timeline = [(0, tick), (tick, tick), (tick, tick)]
    write_dash_manifest(out / "collision.mpd", collision_timeline)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, help="output fixture directory")
    args = parser.parse_args()
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    build_hls(out)
    build_dash(out)
    generated = sorted(p.name for p in out.iterdir())
    print(f"generated {len(generated)} files in {out}")
    for name in generated:
        print(f"  {name}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
