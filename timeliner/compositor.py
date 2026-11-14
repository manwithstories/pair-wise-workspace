"""Layer-stack compositor with a 16-bit accumulation buffer.

Each frame is rendered into a uint16 RGBA canvas. Layers outside their
time interval are skipped; each active layer is warped once per unique
keyframe state (cached) and blended only inside its dirty bounding box.
"""

import os

import numpy as np
from PIL import Image, UnidentifiedImageError

from .blend import composite_region
from .errors import AssetError
from .model import sample_keyframes
from .transform import affine_matrix, dest_bbox, warp

_QUANT = 1e-6


def _load_image(assets_dir, layer):
    path = os.path.join(assets_dir, layer.asset)
    if not os.path.isfile(path):
        raise AssetError("asset not found: %s" % path, "layers[%s].asset" % layer.id)
    try:
        with Image.open(path) as img:
            rgba = img.convert("RGBA")
            arr = np.array(rgba, dtype=np.uint8)
    except (UnidentifiedImageError, OSError, ValueError) as exc:
        raise AssetError("asset is corrupt or unreadable: %s (%s)"
                         % (path, exc), "layers[%s].asset" % layer.id)
    h, w = arr.shape[0], arr.shape[1]
    if (layer.width and layer.width != w) or (layer.height and layer.height != h):
        raise AssetError(
            "asset size %dx%d does not match declared %dx%d"
            % (w, h, layer.width, layer.height),
            "layers[%s].asset" % layer.id)
    return arr


class Compositor:
    """Renders RGB24 frames from a validated timeline. Reused across frames."""

    def __init__(self, timeline, assets_dir):
        self.timeline = timeline
        self.assets_dir = assets_dir
        self.width = timeline.width
        self.height = timeline.height
        self.images = {layer.id: _load_image(assets_dir, layer)
                       for layer in timeline.layers}
        self.blend_calls = 0
        self._warp_cache = {}
        self._canvas = np.zeros((self.height, self.width, 4), dtype=np.uint16)

    def _layer_params(self, layer, t):
        params = sample_keyframes(layer.keyframes, t)
        key = tuple(round(params[name] / _QUANT)
                    for name in ("x", "y", "scale_x", "scale_y", "rotate"))
        return params, key

    def _warp_layer(self, layer, params):
        img = self.images[layer.id]
        src_h, src_w = img.shape[0], img.shape[1]
        matrix = affine_matrix(
            params["x"], params["y"], params["scale_x"], params["scale_y"],
            params["rotate"], layer.anchor, src_w, src_h)
        bbox = dest_bbox(matrix, src_w, src_h, self.width, self.height)
        if bbox is None:
            return None, None
        if (bbox == (0, 0, self.width, self.height)
                and np.abs(matrix - np.eye(3)).max() < 1e-9):
            return bbox, img.astype(np.uint16) * 257
        warped = warp(img, matrix, bbox)
        return bbox, warped.astype(np.uint16) * 257

    def render_frame(self, frame_index, fps):
        """Render one frame and return it as RGB24 bytes (row-major)."""
        t = frame_index / float(fps)
        canvas = self._canvas
        canvas.fill(0)
        for layer in self.timeline.layers:
            if not layer.active(t):
                continue
            params, key = self._layer_params(layer, t)
            cached = self._warp_cache.get(layer.id)
            if cached is not None and cached[0] == key:
                bbox, region = cached[1], cached[2]
            else:
                bbox, region = self._warp_layer(layer, params)
                self._warp_cache[layer.id] = (key, bbox, region)
            if bbox is None:
                continue
            opacity = layer.opacity * params["opacity"]
            if opacity <= 0.0:
                continue
            x0, y0, x1, y1 = bbox
            composite_region(canvas[y0:y1, x0:x1], region, layer.blend, opacity)
            self.blend_calls += 1
        rgb = canvas[..., :3].astype(np.float32)
        rgb *= np.float32(1.0 / 65535.0)
        alpha = canvas[..., 3:4].astype(np.float32)
        alpha *= np.float32(1.0 / 65535.0)
        out = np.rint(rgb * alpha * 255.0).astype(np.uint8)
        return out.tobytes()
