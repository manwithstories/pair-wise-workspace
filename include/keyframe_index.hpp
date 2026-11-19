#pragma once

// Random-access keyframe index.
//
// Every decoded frame belongs to the segment opened by the latest keyframe, so
// segment boundaries are always independently decodable. The index stores each
// segment's keyframe sequence, repaired start/end timestamps, pts expressed in
// the shared time base, source offset and a digest of the segment's frame
// hashes.
//
// Timestamp queries binary-search the keyframe start times; the result reports
// the segment's time base so callers can verify it matches the output frames.

#include <cstdint>
#include <vector>

#include "media_types.hpp"

namespace mfs {

struct SeekResult {
  bool found{false};
  size_t segment_index{0};
  uint32_t segment_id{0};
  uint64_t keyframe_seq{0};
  int64_t segment_start_us{0};
  int64_t frame_ts_us{0};
  Rational time_base{kStreamTimeBase};
  uint64_t source_offset{0};
};

struct IndexStats {
  uint64_t segments{0};
  uint64_t keyframes{0};
  uint64_t queries{0};
  uint64_t hits{0};
  uint64_t time_base_mismatches{0};
};

class KeyframeIndex {
 public:
  KeyframeIndex(Rational time_base, uint32_t max_segments = kDefaultMaxSegments);

  // Builds the index from repaired, sequence-ordered frames. Only frames that
  // decoded successfully become part of a segment. Returns false when no
  // segment could be built (empty or fully undecodable stream).
  bool build(const std::vector<RepairedFrame>& frames);

  // Finds the segment containing `ts_us`. Frames between keyframes resolve to
  // the preceding keyframe's segment.
  SeekResult seek(int64_t ts_us) const;

  const std::vector<IndexEntry>& entries() const { return entries_; }
  const IndexStats& stats() const { return stats_; }
  Rational time_base() const { return time_base_; }
  // True when the segment budget stopped the index before the end of the
  // stream; earlier segments remain queryable.
  bool truncated() const { return truncated_; }

 private:
  void finalise_segment(const std::vector<RepairedFrame>& frames, size_t begin,
                        size_t end);

  Rational time_base_;
  uint32_t max_segments_;
  std::vector<IndexEntry> entries_;
  std::vector<size_t> frame_segment_;  // frame index -> segment slot
  IndexStats stats_;
  bool truncated_{false};
};

// Converts microseconds to the given time base with half-up rounding.
int64_t microseconds_to_ticks(int64_t ts_us, Rational time_base);

}  // namespace mfs
