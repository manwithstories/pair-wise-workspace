"""Generate normal, boundary and corrupt media fixtures with ffmpeg.

All fixtures are synthesized locally; nothing references files outside the
repository.  ``normal.mp4`` is clean CFR A/V, ``boundary.mov`` is VFR video
with audio (used for the resume demo), and ``corrupt.mp4`` is a faststart
file truncated mid-mdat so the demuxer stops far before the declared
duration.
"""

from __future__ import annotations

import argparse
import shutil
import subprocess
from pathlib import Path

VIDEO = "testsrc2=size=320x240:rate=30"


def _run(cmd: list[str]) -> None:
    subprocess.run(cmd, check=True, capture_output=True)


def make_normal(out: Path) -> Path:
    path = out / "normal.mp4"
    _run(
        [
            "ffmpeg", "-y",
            "-f", "lavfi", "-i", f"{VIDEO}:duration=20",
            "-f", "lavfi", "-i", "sine=frequency=440:duration=20",
            "-c:v", "libx264", "-g", "30", "-pix_fmt", "yuv420p",
            "-c:a", "aac", "-b:a", "96k",
            str(path),
        ]
    )
    return path


def make_boundary(out: Path) -> Path:
    """VFR video (every 7th frame dropped, original timestamps kept)."""
    path = out / "boundary.mov"
    _run(
        [
            "ffmpeg", "-y",
            "-f", "lavfi", "-i", f"{VIDEO}:duration=30",
            "-f", "lavfi", "-i", "sine=frequency=330:duration=30",
            "-vf", "select='not(mod(n\\,7))'",
            "-fps_mode", "vfr",
            "-c:v", "libx264", "-g", "15", "-pix_fmt", "yuv420p",
            "-c:a", "aac", "-b:a", "96k",
            str(path),
        ]
    )
    return path


def make_corrupt(out: Path) -> Path:
    """A faststart file truncated to 60%: opens fine, ends mid-stream."""
    whole = out / "_corrupt_source.mp4"
    _run(
        [
            "ffmpeg", "-y",
            "-f", "lavfi", "-i", f"{VIDEO}:duration=15",
            "-f", "lavfi", "-i", "sine=frequency=550:duration=15",
            "-c:v", "libx264", "-g", "30", "-pix_fmt", "yuv420p",
            "-c:a", "aac", "-b:a", "96k",
            "-movflags", "+faststart",
            str(whole),
        ]
    )
    path = out / "corrupt.mp4"
    data = whole.read_bytes()
    path.write_bytes(data[: int(len(data) * 0.6)])
    whole.unlink()
    return path


def make_all(out: str | Path) -> list[Path]:
    out = Path(out)
    if shutil.which("ffmpeg") is None:
        raise SystemExit("ffmpeg not found; install with: brew install ffmpeg")
    out.mkdir(parents=True, exist_ok=True)
    paths = [make_normal(out), make_boundary(out), make_corrupt(out)]
    return paths


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", required=True)
    args = parser.parse_args(argv)
    for path in make_all(args.out):
        print(f"fixture={path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
