#pragma once

#include <atomic>
#include <cstdint>
#include <string>
#include <vector>

#include "media/checksum.hpp"
#include "media/checkpoint.hpp"
#include "media/delivery.hpp"
#include "media/index.hpp"
#include "media/json.hpp"
#include "media/timeline.hpp"
#include "media/types.hpp"

namespace mediaqc {

struct PipelineOptions {
  std::string input_dir;
  std::string output_dir;            // directory of segments + index + manifest
  std::string checkpoint_path;       // empty disables checkpointing
  std::uint64_t queue_frames = 8;    // max frames buffered between stages
  std::uint64_t queue_bytes = 256ull * 1024 * 1024;
  std::uint64_t segment_frames = 500;  // frames per output segment
  bool resume = true;                // honour an existing checkpoint
  bool force = false;                // replace non-empty output directory
  // Stop after this many source frames (0 = run to completion).  Used by the
  // "slow consumer"/interrupt demo and by resume tests.
  std::uint64_t stop_after_frames = 0;
};

struct PipelineReport {
  std::uint64_t frames_total = 0;
  std::uint64_t frames_repaired = 0;
  std::uint64_t frames_verified = 0;
  std::uint64_t frames_corrupt = 0;
  std::uint64_t frames_duplicate = 0;
  std::uint64_t frames_truncated = 0;
  std::int64_t first_corrupt_offset = -1;
  std::uint64_t peak_queue_frames = 0;
  std::uint64_t peak_queue_bytes = 0;
  std::uint64_t index_entries = 0;
  std::int64_t resume_ms = 0;
  bool resumed = false;
  bool interrupted = false;
  std::string output_sha256;  // digest covering all segment digests in order
  std::string input_sha256;
  bool ok = false;
  Error error;
  ErrorList errors;
  std::vector<RepairRecord> repairs;

  [[nodiscard]] Json to_json() const;
};

// Runs the pipeline.  Returns a report; `ok` may be true with a non-empty
// error list when the material was processed but found damaged (exit 3).
class Pipeline {
 public:
  explicit Pipeline(PipelineOptions options);

  // Request an asynchronous stop from another thread.  The current record
  // finishes, the checkpoint is flushed and run() returns.
  void cancel();

  [[nodiscard]] PipelineReport run();

 private:
  PipelineOptions options_;
  std::atomic<bool> cancel_requested_{false};
};

}  // namespace mediaqc
