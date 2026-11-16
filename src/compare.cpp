#include "compare.hpp"

#include <algorithm>
#include <cmath>

#include "statistics.hpp"

namespace lutgrade {

CompareReport compare_rendered(const RenderOutput& left, const RenderOutput& right,
                               int width, double threshold) {
  CompareReport report;
  report.pixels = left.pixel_count();
  report.threshold = threshold;
  report.hash_scalar = compute_statistics(left).hash;
  report.hash_tiled = compute_statistics(right).hash;

  const std::size_t samples = std::min(left.after.size(), right.after.size());
  const std::size_t columns = width > 0 ? static_cast<std::size_t>(width) : 1;

  bool saw_diff = false;
  double max_diff = 0.0;
  for (std::size_t i = 0; i < samples; ++i) {
    const double diff = std::fabs(static_cast<double>(left.after[i]) -
                                  static_cast<double>(right.after[i]));
    if (!(diff <= max_diff)) max_diff = diff;  // NaN-safe: keeps max finite
    if (!saw_diff && diff > threshold) {
      saw_diff = true;
      const std::size_t pixel = i / 3;
      report.has_first_diff = true;
      report.first_x = pixel % columns;
      report.first_y = pixel / columns;
      report.first_channel = static_cast<int>(i % 3);
      report.left = left.after[i];
      report.right = right.after[i];
    }
  }
  report.max_diff = max_diff;
  report.match = !saw_diff;
  return report;
}

}  // namespace lutgrade
