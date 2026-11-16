#pragma once
#include <array>
#include <cstdint>
#include <string>
#include <vector>

#include "interpolator.hpp"

// Post-render statistics.
//
// Everything here is computed from the RenderOutput float buffers, so it is
// independent of which render path produced them. For a deterministic result
// the hash is taken over the quantized 8-bit output, not the floats.
namespace lutgrade {

inline constexpr std::size_t kHistogramBins = 256;

struct ChannelDeviation {
  double max_abs = 0.0;   // largest |after - before| over this channel
  double mean_abs = 0.0;  // mean |after - before| over this channel
  std::size_t max_x = 0;  // where max_abs occurred (first hit, row-major)
  std::size_t max_y = 0;
};

struct Statistics {
  std::array<std::array<std::uint64_t, kHistogramBins>, 3> hist_before{};
  std::array<std::array<std::uint64_t, kHistogramBins>, 3> hist_after{};
  std::array<ChannelDeviation, 3> deviation{};
  double mean_deviation = 0.0;      // mean |after - before| over all samples
  std::uint64_t saturated_pixels = 0;  // pixels clamped in either path
  std::uint64_t hash = 0;           // FNV-1a over quantized RGB
  std::size_t pixel_count = 0;
};

Statistics compute_statistics(const RenderOutput& rendered);

// Formats the 64-bit hash as 16 lowercase hex digits.
std::string format_hash(std::uint64_t hash);

}  // namespace lutgrade
