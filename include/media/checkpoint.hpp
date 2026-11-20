#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media/json.hpp"
#include "media/timeline.hpp"
#include "media/types.hpp"

namespace mediaqc {

// A durable record of how far a run got.  A checkpoint is only reusable when
// the input digest, configuration digest and output path all still match and
// every committed segment still hashes to what the checkpoint recorded.
// Anything else is refused rather than silently reused.
struct CommittedSegment {
  std::string track;
  std::string path;      // relative to the output directory
  std::string sha256;    // digest recorded when the segment was finalized
  std::uint64_t first_frame = 0;
  std::uint64_t frames = 0;
  Ns start_ns = 0;
  Ns end_ns = 0;         // exclusive end of the segment on the timeline
};

// One validated index entry carried across a resume so the final index covers
// everything already committed.
struct CommittedEntry {
  Ns time_ns = 0;
  std::uint64_t frame = 0;
  std::uint64_t frame_index = 0;
  std::string track;
  std::size_t segment = 0;
  std::uint64_t payload_offset = 0;
  std::uint64_t payload_size = 0;
  bool keyframe = false;
  std::string sha256;
};

struct Checkpoint {
  int version = 1;
  std::string input_sha256;
  std::string config_sha256;
  std::string output_path;
  std::uint64_t frames_committed = 0;
  std::uint64_t frames_repaired = 0;
  std::vector<std::uint64_t> records_consumed;  // per track, metadata lines read
  std::uint64_t peak_queue_frames = 0;
  std::string phase;                    // "writing", "finalized"
  std::vector<TimelineState> timeline;  // per-track timeline state
  std::vector<CommittedSegment> segments;
  std::vector<CommittedEntry> entries;
  Json extra;

  [[nodiscard]] Json to_json() const;
  [[nodiscard]] static bool from_json(const Json& value, Checkpoint& out,
                                      Error& error);
};

// Loads a checkpoint; a missing file yields false with `found == false`.
[[nodiscard]] bool load_checkpoint(const std::string& path, Checkpoint& out,
                                   bool& found, Error& error);

// Atomically writes a checkpoint: temp file, fsync, rename.  A crash leaves
// either the previous checkpoint or the new one, never a torn file.
[[nodiscard]] bool save_checkpoint(const std::string& path,
                                   const Checkpoint& checkpoint, Error& error);

// Removes a checkpoint after a successful run so it cannot be reused.
[[nodiscard]] bool remove_checkpoint(const std::string& path, Error& error);

// Digests the pipeline configuration so a changed queue size, segment size or
// output path invalidates the checkpoint.
[[nodiscard]] std::string config_digest(const std::string& input_path,
                                        const std::string& output_path,
                                        std::uint64_t queue_frames,
                                        std::uint64_t queue_bytes,
                                        std::uint64_t segment_frames);

}  // namespace mediaqc
