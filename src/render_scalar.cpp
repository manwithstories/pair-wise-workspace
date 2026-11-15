#include "render.h"

#include <cstddef>

namespace lutgrade {

FloatImage render_scalar(const CubeLut& lut, const FloatImage& input, const RenderOptions& options) {
  if (input.channels != 3 ||
      input.pixels.size() != static_cast<std::size_t>(input.width) * input.height * 3) {
    throw std::invalid_argument("scalar renderer requires an RGB image");
  }

  FloatImage output;
  output.width = input.width;
  output.height = input.height;
  output.channels = 3;
  output.pixels.resize(input.pixels.size());
  for (std::size_t pixel = 0; pixel < static_cast<std::size_t>(input.width) * input.height; ++pixel) {
    const Vec3 rgb{input.pixels[pixel * 3], input.pixels[pixel * 3 + 1],
                   input.pixels[pixel * 3 + 2]};
    const Vec3 result = grade_pixel(lut, rgb, options);
    output.pixels[pixel * 3] = static_cast<float>(result.r);
    output.pixels[pixel * 3 + 1] = static_cast<float>(result.g);
    output.pixels[pixel * 3 + 2] = static_cast<float>(result.b);
  }
  return output;
}

}  // namespace lutgrade

