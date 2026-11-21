#include "relay/index_builder.hpp"

#include <algorithm>

namespace relay {

void IndexBuilder::add(const FrameRecord& rec, const RepairedClock& clock,
                       uint32_t segment_id, uint64_t segment_offset) {
  const double t = rec.time_base.to_seconds(clock.repaired);

  if (by_seq_.count(rec.frame_seq) != 0) {
    ++duplicates_; // repeated delivery: keep the first, count the copy
    return;
  }
  if (has_prev_ && rec.frame_seq > prev_seq_ + 1) {
    gaps_.push_back({prev_seq_, rec.frame_seq - prev_seq_ - 1, prev_seconds_, t});
  }

  if (rec.keyframe) {
    last_keyframe_seq_ = rec.frame_seq;
    has_keyframe_ = true;
  }
  IndexEntry e;
  e.frame_seq = rec.frame_seq;
  e.time_seconds = t;
  e.segment_id = segment_id;
  e.segment_offset = segment_offset;
  e.keyframe_seq = has_keyframe_ ? last_keyframe_seq_ : rec.frame_seq;

  by_seq_[e.frame_seq] = entries_.size();
  keyframe_flags_[e.frame_seq] = rec.keyframe;
  entries_.push_back(e);
  prev_seq_ = rec.frame_seq;
  prev_seconds_ = t;
  has_prev_ = true;
}

const IndexEntry* IndexBuilder::find_by_seq(uint64_t seq) const {
  auto it = by_seq_.find(seq);
  return it == by_seq_.end() ? nullptr : &entries_[it->second];
}

const IndexEntry* IndexBuilder::find_by_time(double seconds) const {
  if (entries_.empty()) return nullptr;
  const IndexEntry* best = &entries_.front();
  for (const auto& e : entries_) {
    if (e.time_seconds <= seconds) best = &e;
    else break;
  }
  return best;
}

bool IndexBuilder::validate_entry(const IndexEntry& e,
                                  uint64_t segment_size) const {
  if (e.segment_offset >= segment_size) return false; // offset out of bounds
  auto it = keyframe_flags_.find(e.keyframe_seq);
  if (it == keyframe_flags_.end()) return false; // dangling keyframe ref
  return it->second; // keyframe hit: reference must be a real keyframe
}

double IndexBuilder::keyframe_hit_rate() const {
  if (entries_.empty()) return 1.0;
  uint64_t hits = 0;
  for (const auto& e : entries_) {
    auto it = keyframe_flags_.find(e.keyframe_seq);
    if (it != keyframe_flags_.end() && it->second) ++hits;
  }
  return static_cast<double>(hits) / static_cast<double>(entries_.size());
}

} // namespace relay
