#include "keyframe_index.hpp"

#include <algorithm>
#include <limits>

#include "byte_io.hpp"
#include "sha256.hpp"

namespace mfs {
namespace {

int64_t divide_round_half_up(int64_t numerator, int64_t denominator) {
  if (denominator == 0) {
    return 0;
  }
  const bool negative = (numerator < 0) != (denominator < 0);
  const uint64_t abs_num = static_cast<uint64_t>(
      negative ? -(numerator + 1) : numerator) + (negative ? 1u : 0u);
  const uint64_t abs_den = static_cast<uint64_t>(denominator < 0 ? -denominator
                                                                 : denominator);
  const uint64_t quotient = (abs_num + abs_den / 2) / abs_den;
  return negative ? -static_cast<int64_t>(quotient)
                  : static_cast<int64_t>(quotient);
}

}  // namespace

int64_t microseconds_to_ticks(int64_t ts_us, Rational time_base) {
  if (!time_base.valid()) {
    return 0;
  }
  // ticks = us / 1e6 / (num/den) = us * den / (1e6 * num)
  return divide_round_half_up(ts_us * static_cast<int64_t>(time_base.den),
                              static_cast<int64_t>(1000000) *
                                  static_cast<int64_t>(time_base.num));
}

KeyframeIndex::KeyframeIndex(Rational time_base, uint32_t max_segments)
    : time_base_(time_base.valid() ? time_base : kStreamTimeBase),
      max_segments_(std::max<uint32_t>(max_segments, 1)) {}

bool KeyframeIndex::build(const std::vector<RepairedFrame>& frames) {
  entries_.clear();
  frame_segment_.assign(frames.size(), 0);
  stats_ = IndexStats{};
  truncated_ = false;
  if (frames.empty()) {
    return false;
  }

  size_t segment_begin = std::numeric_limits<size_t>::max();
  size_t segment_end = 0;  // exclusive
  bool segment_open = false;

  auto close_segment = [&]() {
    if (!segment_open) {
      return true;
    }
    if (entries_.size() >= max_segments_) {
      // Segment budget exhausted: keep what was built and flag the cut so the
      // pipeline can surface it, rather than discarding valid work.
      truncated_ = true;
      return false;
    }
    finalise_segment(frames, segment_begin, segment_end);
    segment_open = false;
    return true;
  };

  for (size_t i = 0; i < frames.size(); ++i) {
    const RepairedFrame& frame = frames[i];
    if (!frame.decoded) {
      continue;
    }
    if (!segment_open || frame.keyframe) {
      if (!close_segment()) {
        break;  // budget exhausted; earlier segments stay valid
      }
      segment_begin = i;
      segment_open = true;
      ++stats_.keyframes;
    }
    segment_end = i + 1;
  }
  close_segment();
  stats_.segments = entries_.size();
  return !entries_.empty();
}

void KeyframeIndex::finalise_segment(const std::vector<RepairedFrame>& frames,
                                     size_t begin, size_t end) {
  if (begin >= end || end > frames.size()) {
    return;
  }
  const size_t slot = entries_.size();
  IndexEntry entry;
  entry.segment_id = static_cast<uint32_t>(slot);
  entry.keyframe_seq = frames[begin].seq;
  entry.start_ts_us = frames[begin].ts_us;
  entry.end_ts_us = frames[end - 1].ts_us + 1;
  entry.pts = microseconds_to_ticks(frames[begin].ts_us, time_base_);
  entry.time_base = time_base_;
  entry.source_offset = frames[begin].source_offset;

  Sha256 segment_hash;
  uint32_t count = 0;
  for (size_t i = begin; i < end; ++i) {
    if (!frames[i].decoded) {
      continue;
    }
    segment_hash.update(frames[i].frame_sha256.data(),
                        frames[i].frame_sha256.size());
    ++count;
  }
  entry.frame_count = count;
  entry.segment_sha256 = segment_hash.finish();
  entries_.push_back(entry);

  for (size_t i = begin; i < end; ++i) {
    if (frames[i].decoded) {
      frame_segment_[i] = slot;
    }
  }
}

SeekResult KeyframeIndex::seek(int64_t ts_us) const {
  SeekResult result;
  if (entries_.empty()) {
    return result;
  }
  // Last segment whose start_ts_us <= ts_us.
  auto it = std::upper_bound(
      entries_.begin(), entries_.end(), ts_us,
      [](int64_t ts, const IndexEntry& entry) { return ts < entry.start_ts_us; });
  if (it == entries_.begin()) {
    return result;  // before the first keyframe
  }
  --it;
  result.found = true;
  result.segment_index = static_cast<size_t>(it - entries_.begin());
  result.segment_id = it->segment_id;
  result.keyframe_seq = it->keyframe_seq;
  result.segment_start_us = it->start_ts_us;
  result.frame_ts_us = ts_us;
  result.time_base = it->time_base;
  result.source_offset = it->source_offset;
  return result;
}

}  // namespace mfs
