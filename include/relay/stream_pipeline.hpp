#pragma once

#include "relay/frame_decoder.hpp"
#include "relay/frame_record.hpp"
#include "relay/index_builder.hpp"
#include "relay/media_clock.hpp"
#include "relay/segment_writer.hpp"

#include <cstdint>
#include <deque>
#include <string>

namespace relay {

struct PipelineConfig {
  size_t queue_capacity = 64;
  uint32_t segment_frames = 120;
  ClockRepairConfig clock;
};

struct PipelineStats {
  uint64_t input_records = 0;
  uint64_t parse_rejected = 0;    // per-record field rejects
  uint64_t checksum_isolated = 0; // frames isolated on checksum mismatch
  uint64_t decode_failed = 0;     // frames isolated on decode failure
  uint64_t clock_errors = 0;      // frames kept but flagged by repair
  uint64_t repaired_wraps = 0;
  uint64_t duplicates = 0;
  uint64_t gaps = 0;
  uint64_t frames_written = 0;
  size_t peak_buffered_frames = 0;
  uint64_t backpressure_events = 0; // producer paused: queue at capacity
  uint64_t resume_events = 0;       // producer resumed after drain
};

enum class PipelineStatus {
  Ok = 0,
  InputError = 2,  // bad arguments / unreadable input
  MediaError = 3,  // unrecoverable container structure error
  WriteError = 4,  // segment write / recovery failure
};

// Single-threaded streaming pipeline: parse -> isolate corrupt -> decode
// -> clock repair -> segment write -> index. A bounded queue between the
// parse stage and the consumer applies backpressure; memory stays flat
// regardless of total input frames.
class StreamPipeline {
public:
  StreamPipeline(PipelineConfig cfg, std::string out_dir);

  PipelineStatus run(const std::string& input_path);

  const PipelineStats& stats() const { return stats_; }
  const IndexBuilder& index() const { return index_; }
  const SegmentWriter& writer() const { return writer_; }
  const ParseResult& structural_error() const { return structural_err_; }

private:
  // Consumer side of the bounded queue; returns false on write failure.
  bool drain_one();

  PipelineConfig cfg_;
  SegmentWriter writer_;
  IndexBuilder index_;
  ClockRepairer repairer_;
  std::deque<FrameRecord> queue_;
  PipelineStats stats_;
  ParseResult structural_err_;
};

} // namespace relay
