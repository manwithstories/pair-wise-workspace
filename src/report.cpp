#include <cstdio>
#include <string>

#include "media_archive/report.h"

namespace media_archive {

ReportFormat report_format_from_name(const std::string& name) {
  return name == "text" ? ReportFormat::Text : ReportFormat::Json;
}

namespace {
// Fixed-precision formatting keeps output deterministic across runs.
std::string fixed(double v, int digits = 4) {
  char buf[64];
  std::snprintf(buf, sizeof(buf), "%.*f", digits, v);
  return std::string(buf);
}
}  // namespace

std::string render_report_json(const RecoveryReport& r) {
  std::string s;
  s.reserve(512);
  s += "{\n";
  auto field_d = [&](const char* k, double v) {
    s += "  \"";
    s += k;
    s += "\": ";
    s += fixed(v);
    s += ",\n";
  };
  auto field_u = [&](const char* k, uint64_t v) {
    s += "  \"";
    s += k;
    s += "\": ";
    s += std::to_string(v);
    s += ",\n";
  };
  auto field_b = [&](const char* k, bool v) {
    s += "  \"";
    s += k;
    s += "\": ";
    s += v ? "true" : "false";
    s += ",\n";
  };

  s += "  \"codec\": \"";
  s += codec_name(r.codec);
  s += "\",\n";
  field_u("recovered_packets", r.recovered_packets);
  field_u("unrecovered_bytes", r.unrecovered_bytes);
  field_d("loss_recovery_rate", r.loss_recovery_rate);
  field_d("p95_latency_ms", r.p95_latency_ms);
  field_u("p95_sample_count", r.p95_sample_count);
  field_d("av_sync_error_ms", r.av_sync_error_ms);
  field_u("audio_sample_delta",
          static_cast<uint64_t>(r.audio_sample_delta < 0
                                    ? -r.audio_sample_delta
                                    : r.audio_sample_delta));
  field_d("index_seek_error_ms", r.index_seek_error_ms);
  field_b("checkpoint_resumed", r.checkpoint_resumed);
  field_u("keyframe_count", r.keyframe_count);
  field_u("gap_count", r.gap_count);
  field_u("uncertain_pts_count", r.uncertain_pts_count);
  field_b("valid", r.valid);
  // Last two fields: rollback block / failure offset use null when absent.
  s += "  \"rolled_back_block\": ";
  if (r.rolled_back_block == UINT64_MAX)
    s += "null";
  else
    s += std::to_string(r.rolled_back_block);
  s += ",\n";
  s += "  \"failure_offset\": ";
  if (r.failure_offset < 0)
    s += "null";
  else
    s += std::to_string(r.failure_offset);
  s += "\n}\n";
  return s;
}

std::string render_report_text(const RecoveryReport& r) {
  std::string s;
  s += "media-archive recovery report\n";
  s += "  codec:                 ";
  s += codec_name(r.codec);
  s += "\n";
  s += "  recovered_packets:     " + std::to_string(r.recovered_packets) +
       "\n";
  s += "  unrecovered_bytes:     " + std::to_string(r.unrecovered_bytes) +
       "\n";
  s += "  loss_recovery_rate:    " + fixed(r.loss_recovery_rate) + "\n";
  s += "  p95_latency_ms:        " + fixed(r.p95_latency_ms) + " (n=" +
       std::to_string(r.p95_sample_count) + ")\n";
  s += "  av_sync_error_ms:      " + fixed(r.av_sync_error_ms) + "\n";
  s += "  audio_sample_delta:    " + std::to_string(r.audio_sample_delta) +
       "\n";
  s += "  index_seek_error_ms:   " + fixed(r.index_seek_error_ms) + "\n";
  s += "  checkpoint_resumed:    ";
  s += r.checkpoint_resumed ? "true" : "false";
  s += "\n";
  s += "  keyframe_count:        " + std::to_string(r.keyframe_count) + "\n";
  s += "  gap_count:             " + std::to_string(r.gap_count) + "\n";
  s += "  uncertain_pts_count:   " + std::to_string(r.uncertain_pts_count) +
       "\n";
  s += "  rolled_back_block:     ";
  s += r.rolled_back_block == UINT64_MAX
           ? "none"
           : std::to_string(r.rolled_back_block);
  s += "\n";
  s += "  failure_offset:        ";
  s += r.failure_offset < 0 ? "none" : std::to_string(r.failure_offset);
  s += "\n";
  s += "  valid:                 ";
  s += r.valid ? "true" : "false";
  s += "\n";
  return s;
}

std::string render_report(const RecoveryReport& report, ReportFormat fmt) {
  return fmt == ReportFormat::Json ? render_report_json(report)
                                   : render_report_text(report);
}

}  // namespace media_archive
