#include <cinttypes>
#include <cmath>
#include <cstdio>
#include <string>
#include <vector>

#include "cli_options.hpp"
#include "commands.hpp"
#include "json_report.hpp"
#include "media_types.hpp"
#include "pipeline.hpp"

namespace mfs {

int cmd_verify(const OptionTable& options) {
  std::string input;
  std::string report_path;
  if (!options.get("input", input) || !options.get("report", report_path)) {
    std::fprintf(stderr, "verify requires --input --report\n");
    return kExitUsageOrFormat;
  }
  std::vector<uint8_t> data;
  try {
    data = read_file(input);
  } catch (const MediaError& error) {
    std::fprintf(stderr, "%s\n", error.what());
    return kExitUsageOrFormat;
  }
  std::string report;
  if (!read_text_file(report_path, report)) {
    std::fprintf(stderr, "cannot read report: %s\n", report_path.c_str());
    return kExitUsageOrFormat;
  }
  ReportMetrics metrics;
  if (!parse_report_metrics(report, metrics)) {
    std::fprintf(stderr, "report is missing required metrics: %s\n",
                 report_path.c_str());
    return kExitUsageOrFormat;
  }

  // Re-run from the source bytes and compare against the stored summary, so a
  // stale or tampered report cannot pass verification.
  const RepairResult fresh = run_pipeline(data);
  const double epsilon = 1e-9;
  const bool consistent =
      std::fabs(fresh.roundtrip_match_rate - metrics.roundtrip_match_rate) < epsilon &&
      std::fabs(fresh.keyframe_index_hit_rate - metrics.keyframe_index_hit_rate) < epsilon &&
      std::fabs(fresh.segment_hash_match_rate - metrics.segment_hash_match_rate) < epsilon &&
      fresh.repaired_frames == metrics.repaired_frames &&
      fresh.duplicate_packets == metrics.duplicate_packets &&
      fresh.lost_ranges == metrics.lost_ranges &&
      fresh.decode_errors == metrics.decode_errors &&
      fresh.first_error_offset == metrics.first_error_offset;

  AcceptanceThresholds thresholds;
  const bool pass = meets_thresholds(fresh, thresholds) && consistent;
  std::printf("{\n");
  std::printf("  \"roundtrip_match_rate\": %.6f,\n", fresh.roundtrip_match_rate);
  std::printf("  \"keyframe_index_hit_rate\": %.6f,\n",
              fresh.keyframe_index_hit_rate);
  std::printf("  \"segment_hash_match_rate\": %.6f,\n",
              fresh.segment_hash_match_rate);
  std::printf("  \"repaired_frames\": %" PRIu64 ",\n", fresh.repaired_frames);
  std::printf("  \"duplicate_packets\": %" PRIu64 ",\n", fresh.duplicate_packets);
  std::printf("  \"lost_ranges\": %" PRIu64 ",\n", fresh.lost_ranges);
  std::printf("  \"decode_errors\": %" PRIu64 ",\n", fresh.decode_errors);
  std::printf("  \"first_error_offset\": %" PRId64 ",\n", fresh.first_error_offset);
  std::printf("  \"report_consistent\": %s,\n", consistent ? "true" : "false");
  std::printf("  \"status\": \"%s\"\n", pass ? "pass" : "fail");
  std::printf("}\n");
  if (!consistent) {
    std::fprintf(stderr, "report does not match a fresh run of the input\n");
  }
  return pass ? kExitOk : kExitThreshold;
}

}  // namespace mfs
