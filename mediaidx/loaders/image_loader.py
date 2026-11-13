"""PNG/JPEG/WebP decoding and the shared image normalization contract."""

from __future__ import annotations

import io
from dataclasses import dataclass
from pathlib import Path

import numpy as np
from PIL import Image, ImageOps, UnidentifiedImageError

from mediaidx.canonical import IMAGE_FORMATS, MediaDecodeError, MediaFamily


@dataclass
class NormalizedImage:
    rgb: np.ndarray
    source_width: int
    source_height: int
    decoded_format: str

    def metadata(self) -> dict[str, object]:
        return {
            "width": int(self.rgb.shape[1]),
            "height": int(self.rgb.shape[0]),
            "source_width": self.source_width,
            "source_height": self.source_height,
            "decoded_format": self.decoded_format,
        }


def _flatten_on_white(image: Image.Image) -> Image.Image:
    if "A" in image.getbands() or "transparency" in image.info:
        rgba = image.convert("RGBA")
        background = Image.new("RGBA", rgba.size, (255, 255, 255, 255))
        rgba = Image.alpha_composite(background, rgba)
        return rgba.convert("RGB")
    return image.convert("RGB")


def normalize_image(image: Image.Image) -> np.ndarray:
    """Apply the canonical image contract to a Pillow image."""

    image = ImageOps.exif_transpose(image)
    image = _flatten_on_white(image)
    return np.asarray(image, dtype=np.uint8)


def normalize_rgb_array(rgb: np.ndarray) -> np.ndarray:
    """Normalize an already decoded RGB byte array (used by the video layer)."""

    if rgb.dtype != np.uint8:
        rgb = np.clip(rgb, 0, 255).astype(np.uint8)
    if rgb.ndim == 2:
        return np.repeat(rgb[:, :, None], 3, axis=2)
    if rgb.ndim != 3 or rgb.shape[2] not in (3, 4):
        raise MediaDecodeError("decode_error", f"unexpected RGB frame shape {rgb.shape}")
    if rgb.shape[2] == 4:
        alpha = rgb[:, :, 3:4].astype(np.float32) / 255.0
        color = rgb[:, :, :3].astype(np.float32)
        rgb = np.clip(color * alpha + 255.0 * (1.0 - alpha), 0, 255).astype(np.uint8)
    return np.ascontiguousarray(rgb[:, :, :3])


def decode_image_bytes(data: bytes, *, source_path: Path | None = None) -> NormalizedImage:
    del source_path
    try:
        with Image.open(io.BytesIO(data)) as image:
            image.load()
            decoded_format = (image.format or "").lower()
            source_size = image.size
            rgb = normalize_image(image)
    except UnidentifiedImageError as exc:
        raise MediaDecodeError("corrupt_media", "Pillow cannot identify image data") from exc
    except (OSError, ValueError, SyntaxError) as exc:
        raise MediaDecodeError("corrupt_media", f"image decode failed: {exc}") from exc
    return NormalizedImage(
        rgb=rgb,
        source_width=int(source_size[0]),
        source_height=int(source_size[1]),
        decoded_format=decoded_format,
    )


def decode_image_file(path: Path, data: bytes | None = None) -> NormalizedImage:
    if data is None:
        try:
            data = path.read_bytes()
        except OSError as exc:
            raise MediaDecodeError("io_error", str(exc), retryable=True) from exc
    return decode_image_bytes(data, source_path=path)


class ImageLoader:
    name = "image-pillow"
    formats = tuple(sorted(IMAGE_FORMATS))

    def register_into(self, registry: object) -> None:
        registry.register(  # type: ignore[attr-defined]
            MediaFamily.IMAGE,
            self.formats,
            self.name,
            decode_image_file,
            metadata={"loader": self.name},
        )


IMAGE_LOADER = ImageLoader()
load_image = decode_image_file
