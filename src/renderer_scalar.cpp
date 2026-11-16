#include "renderer_scalar.hpp"

#include <cstddef>

namespace lutgrade {

RenderOutput render_scalar(const Image& input, const GradingKernel& kernel) {
  RenderOutput out;
  out.width = input.width;
  out.height = input.height;
  const std::size_t pixels = input.pixel_count();
  out.before.resize(pixels * 3);
  out.after.resize(pixels * 3);

  for (int y = 0; y < input.height; ++y) {
    for (int x = 0; x < input.width; ++x) {
      const std::size_t p = static_cast<std::size_t>(y) * input.width + x;
      const std::uint8_t* src = input.rgb.data() + p * 3;
      const PixelResult result = kernel.process(src[0], src[1], src[2], x, y);
      // Deterministic placement: pixel p always writes slots p*3 .. p*3+2.
      out.before[p * 3 + 0] = result.pre_lut[0];
      out.before[p * 3 + 1] = result.pre_lut[1];
      out.before[p * 3 + 2] = result.pre_lut[2];
      out.after[p * 3 + 0] = result.rgb[0];
      out.after[p * 3 + 1] = result.rgb[1];
      out.after[p * 3 + 2] = result.rgb[2];
      if (result.clipped) ++out.clipped_pixels;
    }
  }
  return out;
}

}  // namespace lutgrade
