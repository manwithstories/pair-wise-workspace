#include "frame_pattern.hpp"

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <vector>

namespace mfs {

// Deterministic motion pattern: a moving gradient plus a bright border on
// keyframes. Cheap to encode yet visually distinct between frames.
std::vector<uint8_t> render_pattern(uint32_t width, uint32_t height,
                                    uint32_t index, bool keyframe) {
  std::vector<uint8_t> rgb(static_cast<size_t>(width) * height * 3);
  const uint32_t phase = index * 3;
  for (uint32_t y = 0; y < height; ++y) {
    const uint8_t row =
        static_cast<uint8_t>((y * 255) / std::max<uint32_t>(height, 1));
    for (uint32_t x = 0; x < width; ++x) {
      const size_t at = (static_cast<size_t>(y) * width + x) * 3;
      uint8_t r = static_cast<uint8_t>((x + phase) & 0xFF);
      uint8_t g = static_cast<uint8_t>(row ^ (phase >> 1));
      uint8_t b = static_cast<uint8_t>((x ^ y) + index);
      if (keyframe && (x < 4 || y < 4 || x + 4 >= width || y + 4 >= height)) {
        r = g = b = 255;
      }
      rgb[at + 0] = r;
      rgb[at + 1] = g;
      rgb[at + 2] = b;
    }
  }
  return rgb;
}

}  // namespace mfs
