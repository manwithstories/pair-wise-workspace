from __future__ import annotations

import io
import tempfile
import unittest
from pathlib import Path

import numpy as np
import soundfile as sf
from PIL import Image, ImageDraw

from mediaidx.fingerprint import (
    audio_fingerprint,
    hamming64,
    image_fingerprint,
    verify_audio_fingerprints,
    verify_image_fingerprints,
)
from mediaidx.loaders.audio_loader import decode_audio_bytes
from mediaidx.loaders.image_loader import decode_image_bytes


def make_scene(seed: int = 3) -> Image.Image:
    rng = np.random.default_rng(seed)
    width, height = 320, 240
    yy, xx = np.mgrid[0:height, 0:width]
    array = np.stack(
        [
            (xx * 255 / width + seed * 20) % 256,
            (yy * 255 / height + seed * 30) % 256,
            ((xx + yy) * 180 / (width + height)) % 256,
        ],
        axis=2,
    ).astype(np.uint8)
    image = Image.fromarray(array)
    draw = ImageDraw.Draw(image)
    for _ in range(4):
        x = int(rng.integers(10, width - 70))
        y = int(rng.integers(10, height - 60))
        size = int(rng.integers(35, 65))
        draw.ellipse((x, y, x + size, y + size), fill=tuple(int(v) for v in rng.integers(0, 255, 3)))
    return image


def image_bytes(image: Image.Image, fmt: str, **kwargs: object) -> bytes:
    buffer = io.BytesIO()
    image.save(buffer, format=fmt, **kwargs)
    return buffer.getvalue()


class ImageTests(unittest.TestCase):
    def test_cross_format_and_half_size_have_same_hash(self) -> None:
        scene = make_scene()
        encoded = [
            image_bytes(scene, "PNG"),
            image_bytes(scene, "JPEG", quality=85),
            image_bytes(scene, "WEBP", quality=80),
            image_bytes(scene.resize((160, 120), Image.Resampling.LANCZOS), "JPEG", quality=85),
        ]
        fingerprints = [image_fingerprint(decode_image_bytes(data)) for data in encoded]
        base = fingerprints[0]
        for candidate in fingerprints[1:]:
            self.assertLessEqual(hamming64(base["phash"], candidate["phash"]), 4)
            self.assertLessEqual(hamming64(base["dhash"], candidate["dhash"]), 6)
            self.assertTrue(verify_image_fingerprints(base, candidate)[0])

    def test_exif_rotation_and_alpha_flatten(self) -> None:
        scene = make_scene(9)
        upright = image_bytes(scene, "JPEG", quality=90)
        physical = scene.rotate(90, expand=True)
        exif = Image.Exif()
        exif[274] = 6
        rotated = image_bytes(physical, "JPEG", quality=90, exif=exif)
        self.assertTrue(
            verify_image_fingerprints(
                image_fingerprint(decode_image_bytes(upright)),
                image_fingerprint(decode_image_bytes(rotated)),
            )[0]
        )

        rgba = scene.convert("RGBA")
        draw = ImageDraw.Draw(rgba)
        draw.rectangle((80, 50, 220, 170), fill=(0, 0, 0, 0))
        transparent = image_bytes(rgba, "PNG")
        background = Image.new("RGBA", rgba.size, (255, 255, 255, 255))
        flattened = image_bytes(Image.alpha_composite(background, rgba).convert("RGB"), "PNG")
        self.assertTrue(
            verify_image_fingerprints(
                image_fingerprint(decode_image_bytes(transparent)),
                image_fingerprint(decode_image_bytes(flattened)),
            )[0]
        )

    def test_different_image_is_not_merged(self) -> None:
        left = image_fingerprint(decode_image_bytes(image_bytes(make_scene(3), "PNG")))
        right = image_fingerprint(decode_image_bytes(image_bytes(make_scene(7), "PNG")))
        self.assertFalse(verify_image_fingerprints(left, right)[0])


def audio_data() -> tuple[np.ndarray, int]:
    sample_rate = 44_100
    t = np.arange(int(4.0 * sample_rate)) / sample_rate
    left = 0.15 * np.sin(2 * np.pi * 220 * t) + 0.10 * np.sin(2 * np.pi * 330 * t)
    right = 0.92 * left + 0.03 * np.sin(2 * np.pi * 440 * t)
    return np.stack([left, right], axis=1).astype(np.float32), sample_rate


def write_audio_bytes(samples: np.ndarray, sample_rate: int, fmt: str) -> bytes:
    buffer = io.BytesIO()
    sf.write(buffer, samples, sample_rate, format=fmt)
    return buffer.getvalue()


class AudioTests(unittest.TestCase):
    def test_resample_downmix_gain_and_head_trim(self) -> None:
        samples, sample_rate = audio_data()
        target_rate = 48_000
        target_count = int(round(samples.shape[0] * target_rate / sample_rate))
        positions = np.arange(target_count) / target_rate
        source_positions = np.arange(samples.shape[0]) / sample_rate
        resampled = np.stack(
            [np.interp(positions, source_positions, samples[:, channel]) for channel in range(2)],
            axis=1,
        ).astype(np.float32)
        variants = {
            "wav": write_audio_bytes(samples, sample_rate, "WAV"),
            "flac48": write_audio_bytes(resampled, target_rate, "FLAC"),
            "mono_quiet": write_audio_bytes(samples.mean(axis=1) * 0.5, sample_rate, "WAV"),
            "head_trim": write_audio_bytes(samples[int(0.2 * sample_rate) :], sample_rate, "WAV"),
            "ogg": write_audio_bytes(np.clip(samples * 2, -1, 1), sample_rate, "OGG"),
        }
        fingerprints = {
            name: audio_fingerprint(decode_audio_bytes(data)) for name, data in variants.items()
        }
        base = fingerprints["wav"]
        for name, candidate in fingerprints.items():
            self.assertLessEqual(hamming64(base["profile_hash"], candidate["profile_hash"]), 8, name)
            self.assertTrue(verify_audio_fingerprints(base, candidate)[0], name)

    def test_duration_mismatch_is_rejected(self) -> None:
        samples, sample_rate = audio_data()
        long_fp = audio_fingerprint(decode_audio_bytes(write_audio_bytes(samples, sample_rate, "WAV")))
        short_fp = audio_fingerprint(
            decode_audio_bytes(write_audio_bytes(samples[: sample_rate], sample_rate, "WAV"))
        )
        same, detail = verify_audio_fingerprints(long_fp, short_fp)
        self.assertFalse(same)
        self.assertLess(detail["duration_ratio"], 0.5)

    def test_head_crop_boundary(self) -> None:
        samples, sample_rate = audio_data()
        base_fp = audio_fingerprint(decode_audio_bytes(write_audio_bytes(samples, sample_rate, "WAV")))
        cropped_250 = audio_fingerprint(
            decode_audio_bytes(write_audio_bytes(samples[int(0.25 * sample_rate) :], sample_rate, "WAV"))
        )
        cropped_400 = audio_fingerprint(
            decode_audio_bytes(write_audio_bytes(samples[int(0.40 * sample_rate) :], sample_rate, "WAV"))
        )
        self.assertTrue(verify_audio_fingerprints(base_fp, cropped_250)[0])
        self.assertFalse(verify_audio_fingerprints(base_fp, cropped_400)[0])


if __name__ == "__main__":
    unittest.main()
