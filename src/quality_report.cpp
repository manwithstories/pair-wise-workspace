#include "media/quality_report.hpp"
#include <cmath>
#include <fstream>
#include <iomanip>
#include <sstream>

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

namespace {
std::string jsonEscape(const std::string& s) {
  std::string o;
  for (char c : s) {
    switch (c) {
    case '"': o += "\\\""; break;
    case '\\': o += "\\\\"; break;
    case '\n': o += "\\n"; break;
    default: o += c;
    }
  }
  return o;
}
} // namespace

TimeError DiagnosticLog::open() {
  auto f = std::make_unique<std::ofstream>(path_, std::ios::trunc);
  if (!*f) return TimeError::IoError;
  file_ = std::move(f);
  opened_ = true;
  return TimeError::None;
}

TimeError DiagnosticLog::append(const TimeEvent& ev) {
  if (!opened_ || !file_) return TimeError::IoError;
  *file_ << "{\"packet_index\":" << ev.packetIndex
         << ",\"raw_pts\":" << ev.rawPts
         << ",\"repaired_us\":" << ev.repairedUs
         << ",\"error\":\"" << toString(ev.error) << "\""
         << ",\"detail\":\"" << jsonEscape(ev.detail) << "\"}\n";
  file_->flush();
  return file_->good() ? TimeError::None : TimeError::IoError;
}

TimeError DiagnosticLog::close() {
  if (!opened_) return TimeError::None;
  file_->flush();
  bool ok = file_->good();
  file_->close();
  opened_ = false;
  return ok ? TimeError::None : TimeError::IoError;
}

std::string renderReportJson(const QualityStats& s) {
  std::ostringstream o;
  o << std::fixed << std::setprecision(3);
  o << "{\n"
    << "  \"packets_in\": " << s.packetsIn << ",\n"
    << "  \"packets_repaired\": " << s.packetsRepaired << ",\n"
    << "  \"wraps\": " << s.wraps << ",\n"
    << "  \"drift_ppm\": " << s.driftPpm << ",\n"
    << "  \"drift_low_confidence\": " << (s.lowConfidence ? "true" : "false")
    << ",\n"
    << "  \"queue_peak\": " << s.queuePeak << ",\n"
    << "  \"frames_decoded\": " << s.framesDecoded << ",\n"
    << "  \"frames_corrupt\": " << s.framesCorrupt << ",\n"
    << "  \"target_bitrate_bps\": " << s.targetBitrateBps << ",\n"
    << "  \"actual_bitrate_bps\": " << s.actualBitrateBps << ",\n"
    << "  \"bitrate_error_pct\": " << s.bitrateErrorPct << ",\n"
    << "  \"psnr_db\": " << s.psnrDb << ",\n"
    << "  \"manifest_alignment_ms\": " << s.alignmentErrorMs << ",\n"
    << "  \"seek_error_ms\": " << s.seekErrorMs << ",\n"
    << "  \"duration_ms\": " << (s.durationUs / 1000) << "\n"
    << "}\n";
  return o.str();
}
} // namespace media
