#pragma once

// End-to-end repair pipeline.
//
//   parse -> reorder/dedupe -> timestamp repair -> decode -> index -> verify
//
// The pipeline is deliberately a single function over an in-memory buffer so
// that every stage can be exercised from tests and from the CLI without
// touching global state. Recoverable damage is collected as typed errors with
// byte offsets; only unrecoverable structural problems surface as MediaError.

#include <cstdint>
#include <span>
#include <string>
#include <vector>

#include "media_types.hpp"
#include "packet_reader.hpp"
#include "reorder_buffer.hpp"
#include "timestamp_repair.hpp"

namespace mfs {

struct PipelineOptions {
  ParseOptions parse{};
  ReorderOptions reorder{};
  TimestampOptions timestamps{};
  Rational index_time_base{kStreamTimeBase};
  uint32_t max_segments{kDefaultMaxSegments};
  uint32_t segment_frames{kDefaultSegmentFrames};
  uint32_t frame_rate{30};
};

// Reads a whole file into memory. Throws MediaError(FileHeaderTruncated) when
// the file cannot be read.
std::vector<uint8_t> read_file(const std::string& path);

// Runs the full pipeline over `data`. Throws MediaError only for unrecoverable
// conditions (bad file header, unreadable input).
RepairResult run_pipeline(std::span<const uint8_t> data,
                          const PipelineOptions& options = {});

// Metrics thresholds used by `verify`.
struct AcceptanceThresholds {
  double roundtrip_match_rate{0.99};
  double keyframe_index_hit_rate{0.98};
  double segment_hash_match_rate{1.00};
};

bool meets_thresholds(const RepairResult& result,
                      const AcceptanceThresholds& thresholds);

}  // namespace mfs
