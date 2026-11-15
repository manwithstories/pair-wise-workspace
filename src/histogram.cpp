#include "histogram.h"

#include <algorithm>
#include <cmath>
#include <cstddef>
#include <iomanip>
#include <sstream>
#include <stdexcept>

namespace lutgrade {
namespace {

unsigned int histogram_bucket(float value) {
  if (!std::isfinite(value)) {
    return 0;
  }
  const double clamped = std::clamp(static_cast<double>(value), 0.0, 1.0);
  return std::min(255U, static_cast<unsigned int>(clamped * 256.0));
}

}  // namespace

std::uint64_t Histogram::nonzero_count() const {
  std::uint64_t count = 0;
  for (std::size_t i = 0; i < 256; ++i) {
    if (red[i] != 0 || green[i] != 0 || blue[i] != 0) {
      ++count;
    }
  }
  return count;
}

double ChannelDeviation::maximum() const {
  return std::max(red, std::max(green, blue));
}

Histogram compute_histogram(const FloatImage& image) {
  if (image.width == 0 || image.height == 0 || (image.channels != 1 && image.channels != 3) ||
      image.pixels.size() != static_cast<std::size_t>(image.width) * image.height * image.channels) {
    throw std::invalid_argument("cannot histogram an image with invalid dimensions");
  }

  Histogram histogram;
  const std::size_t pixel_count = static_cast<std::size_t>(image.width) * image.height;
  for (std::size_t pixel = 0; pixel < pixel_count; ++pixel) {
    if (image.channels == 1) {
      const unsigned int bucket = histogram_bucket(image.pixels[pixel]);
      ++histogram.red[bucket];
      ++histogram.green[bucket];
      ++histogram.blue[bucket];
    } else {
      ++histogram.red[histogram_bucket(image.pixels[pixel * 3])];
      ++histogram.green[histogram_bucket(image.pixels[pixel * 3 + 1])];
      ++histogram.blue[histogram_bucket(image.pixels[pixel * 3 + 2])];
    }
  }
  return histogram;
}

ChannelDeviation max_abs_deviation(const FloatImage& first, const FloatImage& second) {
  if (first.width != second.width || first.height != second.height || first.channels != 3 ||
      second.channels != 3) {
    throw std::invalid_argument("cannot compare images with different dimensions");
  }
  if (first.pixels.size() != second.pixels.size()) {
    throw std::invalid_argument("cannot compare mismatched pixel buffers");
  }

  ChannelDeviation deviation;
  for (std::size_t pixel = 0; pixel < static_cast<std::size_t>(first.width) * first.height; ++pixel) {
    deviation.red = std::max(deviation.red,
                             std::abs(static_cast<double>(first.pixels[pixel * 3]) -
                                      static_cast<double>(second.pixels[pixel * 3])));
    deviation.green = std::max(deviation.green,
                               std::abs(static_cast<double>(first.pixels[pixel * 3 + 1]) -
                                        static_cast<double>(second.pixels[pixel * 3 + 1])));
    deviation.blue = std::max(deviation.blue,
                              std::abs(static_cast<double>(first.pixels[pixel * 3 + 2]) -
                                       static_cast<double>(second.pixels[pixel * 3 + 2])));
  }
  return deviation;
}

std::string histogram_json(const Histogram& histogram) {
  const auto append_channel = [](std::ostringstream& stream, const char* name,
                                 const std::array<std::uint64_t, 256>& buckets) {
    stream << "    \"" << name << "\": [";
    for (std::size_t i = 0; i < buckets.size(); ++i) {
      if (i != 0) {
        stream << ", ";
      }
      stream << buckets[i];
    }
    stream << "]\n";
  };

  std::ostringstream stream;
  stream << "{\n";
  append_channel(stream, "r", histogram.red);
  stream << ",\n";
  append_channel(stream, "g", histogram.green);
  stream << ",\n";
  append_channel(stream, "b", histogram.blue);
  stream << "}";
  return stream.str();
}

namespace {

std::string json_escape(const std::string& value) {
  std::ostringstream stream;
  for (const char ch : value) {
    switch (ch) {
      case '"': stream << "\\\""; break;
      case '\\': stream << "\\\\"; break;
      case '\n': stream << "\\n"; break;
      case '\r': stream << "\\r"; break;
      case '\t': stream << "\\t"; break;
      default: stream << ch; break;
    }
  }
  return stream.str();
}

}  // namespace

std::string report_json(const GradeReport& report, const std::string& lut_path,
                        const std::string& input_path, const std::string& output_path) {
  std::ostringstream stream;
  stream << std::setprecision(17);
  stream << "{\n";
  stream << "  \"lut_grid_3d\": \"" << report.grid_3d << "x" << report.grid_3d << "x"
         << report.grid_3d << "\",\n";
  stream << "  \"lut_grid_3d_size\": " << report.grid_3d << ",\n";
  stream << "  \"shaper_size\": " << report.shaper_size << ",\n";
  stream << "  \"interpolation\": \"" << json_escape(report.interpolation) << "\",\n";
  stream << "  \"threads\": " << report.threads << ",\n";
  stream << "  \"image\": {\"width\": " << report.width << ", \"height\": " << report.height
         << "},\n";
  stream << "  \"source\": \"" << json_escape(report.source) << "\",\n";
  stream << "  \"destination\": \"" << json_escape(report.destination) << "\",\n";
  stream << "  \"max_dev_r\": " << report.deviation.red << ",\n";
  stream << "  \"max_dev_g\": " << report.deviation.green << ",\n";
  stream << "  \"max_dev_b\": " << report.deviation.blue << ",\n";
  stream << "  \"scalar_vs_tiled_max_dev\": " << report.deviation.maximum() << ",\n";
  stream << "  \"hist_bins\": 256,\n";
  stream << "  \"hist_before_nonzero\": " << report.before.nonzero_count() << ",\n";
  stream << "  \"hist_after_nonzero\": " << report.after.nonzero_count() << ",\n";
  stream << "  \"verification_threshold\": " << report.threshold << ",\n";
  stream << "  \"verification_passed\": " << (report.verification ? "true" : "false") << ",\n";
  stream << "  \"input\": \"" << json_escape(input_path) << "\",\n";
  stream << "  \"lut\": \"" << json_escape(lut_path) << "\",\n";
  stream << "  \"output\": \"" << json_escape(output_path) << "\",\n";
  stream << "  \"hist_before\": " << histogram_json(report.before) << ",\n";
  stream << "  \"hist_after\": " << histogram_json(report.after) << "\n";
  stream << "}\n";
  return stream.str();
}

}  // namespace lutgrade

