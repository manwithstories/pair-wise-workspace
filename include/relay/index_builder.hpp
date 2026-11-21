#pragma once

#include "relay/frame_record.hpp"
#include "relay/media_clock.hpp"

#include <cstdint>
#include <unordered_map>
#include <vector>

namespace relay {

struct IndexEntry {
  uint64_t frame_seq = 0;
  double time_seconds = 0.0;   // repaired presentation time
  uint32_t segment_id = 0;
  uint64_t segment_offset = 0; // byte offset of the frame in its segment
  uint64_t keyframe_seq = 0;   // nearest keyframe at or before this frame
};

// Traceable gap in the timeline (missing or duplicated frame seqs).
struct TimelineGap {
  uint64_t after_seq = 0;
  uint64_t missing = 0;
  double start_seconds = 0.0;
  double end_seconds = 0.0;
};

// Builds a random-access index over repaired frames. VFR-safe: entries
// store observed repaired times, never a synthesized fixed interval.
class IndexBuilder {
public:
  // Add one repaired frame in arrival order. Duplicate seqs are counted
  // and ignored; seq gaps produce TimelineGap records.
  void add(const FrameRecord& rec, const RepairedClock& clock,
           uint32_t segment_id, uint64_t segment_offset);

  const IndexEntry* find_by_seq(uint64_t seq) const;
  // Nearest entry at or before the given repaired time.
  const IndexEntry* find_by_time(double seconds) const;

  // Read-path validation: offset inside segment bounds and the referenced
  // keyframe really is a keyframe in the index.
  bool validate_entry(const IndexEntry& e, uint64_t segment_size) const;

  // Fraction of entries whose keyframe reference resolves to a keyframe.
  double keyframe_hit_rate() const;

  const std::vector<TimelineGap>& gaps() const { return gaps_; }
  const std::vector<IndexEntry>& entries() const { return entries_; }
  uint64_t duplicate_count() const { return duplicates_; }
  size_t size() const { return entries_.size(); }

private:
  std::vector<IndexEntry> entries_;
  std::unordered_map<uint64_t, size_t> by_seq_;
  std::unordered_map<uint64_t, bool> keyframe_flags_;
  std::vector<TimelineGap> gaps_;
  uint64_t duplicates_ = 0;
  uint64_t last_keyframe_seq_ = 0;
  bool has_keyframe_ = false;
  bool has_prev_ = false;
  uint64_t prev_seq_ = 0;
  double prev_seconds_ = 0.0;
};

} // namespace relay
