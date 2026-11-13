"""Generate a deterministic, fully synthetic media corpus and truth manifest.

The corpus intentionally contains misleading extensions, hidden directories,
zero-byte files, symlinks and corrupt files in addition to positive variants
and hard negatives.  No external media files are referenced.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import tempfile
import warnings
from pathlib import Path
from typing import Iterable

import numpy as np
import soundfile as sf
from PIL import Image, ImageDraw


BASE_DIR = Path(__file__).resolve().parent
IMAGE_SIZE = (640, 480)


def ffmpeg_executable() -> str:
    try:
        with warnings.catch_warnings():
            warnings.simplefilter("ignore", UserWarning)
            import imageio_ffmpeg

        return imageio_ffmpeg.get_ffmpeg_exe()
    except Exception:
        executable = shutil.which("ffmpeg")
        if executable is None:
            raise RuntimeError("ffmpeg is required to generate video fixtures")
        return executable


FFMPEG = ffmpeg_executable()


def run_ffmpeg(arguments: Iterable[str], *, allow_failure: bool = False) -> bool:
    try:
        process = subprocess.run(
            [FFMPEG, "-y", "-hide_banner", "-loglevel", "error", *arguments],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except OSError as exc:
        if allow_failure:
            return False
        raise RuntimeError(f"ffmpeg could not start: {exc}") from exc
    if process.returncode != 0:
        if allow_failure:
            return False
        message = process.stderr.decode("utf-8", errors="replace")
        raise RuntimeError(f"ffmpeg failed: {message[-1000:]}")
    return True


def image_scene(seed: int) -> Image.Image:
    width, height = IMAGE_SIZE
    yy, xx = np.mgrid[0:height, 0:width]
    array = np.stack(
        [
            (xx * 255 / width + 23 * seed) % 256,
            (yy * 255 / height + 47 * seed) % 256,
            ((xx + yy) * 170 / (width + height) + 71 * seed) % 256,
        ],
        axis=2,
    ).astype(np.uint8)
    image = Image.fromarray(array)
    draw = ImageDraw.Draw(image)
    rng = np.random.default_rng(1000 + seed)
    for index in range(6):
        x = int(rng.integers(25, width - 120))
        y = int(rng.integers(20, height - 110))
        size = int(rng.integers(45, 135))
        color = tuple(int(value) for value in rng.integers(10, 245, 3))
        if index % 2:
            draw.ellipse((x, y, x + size, y + size), fill=color)
        else:
            draw.rectangle((x, y, x + size, y + size), fill=color, outline=(255, 255, 255))
    draw.line(
        (0, int(rng.integers(40, height - 40)), width, int(rng.integers(40, height - 40))),
        fill=(250, 250, 250),
        width=8,
    )
    return image


def image_negative_scene(seed: int) -> Image.Image:
    # Same general visual language as the positives, but spatial layout and
    # large low-frequency shapes are deliberately different.
    width, height = IMAGE_SIZE
    yy, xx = np.mgrid[0:height, 0:width]
    array = np.stack(
        [
            (yy * 255 / height + 19 * seed) % 256,
            (xx * 255 / width + 83 * seed) % 256,
            ((width - xx + yy) * 150 / (width + height) + 57 * seed) % 256,
        ],
        axis=2,
    ).astype(np.uint8)
    image = Image.fromarray(array)
    draw = ImageDraw.Draw(image)
    for index in range(5):
        x = 40 + index * 105
        draw.ellipse((x, 280 - index * 20, x + 90, 380 - index * 20), fill=(20 + index * 35, 210, 240 - index * 25))
        draw.rectangle((x, 40 + index * 24, x + 70, 170 + index * 24), fill=(240 - index * 30, 45, 180))
    return image


def save_image(image: Image.Image, path: Path, fmt: str, **kwargs: object) -> bytes:
    path.parent.mkdir(parents=True, exist_ok=True)
    image.save(path, format=fmt, **kwargs)
    return path.read_bytes()


def save_image_bytes(data: bytes, path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)


def synthesize_audio(seed: int, duration: float = 4.0, sample_rate: int = 44_100) -> np.ndarray:
    t = np.arange(int(round(duration * sample_rate))) / sample_rate
    rng = np.random.default_rng(3000 + seed)
    if seed % 2 == 0:
        base = [220.0, 330.0, 520.0]
    else:
        base = [180.0, 410.0, 780.0]
    left = np.zeros_like(t)
    right = np.zeros_like(t)
    for index, frequency in enumerate(base):
        modulation = 1.0 + (0.35 + 0.1 * index) * np.sin(2 * np.pi * (1.3 + index) * t + seed)
        part = (0.12 - 0.02 * index) * np.sin(2 * np.pi * frequency * t + 0.2 * seed) * modulation
        left += part
        right += np.roll(part, index + 1) * (0.92 + 0.03 * index)
    noise = rng.normal(0.0, 0.004, t.size)
    left += noise
    right += np.roll(noise, 7) * 0.8
    return np.stack([left, right], axis=1).astype(np.float32)


def audio_negative(seed: int, duration: float = 4.0, sample_rate: int = 44_100) -> np.ndarray:
    t = np.arange(int(round(duration * sample_rate))) / sample_rate
    left = (
        0.13 * np.sin(2 * np.pi * 145 * t)
        + 0.10 * np.sin(2 * np.pi * 610 * t + 0.7)
        + 0.06 * np.sin(2 * np.pi * 930 * t) * (1 + 0.5 * np.sin(2 * np.pi * 4 * t))
    )
    right = np.roll(left, 113) * 0.85
    return np.stack([left, right], axis=1).astype(np.float32)


def resample_for_fixture(samples: np.ndarray, source_rate: int, target_rate: int) -> np.ndarray:
    if source_rate == target_rate:
        return samples
    source_positions = np.arange(samples.shape[0]) / source_rate
    target_count = int(round(samples.shape[0] * target_rate / source_rate))
    target_positions = np.arange(target_count) / target_rate
    return np.stack(
        [np.interp(target_positions, source_positions, samples[:, channel]) for channel in range(samples.shape[1])],
        axis=1,
    ).astype(np.float32)


def write_audio(samples: np.ndarray, sample_rate: int, path: Path, fmt: str, subtype: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    sf.write(path, samples, sample_rate, format=fmt, subtype=subtype)


def encode_ogg_128k(source_wav: Path, destination: Path) -> None:
    destination.parent.mkdir(parents=True, exist_ok=True)
    attempts = [
        ["-i", str(source_wav), "-c:a", "libvorbis", "-b:a", "128k", str(destination)],
        ["-i", str(source_wav), "-c:a", "vorbis", "-strict", "-2", "-b:a", "128k", str(destination)],
    ]
    for arguments in attempts:
        if run_ffmpeg(arguments, allow_failure=True):
            return
    # Last-resort deterministic fallback for environments without a Vorbis
    # encoder in ffmpeg.  soundfile/libvorbis still writes a valid OGG file.
    samples, sample_rate = sf.read(source_wav, dtype="float32", always_2d=True)
    write_audio(samples, sample_rate, destination, "OGG", "VORBIS")


def video_frames(seed: int, count: int = 32, size: tuple[int, int] = (320, 180)) -> list[Image.Image]:
    width, height = size
    rng = np.random.default_rng(5000 + seed)
    frames: list[Image.Image] = []
    color_a = tuple(int(value) for value in rng.integers(30, 230, 3))
    color_b = tuple(int(value) for value in rng.integers(30, 230, 3))
    for frame_index in range(count):
        yy, xx = np.mgrid[0:height, 0:width]
        array = np.stack(
            [
                (xx * 235 / width + 11 * seed + frame_index * 2) % 256,
                (yy * 235 / height + 29 * seed + frame_index * 3) % 256,
                np.full((height, width), (90 + seed * 37 + frame_index * 5) % 256),
            ],
            axis=2,
        ).astype(np.uint8)
        image = Image.fromarray(array)
        draw = ImageDraw.Draw(image)
        angle = 2.0 * np.pi * frame_index / count
        cx = width / 2 + width * 0.29 * np.cos(angle + seed * 0.2)
        cy = height / 2 + height * 0.27 * np.sin(angle * (1 + seed * 0.03))
        radius = 25 + 9 * np.sin(angle * 2)
        draw.ellipse((cx - radius, cy - radius, cx + radius, cy + radius), fill=color_a)
        draw.rectangle(
            (12 + (frame_index * 4 + seed * 13) % (width - 130), 12, 90 + (frame_index * 4 + seed * 13) % (width - 130), 55),
            fill=color_b,
        )
        draw.line((0, height * (0.75 + 0.15 * np.sin(angle)), width, height * (0.7 + 0.2 * np.cos(angle))), fill=(250, 250, 250), width=5)
        frames.append(image)
    return frames


def encode_video(
    frames: list[Image.Image],
    destination: Path,
    *,
    codec_args: list[str],
    extra_args: list[str] | None = None,
    trim: tuple[float, float] | None = None,
) -> None:
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="mediaidx-fixture-") as temporary:
        frame_dir = Path(temporary)
        for index, frame in enumerate(frames):
            frame.save(frame_dir / f"{index:04d}.png")
        arguments = ["-framerate", "8", "-i", str(frame_dir / "%04d.png")]
        if trim is not None:
            arguments.extend(["-ss", str(trim[0]), "-t", str(trim[1])])
        if extra_args:
            arguments.extend(extra_args)
        arguments.extend(codec_args)
        arguments.append(str(destination))
        try:
            run_ffmpeg(arguments)
        except RuntimeError:
            if destination.exists():
                destination.unlink()
            raise


def _exif_oriented_jpeg(scene: Image.Image, path: Path) -> None:
    physical = scene.rotate(90, expand=True)
    exif = Image.Exif()
    exif[274] = 6
    path.parent.mkdir(parents=True, exist_ok=True)
    physical.save(path, format="JPEG", quality=88, exif=exif)


def generate(root: Path, manifest: Path, *, force: bool = False) -> dict[str, object]:
    if root.exists():
        if not force:
            raise FileExistsError(f"corpus already exists (use --force): {root}")
        shutil.rmtree(root)
    root.mkdir(parents=True, exist_ok=True)
    assets: dict[str, list[str]] = {}

    def label(asset_id: str, paths: list[Path]) -> None:
        assets[asset_id] = [path.relative_to(root).as_posix() for path in paths]

    # ---------------- Images ----------------
    scene_a = image_scene(3)
    paths_a: list[Path] = []
    master = root / "delivery/images/a_master.png"
    master_bytes = save_image(scene_a, master, "PNG")
    paths_a.append(master)
    jpeg = root / "delivery/images/a_flat.jpg"
    save_image(scene_a, jpeg, "JPEG", quality=86, optimize=True)
    paths_a.append(jpeg)
    webp = root / "delivery/images/a_proxy.webp"
    save_image(scene_a, webp, "WEBP", quality=82)
    paths_a.append(webp)
    half = root / "delivery/images/a_half.jpg"
    save_image(scene_a.resize((320, 240), Image.Resampling.LANCZOS), half, "JPEG", quality=84)
    paths_a.append(half)
    # A misleading extension and a byte-identical copy exercise magic-only
    # dispatch and decode-cache reuse without relying on names.
    mislabeled = root / "delivery/images/a_delivery.mov"
    save_image_bytes(master_bytes, mislabeled)
    paths_a.append(mislabeled)
    exif_jpeg = root / "delivery/images/a_exif.jpg"
    _exif_oriented_jpeg(scene_a, exif_jpeg)
    paths_a.append(exif_jpeg)
    label("image_a", paths_a)

    scene_b = image_scene(11)
    paths_b: list[Path] = []
    b_png = root / "delivery/images/b_frame.png"
    save_image(scene_b, b_png, "PNG")
    paths_b.append(b_png)
    b_webp = root / "delivery/images/b_frame.dat"
    save_image(scene_b, b_webp, "WEBP", quality=78)
    paths_b.append(b_webp)
    b_half = root / "delivery/images/b_half.png"
    save_image(scene_b.resize((360, 270), Image.Resampling.LANCZOS), b_half, "PNG")
    paths_b.append(b_half)
    label("image_b", paths_b)

    # Hard negative: close visual language, different actual image.
    negative = root / "delivery/images/negative_01.jpg"
    save_image(image_negative_scene(4), negative, "JPEG", quality=88)
    label("image_negative_01", [negative])

    # Transparency: both variants normalize to the same white-composited RGB.
    alpha_scene = scene_b.convert("RGBA")
    alpha_draw = ImageDraw.Draw(alpha_scene)
    alpha_draw.ellipse((180, 100, 430, 340), fill=(0, 0, 0, 0))
    alpha_path = root / "delivery/images/alpha_cutout.png"
    save_image(alpha_scene, alpha_path, "PNG")
    white_scene = alpha_scene.copy()
    white_background = Image.new("RGBA", white_scene.size, (255, 255, 255, 255))
    flattened = Image.alpha_composite(white_background, white_scene).convert("RGB")
    flat_path = root / "delivery/images/alpha_flattened.jpg"
    save_image(flattened, flat_path, "JPEG", quality=90)
    label("image_alpha", [alpha_path, flat_path])

    # ---------------- Audio ----------------
    audio_a = synthesize_audio(2)
    paths_audio_a: list[Path] = []
    wav_a = root / "delivery/audio/a_master.wav"
    write_audio(audio_a, 44_100, wav_a, "WAV", "PCM_24")
    paths_audio_a.append(wav_a)
    flac_a = root / "delivery/audio/a_lossless.flac"
    resampled = resample_for_fixture(audio_a, 44_100, 48_000)
    write_audio(resampled, 48_000, flac_a, "FLAC", "PCM_16")
    paths_audio_a.append(flac_a)
    ogg_a = root / "delivery/audio/a_web.ogg"
    with tempfile.TemporaryDirectory(prefix="mediaidx-audio-") as temporary:
        boosted_wav = Path(temporary) / "boosted.wav"
        write_audio(np.clip(audio_a * 2.0, -1.0, 1.0), 44_100, boosted_wav, "WAV", "PCM_16")
        encode_ogg_128k(boosted_wav, ogg_a)
    paths_audio_a.append(ogg_a)
    mono_a = root / "delivery/audio/a_mono.wav"
    mono = np.clip(audio_a * 0.5, -1.0, 1.0).mean(axis=1)
    write_audio(mono, 44_100, mono_a, "WAV", "PCM_16")
    paths_audio_a.append(mono_a)
    trim_a = root / "delivery/audio/a_head_trim.wav"
    write_audio(audio_a[int(0.2 * 44_100) :], 44_100, trim_a, "WAV", "PCM_16")
    paths_audio_a.append(trim_a)
    label("audio_a", paths_audio_a)

    audio_b = synthesize_audio(5)
    wav_b = root / "delivery/audio/b_take.wav"
    write_audio(audio_b, 44_100, wav_b, "WAV", "PCM_16")
    flac_b = root / "delivery/audio/b_take.bin"
    write_audio(audio_b, 44_100, flac_b, "FLAC", "PCM_16")
    label("audio_b", [wav_b, flac_b])

    audio_neg = root / "delivery/audio/negative_01.wav"
    write_audio(audio_negative(1), 44_100, audio_neg, "WAV", "PCM_16")
    label("audio_negative_01", [audio_neg])

    # ---------------- Video ----------------
    video_a_frames = video_frames(1)
    paths_video_a: list[Path] = []
    mp4_a = root / "delivery/video/a_cut.mp4"
    encode_video(
        video_a_frames,
        mp4_a,
        codec_args=["-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "19"],
    )
    paths_video_a.append(mp4_a)
    mov_a = root / "delivery/video/a_grading.mov"
    encode_video(
        video_a_frames,
        mov_a,
        codec_args=["-c:v", "prores_ks", "-profile:v", "3", "-pix_fmt", "yuv422p10le"],
    )
    paths_video_a.append(mov_a)
    mkv_a = root / "delivery/video/a_review.mkv"
    encode_video(
        video_a_frames,
        mkv_a,
        codec_args=["-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "22"],
        extra_args=["-vf", "scale=160:90"],
    )
    paths_video_a.append(mkv_a)
    trimmed_a = root / "delivery/video/a_one_second_crop.mp4"
    encode_video(
        video_a_frames,
        trimmed_a,
        codec_args=["-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "19"],
        trim=(1.0, 2.0),
    )
    paths_video_a.append(trimmed_a)
    label("video_a", paths_video_a)

    video_b_frames = video_frames(2, count=24)
    mp4_b = root / "delivery/video/b_cut.mp4"
    encode_video(
        video_b_frames,
        mp4_b,
        codec_args=["-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "20"],
    )
    mkv_b = root / "delivery/video/b_review.mkv"
    encode_video(
        video_b_frames,
        mkv_b,
        codec_args=["-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "22"],
        extra_args=["-vf", "scale=192:108"],
    )
    label("video_b", [mp4_b, mkv_b])

    video_neg_frames = video_frames(7, count=24)
    negative_video = root / "delivery/video/negative_01.mkv"
    encode_video(
        video_neg_frames,
        negative_video,
        codec_args=["-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "22"],
        extra_args=["-vf", "scale=192:108"],
    )
    label("video_negative_01", [negative_video])

    # ---------------- Adversarial scan cases ----------------
    hidden_dir = root / ".hidden"
    hidden_dir.mkdir(parents=True, exist_ok=True)
    save_image(scene_b, hidden_dir / "should_be_skipped.png", "PNG")
    (root / "queue").mkdir(parents=True, exist_ok=True)
    (root / "queue" / "empty.bin").write_bytes(b"")
    (root / "queue" / "notes.txt").write_text("not a media file\n", encoding="utf-8")
    try:
        (root / "delivery" / "symlink_to_master.png").symlink_to(master)
        (root / "delivery" / "loop_cycle").symlink_to(root, target_is_directory=True)
    except OSError:
        pass

    corrupt = root / "quarantine"
    corrupt.mkdir(parents=True, exist_ok=True)
    broken_png = b"\x89PNG\r\n\x1a\n" + bytes(range(64))
    (corrupt / "broken.png").write_bytes(broken_png)
    broken_flac = b"fLaC" + bytes(range(128))
    (corrupt / "broken.flac").write_bytes(broken_flac)
    broken_mp4 = b"\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2" + bytes(range(64))
    (corrupt / "broken.mp4").write_bytes(broken_mp4)

    manifest.parent.mkdir(parents=True, exist_ok=True)
    truth = {
        "description": "Synthetic labels; filenames are intentionally non-semantic.",
        "assets": assets,
    }
    manifest.write_text(json.dumps(truth, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return {
        "root": str(root),
        "manifest": str(manifest),
        "assets": len(assets),
        "labeled_files": sum(len(paths) for paths in assets.values()),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=BASE_DIR / "corpus")
    parser.add_argument("--manifest", type=Path, default=None)
    parser.add_argument("--force", action="store_true")
    args = parser.parse_args()
    root = args.root.resolve()
    manifest = args.manifest or root.parent / f"{root.name}.labels.json"
    result = generate(root, manifest.resolve(), force=args.force)
    print(json.dumps(result, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
