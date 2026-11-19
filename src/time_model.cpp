#include "media/time_model.hpp"
#include <cmath>

namespace media {

const char* toString(TimeError e) {
  switch (e) {
  case TimeError::None: return "none";
  case TimeError::Wrap: return "wrap";
  case TimeError::BackwardIllegal: return "backward_illegal";
  case TimeError::DuplicatePts: return "duplicate_pts";
  case TimeError::Ambiguous: return "ambiguous";
  case TimeError::CorruptPacket: return "corrupt_packet";
  case TimeError::TruncatedInput: return "truncated_input";
  case TimeError::ZeroDuration: return "zero_duration";
  case TimeError::MissingAnchor: return "missing_anchor";
  case TimeError::QueueCancelled: return "queue_cancelled";
  case TimeError::IoError: return "io_error";
  }
  return "unknown";
}

LineFit fitLine(const std::vector<std::pair<double, double>>& pts) {
  LineFit out;
  out.points = pts.size();
  const size_t n = pts.size();
  if (n < 2) return out;
  double sx = 0, sy = 0, sxx = 0, sxy = 0, syy = 0;
  for (const auto& [x, y] : pts) {
    sx += x; sy += y; sxx += x * x; sxy += x * y; syy += y * y;
  }
  const double den = double(n) * sxx - sx * sx;
  if (std::fpclassify(den) == FP_ZERO) return out;
  out.a = (double(n) * sxy - sx * sy) / den;
  out.b = (sy - out.a * sx) / double(n);
  // 决定系数 R^2（用于置信度）。
  const double mean = sy / double(n);
  double ssTot = 0, ssRes = 0;
  for (const auto& [x, y] : pts) {
    const double f = out.a * x + out.b;
    ssTot += (y - mean) * (y - mean);
    ssRes += (y - f) * (y - f);
  }
  out.r2 = ssTot > 0 ? std::max(0.0, 1.0 - ssRes / ssTot) : 1.0;
  out.valid = true;
  return out;
}

double clampSlopePpm(double ppm) {
  if (ppm > MAX_SLOPE_PPM) return MAX_SLOPE_PPM;
  if (ppm < -MAX_SLOPE_PPM) return -MAX_SLOPE_PPM;
  return ppm;
}

} // namespace media
