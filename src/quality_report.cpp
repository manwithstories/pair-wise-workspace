#include "media/quality_report.hpp"
#include <cmath>
#include <fstream>

namespace media {
void PsnrAccumulator::addFrame(double mse) {
  double psnr = mse <= 1e-12 ? 60.0 : 10.0 * std::log10(255.0 * 255.0 / mse);
  sumPsnr_ += psnr;
  ++frames_;
}
double PsnrAccumulator::psnrDb() const {
  return frames_ ? sumPsnr_ / double(frames_) : 0.0;
}
double computeBitrateErrorPct(int64_t target, int64_t actual) {
  if (target <= 0) return 0.0;
  return std::fabs(double(actual - target) / double(target)) * 100.0;
}
TimeError DiagnosticLog::open() { opened_ = true; return TimeError::None; }
TimeError DiagnosticLog::append(const TimeEvent&) { return TimeError::None; }
TimeError DiagnosticLog::close() { opened_ = false; return TimeError::None; }
std::string renderReportJson(const QualityStats&) { return "{}\n"; }
} // namespace media
