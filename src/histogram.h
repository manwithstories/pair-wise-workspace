#pragma once

#include <array>
#include <cstdint>
#include <string>

#include "image_io.h"

namespace lutgrade {

struct Histogram {
  std::array<std::uint64_t, 256> red{};
  std::array<std::uint64_t, 256> green{};
  std::array<std::uint64_t, 256> blue{};

  std::uint64_t nonzero_count() const;
};

struct ChannelDeviation {
  double red = 0.0;
  double green = 0.0;
  double blue = 0.0;

  double maximum() const;
};

struct GradeReport {
  int grid_3d = 0;
  int shaper_size = 0;
  std::uint32_t width = 0;
  std::uint32_t height = 0;
  unsigned int threads = 1;
  std::string source;
  std::string destination;
  std::string interpolation;
  Histogram before;
  Histogram after;
  ChannelDeviation deviation;
  double threshold = 0.0;
  bool verification = false;
};

Histogram compute_histogram(const FloatImage& image);
ChannelDeviation max_abs_deviation(const FloatImage& first, const FloatImage& second);
std::string histogram_json(const Histogram& histogram);
std::string report_json(const GradeReport& report, const std::string& lut_path,
                        const std::string& input_path, const std::string& output_path);

}  // namespace lutgrade

