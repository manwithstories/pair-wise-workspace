#include "report/Histogram.h"
#include "Errors.h"
#include <cmath>
#include <limits>

namespace lg {

FrameStats computeStats(const Frame& f) {
  FrameStats s;
  double sum[3] = {0.0, 0.0, 0.0};
  std::size_t count = static_cast<std::size_t>(f.w) * f.h;
  for (std::size_t i = 0; i < count; ++i)
    for (int c = 0; c < 3; ++c) {
      float v = f.px[i * 3 + c];
      if (std::isnan(v))
        throw InternalError("NaN pixel encountered during statistics");
      float cl = v < 0 ? 0.0f : (v > 1 ? 1.0f : v);
      int bucket = static_cast<int>(cl * 255.0f + 0.5f);
      ++s.ch[c].buckets[static_cast<std::size_t>(bucket)];
      sum[c] += v;
    }
  for (int c = 0; c < 3; ++c)
    s.ch[c].mean = count ? sum[c] / count : 0.0;
  return s;
}

CompareResult compareFrames(const Frame& a, const Frame& b) {
  if (a.w != b.w || a.h != b.h)
    throw InternalError("frame size mismatch in comparison");
  CompareResult r;
  double sumSq = 0.0;
  std::size_t count = static_cast<std::size_t>(a.w) * a.h;
  for (std::size_t i = 0; i < count; ++i)
    for (int c = 0; c < 3; ++c) {
      double d = static_cast<double>(a.px[i * 3 + c]) - b.px[i * 3 + c];
      double ad = std::fabs(d);
      if (ad > r.maxDev[c])
        r.maxDev[c] = ad;
      sumSq += d * d;
    }
  double mse = count ? sumSq / (count * 3) : 0.0;
  r.rmse = std::sqrt(mse);
  r.psnrDb = mse > 0.0 ? 10.0 * std::log10(1.0 / mse)
                       : std::numeric_limits<double>::infinity();
  return r;
}

} // namespace lg
