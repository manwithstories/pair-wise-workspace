"""Headless first-frame rendering.

Everything is drawn into in-memory RGBA buffers with Pillow, then combined
with NumPy alpha compositing. No window or display is ever opened.
"""
from __future__ import annotations

import io
from dataclasses import dataclass

import numpy as np
from PIL import Image, ImageDraw, ImageFont

from .tl_parse import Cue

WIDTH = 320
HEIGHT = 180
BG_COLOR = (18, 26, 42, 255)
BOX_FILL = (0, 0, 0, 140)
TEXT_FILL = (255, 255, 255, 255)
MARGIN = 14
BOX_PAD = 8


@dataclass(frozen=True, slots=True)
class RenderedFrame:
    cue_id: str
    png_bytes: bytes
    rgb: np.ndarray


def _font() -> ImageFont.FreeTypeFont | ImageFont.ImageFont:
    try:
        return ImageFont.truetype("DejaVuSans.ttf", 16)
    except OSError:
        return ImageFont.load_default()


def _draw_cue_layer(cue: Cue) -> Image.Image:
    layer = Image.new("RGBA", (WIDTH, HEIGHT), (0, 0, 0, 0))
    draw = ImageDraw.Draw(layer)
    font = _font()
    text = f"{cue.id}  {cue.text}"
    bbox = draw.textbbox((0, 0), text, font=font)
    text_w = bbox[2] - bbox[0]
    text_h = bbox[3] - bbox[1]
    box_w = min(WIDTH - 2 * MARGIN, text_w + 2 * BOX_PAD)
    box_h = text_h + 2 * BOX_PAD
    x0 = (WIDTH - box_w) // 2
    y0 = HEIGHT - MARGIN - box_h
    draw.rounded_rectangle(
        (x0, y0, x0 + box_w, y0 + box_h), radius=6, fill=BOX_FILL
    )
    text_x = x0 + (box_w - text_w) // 2 - bbox[0]
    text_y = y0 + BOX_PAD - bbox[1]
    draw.text((text_x, text_y), text, font=font, fill=TEXT_FILL)
    return layer


def render_first_frame(cue: Cue) -> RenderedFrame:
    background = Image.new("RGBA", (WIDTH, HEIGHT), BG_COLOR)
    overlay = _draw_cue_layer(cue)

    base = np.asarray(background, dtype=np.float64)
    layer = np.asarray(overlay, dtype=np.float64)
    alpha = layer[..., 3:4] / 255.0
    composed = layer[..., :3] * alpha + base[..., :3] * (1.0 - alpha)
    out = np.empty((HEIGHT, WIDTH, 4), dtype=np.uint8)
    out[..., :3] = np.rint(composed).astype(np.uint8)
    out[..., 3] = 255

    image = Image.fromarray(out, mode="RGBA").convert("RGB")
    buffer = io.BytesIO()
    image.save(buffer, format="PNG", compress_level=9)
    png_bytes = buffer.getvalue()
    return RenderedFrame(cue_id=cue.id, png_bytes=png_bytes, rgb=np.asarray(image))
