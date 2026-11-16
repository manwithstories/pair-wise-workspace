#include "statistics.hpp"

#include <cmath>
#include <cstdio>

namespace lutgrade {
namespace {

// Bin for a 0..1 float. Uses the same rounding as quantize() so the histogram
// and the written PPM agree on which bucket a pixel belongs to.
std::size_t bin_of(float value) {
  double scaled = static_cast<double>(value) * 255.0;
  if (!std::isfinite(scaled)) scaled = 0.0;
  scaled = std::floor(scaled + 0.5);
  if (scaled < 0.0) scaled = 0.0;
  if (scaled > 255.0) scaled = 255.0;
  return static_cast<std::size_t>(scaled);
}

constexpr std::uint64_t kFnvOffset = 1469598103934665603ULL;
constexpr std::uint64_t kFnvPrime = 1099511628211ULL;

}  // namespace

Statistics compute_statistics(const RenderOutput& rendered) {
  Statistics stats;
  stats.pixel_count = rendered.pixel_count();
  const std::size_t pixels = stats.pixel_count;

  std::array<double, 3> sum_abs{};
  std::uint64_t hash = kFnvOffset;

  for (std::size_t p = 0; p < pixels; ++p) {
    for (std::size_t c = 0; c < 3; ++c) {
      const float before = rendered.before[p * 3 + c];
      const float after = rendered.after[p * 3 + c];
      stats.hist_before[c][bin_of(before)] += 1;
      stats.hist_after[c][bin_of(after)] += 1;

      const double diff = std::fabs(static_cast<double>(after) -
                                    static_cast<double>(before));
      if (diff > stats.deviation[c].max_abs) {
        stats.deviation[c].max_abs = diff;
        stats.deviation[c].max_x = p % static_cast<std::size_t>(rendered.width);
        stats.deviation[c].max_y = p / static_cast<std::size_t>(rendered.width);
      }
      sum_abs[c] += diff;
    }

    // Hash the quantized output bytes so it is stable across platforms.
    for (std::size_t c = 0; c < 3; ++c) {
      const std::uint8_t byte =
          static_cast<std::uint8_t>(bin_of(rendered.after[p * 3 + c]));
      hash ^= byte;
      hash *= kFnvPrime;
    }
  }

  double total = 0.0;
  for (std::size_t c = 0; c < 3; ++c) {
    stats.deviation[c].mean_abs =
        pixels == 0 ? 0.0 : sum_abs[c] / static_cast<double>(pixels);
    total += sum_abs[c];
  }
  stats.mean_deviation =
      pixels == 0 ? 0.0 : total / static_cast<double>(pixels * 3);
  stats.saturated_pixels = rendered.clipped_pixels;
  stats.hash = hash;
  return stats;
}

std::string format_hash(std::uint64_t hash) {
  char buf[17];
  std::snprintf(buf, sizeof(buf), "%016llx",
                static_cast<unsigned long long>(hash));
  return std::string(buf);
}

}  // namespace lutgrade
