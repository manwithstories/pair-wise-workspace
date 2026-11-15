#include "render.h"

#include <algorithm>
#include <exception>
#include <thread>

namespace lutgrade {

FloatImage render_tiled(const CubeLut& lut, const FloatImage& input, const RenderOptions& options,
                        unsigned int threads) {
  if (threads == 0) {
    throw std::invalid_argument("thread count must be positive");
  }
  if (input.channels != 3 ||
      input.pixels.size() != static_cast<std::size_t>(input.width) * input.height * 3) {
    throw std::invalid_argument("tiled renderer requires an RGB image");
  }

  FloatImage output;
  output.width = input.width;
  output.height = input.height;
  output.channels = 3;
  output.pixels.resize(input.pixels.size());

  const unsigned int band_count = std::min<unsigned int>(threads, input.height);
  const unsigned int rows_per_band = (input.height + band_count - 1) / band_count;
  std::vector<std::exception_ptr> failures(band_count);
  std::vector<std::thread> workers;
  workers.reserve(band_count);

  try {
    for (unsigned int band = 0; band < band_count; ++band) {
      const std::uint32_t first_row = band * rows_per_band;
      const std::uint32_t last_row = std::min(input.height, first_row + rows_per_band);
      if (first_row >= last_row) {
        continue;
      }
      workers.emplace_back([&, band, first_row, last_row] {
        try {
          for (std::uint32_t y = first_row; y < last_row; ++y) {
            for (std::uint32_t x = 0; x < input.width; ++x) {
              const std::size_t pixel = static_cast<std::size_t>(y) * input.width + x;
              const Vec3 rgb{input.pixels[pixel * 3], input.pixels[pixel * 3 + 1],
                             input.pixels[pixel * 3 + 2]};
              const Vec3 result = grade_pixel(lut, rgb, options);
              output.pixels[pixel * 3] = static_cast<float>(result.r);
              output.pixels[pixel * 3 + 1] = static_cast<float>(result.g);
              output.pixels[pixel * 3 + 2] = static_cast<float>(result.b);
            }
          }
        } catch (...) {
          failures[band] = std::current_exception();
        }
      });
    }
  } catch (...) {
    for (std::thread& worker : workers) {
      if (worker.joinable()) {
        worker.join();
      }
    }
    throw;
  }

  for (std::thread& worker : workers) {
    worker.join();
  }
  for (const std::exception_ptr& failure : failures) {
    if (failure) {
      std::rethrow_exception(failure);
    }
  }
  return output;
}

}  // namespace lutgrade

