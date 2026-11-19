#pragma once

// Repair report schema used by `repair` and the metric reader used by
// `verify`. Numeric formatting and structure come from json_writer.hpp.

#include <cstdint>
#include <string>

#include "media_types.hpp"
#include "pipeline.hpp"

namespace mfs {

// Serialises a full repair result. `frames` includes per-frame hashes.
std::string report_json(const RepairResult& result,
                        const PipelineOptions& options);

// Parses only the top-level numeric metrics needed by `verify`. Returns false
// when the report is missing or malformed.
struct ReportMetrics {
  double roundtrip_match_rate{-1.0};
  double keyframe_index_hit_rate{-1.0};
  double segment_hash_match_rate{-1.0};
  uint64_t repaired_frames{0};
  uint64_t duplicate_packets{0};
  uint64_t lost_ranges{0};
  uint64_t decode_errors{0};
  int64_t first_error_offset{-1};
};

bool parse_report_metrics(const std::string& json, ReportMetrics& out);

}  // namespace mfs
