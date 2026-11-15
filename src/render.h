#pragma once

#include <stdexcept>
#include <vector>

#include "color_matrix.h"
#include "cube_parser.h"
#include "image_io.h"

namespace lutgrade {

struct RenderOptions {
  ColorSpaceId source = ColorSpaceId::AcesCg;
  ColorSpaceId destination = ColorSpaceId::Rec709;
  Interpolation interpolation = Interpolation::Tetrahedral;
  bool transform_color = true;
};

inline Vec3 grade_pixel(const CubeLut& lut, const Vec3& input, const RenderOptions& options) {
  Vec3 target = input;
  if (options.transform_color) {
    target = convert_linear_rgb(options.source, options.destination,
                                decode_rgb(options.source, input));
  }
  Vec3 graded = apply_lut(lut, target, options.interpolation);
  if (options.transform_color) {
    graded = encode_rgb(options.destination, clamp01(graded));
  }
  return clamp01(graded);
}

FloatImage render_scalar(const CubeLut& lut, const FloatImage& input, const RenderOptions& options);
FloatImage render_tiled(const CubeLut& lut, const FloatImage& input, const RenderOptions& options,
                        unsigned int threads);

}  // namespace lutgrade

