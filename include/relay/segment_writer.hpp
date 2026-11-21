#pragma once

#include "relay/frame_record.hpp"

#include <cstdint>
#include <string>
#include <vector>

namespace relay {

struct SegmentInfo {
  uint32_t id = 0;
  std::string path;   // final .bin path
  uint64_t frames = 0;
  uint64_t bytes = 0;
  Sha256 sha256{};
};

// Writes frames into fixed-size segments using a temp file + done marker.
// A segment is only visible (and indexable) after its marker lands; on
// restart, incomplete temp files are discarded and only segments whose
// hash matches the manifest are resumed.
class SegmentWriter {
public:
  SegmentWriter(std::string out_dir, uint32_t frames_per_segment);

  // Scans out_dir: verifies completed segments against the manifest,
  // drops incomplete temp files, resumes after the last verified id.
  bool recover();

  // Appends one frame; rolls to a new segment when the current is full.
  // Returns the frame's byte offset inside its segment, or UINT64_MAX on
  // write failure (failed() then returns true).
  uint64_t write_frame(const FrameRecord& rec);

  // Seals any open segment and flushes the manifest.
  bool finalize();

  const std::vector<SegmentInfo>& completed() const { return completed_; }
  uint32_t current_segment_id() const { return current_id_; }
  bool failed() const { return failed_; }
  const std::string& out_dir() const { return out_dir_; }

  // Verifies a completed segment file against its expected hash.
  static bool verify_file(const std::string& path, const Sha256& expected,
                          uint64_t expected_bytes);

private:
  bool seal_current();
  bool flush_manifest() const;
  std::string tmp_path(uint32_t id) const;
  std::string bin_path(uint32_t id) const;
  std::string done_path(uint32_t id) const;

  std::string out_dir_;
  uint32_t frames_per_segment_;
  std::vector<SegmentInfo> completed_;
  uint32_t current_id_ = 0;
  std::FILE* cur_ = nullptr;
  uint64_t cur_frames_ = 0;
  uint64_t cur_bytes_ = 0;
  bool failed_ = false;
};

} // namespace relay
