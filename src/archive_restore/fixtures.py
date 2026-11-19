"""Generate local HLS/DASH fixture assets with FFmpeg (no external files).

Scenarios produced under the output root:

* ``normal``    – two video bitrates + one audio track, fully aligned
* ``missing``   – one segment deleted (explicit gap)
* ``duplicate`` – the same segment referenced twice at one time range
* ``outoforder``– media playlist lists segments out of sequence
* ``truncated`` – playlist cut short and a torn segment file
* ``rollback``  – a segment whose start timestamp jumps backwards

Every media file is synthesized locally with the ``ffmpeg`` CLI; PyAV and
soundfile can decode the results, so downstream stages read real timestamps.
"""

from __future__ import annotations

import argparse
import json
import shutil
import subprocess
from pathlib import Path

SEGMENT_US = 2_000_000
N_SEGMENTS = 4
WIDTH, HEIGHT = 320, 180
FPS = 25
SAMPLE_RATE = 48_000


def _ffmpeg(args: list[str]) -> None:
    cmd = ["ffmpeg", "-hide_banner", "-loglevel", "error", "-y", *args]
    subprocess.run(cmd, check=True, capture_output=True)


def _base_video(path: Path, duration: float, bitrate: str) -> None:
    _ffmpeg(["-f", "lavfi", "-i",
             f"testsrc=size={WIDTH}x{HEIGHT}:rate={FPS}:duration={duration}",
             "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
             "-g", str(FPS * 2), "-keyint_min", str(FPS * 2),
             "-b:v", bitrate, str(path)])


def _base_audio(path: Path, duration: float) -> None:
    _ffmpeg(["-f", "lavfi", "-i", f"sine=frequency=440:sample_rate={SAMPLE_RATE}:duration={duration}",
             "-c:a", "pcm_s16le", str(path)])


def _split_segments(src: Path, out_dir: Path, prefix: str, kind: str) -> list[Path]:
    out_dir.mkdir(parents=True, exist_ok=True)
    if kind == "video":
        pattern = str(out_dir / f"{prefix}%01d.ts")
        # Re-encode and force a keyframe at every 2s cut so each segment
        # starts on a keyframe at PTS 0 (reliable alignment boundaries).
        _ffmpeg(["-i", str(src), "-c:v", "libx264", "-preset", "ultrafast",
                 "-pix_fmt", "yuv420p", "-g", str(FPS * 2),
                 "-force_key_frames", "expr:gte(t,n_forced*2)",
                 "-f", "segment", "-segment_time", "2",
                 "-reset_timestamps", "1", pattern])
        return sorted(out_dir.glob(f"{prefix}*.ts"))
    pattern = str(out_dir / f"{prefix}%01d.wav")
    _ffmpeg(["-i", str(src), "-f", "segment", "-segment_time", "2", pattern])
    return sorted(out_dir.glob(f"{prefix}*.wav"))


def _media_playlist(segments: list[Path], name: str = "media.m3u8") -> str:
    lines = ["#EXTM3U", "#EXT-X-TARGETDURATION:2", "#EXT-X-MEDIA-SEQUENCE:0"]
    for seg in segments:
        lines += ["#EXTINF:2.0,", seg.name]
    lines.append("#EXT-X-ENDLIST")
    return "\n".join(lines) + "\n"


def _write_hls(asset_dir: Path, video_dirs: dict[str, list[Path]],
               audio: list[Path]) -> None:
    master = ["#EXTM3U"]
    for idx, (label, segs) in enumerate(video_dirs.items()):
        (asset_dir / label).mkdir(exist_ok=True)
        (asset_dir / label / "media.m3u8").write_text(_media_playlist(segs))
        master.append(f'#EXT-X-STREAM-INF:BANDWIDTH={(idx + 1) * 200000},CODECS="avc1.64001e"')
        master.append(f"{label}/media.m3u8")
    if audio:
        (asset_dir / "audio").mkdir(exist_ok=True)
        (asset_dir / "audio" / "media.m3u8").write_text(_media_playlist(audio))
        master.append('#EXT-X-MEDIA:TYPE=AUDIO,NAME="a0",URI="audio/media.m3u8",CODECS="mp4a.40.2"')
    (asset_dir / "master.m3u8").write_text("\n".join(master) + "\n")


def _build_normal(root: Path) -> None:
    asset = root / "normal"
    work = root / ".src"
    work.mkdir(parents=True, exist_ok=True)
    duration = N_SEGMENTS * 2
    video_dirs: dict[str, list[Path]] = {}
    for label, bitrate in (("v0", "200k"), ("v1", "400k")):
        base = work / f"{label}.mp4"
        _base_video(base, duration, bitrate)
        video_dirs[label] = _split_segments(base, asset / label, "seg", "video")
    abase = work / "a0.wav"
    _base_audio(abase, duration)
    audio = _split_segments(abase, asset / "audio", "seg", "audio")
    _write_hls(asset, video_dirs, audio)


def _clone_normal(root: Path, name: str) -> Path:
    src, dst = root / "normal", root / name
    if dst.exists():
        shutil.rmtree(dst)
    shutil.copytree(src, dst)
    return dst


def _build_missing(root: Path) -> None:
    dst = _clone_normal(root, "missing")
    victim = sorted((dst / "v0").glob("seg*.ts"))[1]
    victim.unlink()
    playlist = dst / "v0" / "media.m3u8"
    # Keep the EXTINF but drop the URI: an explicit dangling/missing segment.
    text = playlist.read_text().replace(f"#EXTINF:2.0,\n{victim.name}\n",
                                        "#EXTINF:2.0,\n")
    playlist.write_text(text)


def _build_duplicate(root: Path) -> None:
    dst = _clone_normal(root, "duplicate")
    playlist = dst / "v0" / "media.m3u8"
    segs = sorted((dst / "v0").glob("seg*.ts"))
    text = playlist.read_text()
    dup = f"#EXTINF:2.0,\n{segs[0].name}\n"
    playlist.write_text(text.replace(dup, dup + dup, 1))


def _build_outoforder(root: Path) -> None:
    dst = _clone_normal(root, "outoforder")
    playlist = dst / "v0" / "media.m3u8"
    segs = sorted((dst / "v0").glob("seg*.ts"))
    names = [s.name for s in segs]
    names[0], names[1] = names[1], names[0]
    lines = ["#EXTM3U", "#EXT-X-TARGETDURATION:2", "#EXT-X-MEDIA-SEQUENCE:0"]
    for n in names:
        lines += ["#EXTINF:2.0,", n]
    playlist.write_text("\n".join(lines) + "\n#EXT-X-ENDLIST\n")


def _build_truncated(root: Path) -> None:
    dst = _clone_normal(root, "truncated")
    playlist = dst / "v0" / "media.m3u8"
    lines = playlist.read_text().splitlines()
    keep = [ln for ln in lines if ln != "#EXT-X-ENDLIST"][:-1]
    playlist.write_text("\n".join(keep) + "\n")
    torn = sorted((dst / "v0").glob("seg*.ts"))[-1]
    data = torn.read_bytes()
    torn.write_bytes(data[: max(1, len(data) // 3)])


def _build_rollback(root: Path) -> None:
    dst = _clone_normal(root, "rollback")
    segs = sorted((dst / "v0").glob("seg*.ts"))
    # Re-stamp a later segment so its timeline restarts near zero (rollback).
    target = segs[2]
    _ffmpeg(["-i", str(segs[2]), "-c", "copy", "-output_ts_offset", "0",
             "-f", "mpegts", str(target.with_suffix(".rb.ts"))])
    target.with_suffix(".rb.ts").replace(target)


def build_all(out_root: Path) -> dict:
    if out_root.exists():
        shutil.rmtree(out_root)
    out_root.mkdir(parents=True)
    _build_normal(out_root)
    _build_missing(out_root)
    _build_duplicate(out_root)
    _build_outoforder(out_root)
    _build_truncated(out_root)
    _build_rollback(out_root)
    scenarios = {p.name: str(p / "master.m3u8")
                 for p in sorted(out_root.iterdir()) if p.is_dir() and p.name != ".src"}
    manifest = {"generated": scenarios, "segment_us": SEGMENT_US}
    (out_root / "fixtures.json").write_text(json.dumps(manifest, indent=2))
    shutil.rmtree(out_root / ".src", ignore_errors=True)
    return manifest


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate HLS/DASH restoration fixtures")
    parser.add_argument("--out", required=True, type=Path)
    args = parser.parse_args(argv)
    manifest = build_all(args.out)
    print(json.dumps(manifest, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
