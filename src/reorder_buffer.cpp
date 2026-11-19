#include "reorder_buffer.hpp"

#include <algorithm>
#include <cmath>
#include <utility>

#include "byte_io.hpp"

namespace mfs {
namespace {

int64_t metadata_arrival_us(const Packet& packet) {
  for (const MetadataField& field : packet.header.metadata) {
    if (field.id == kFieldArrivalUs && field.value.size() == 8) {
      return static_cast<int64_t>(ByteReader(field.value).u64());
    }
  }
  return -1;
}

}  // namespace

ReorderBuffer::ReorderBuffer(ReorderOptions options) : options_(options) {
  options_.base_window = std::max<uint32_t>(options_.base_window, 1);
  options_.max_window = std::max(options_.max_window, options_.base_window);
  options_.max_buffered_packets =
      std::max<size_t>(options_.max_buffered_packets, 1);
  options_.max_recent_sequences =
      std::max<size_t>(options_.max_recent_sequences, 64);
  options_.nominal_interval_us = std::max<int64_t>(options_.nominal_interval_us, 1);
  effective_window_ = options_.base_window;
  stats_.effective_window = effective_window_;
}

void ReorderBuffer::observe_arrival(const Packet& packet) {
  const int64_t arrival = metadata_arrival_us(packet);
  if (arrival < 0) {
    return;
  }
  if (last_arrival_us_ >= 0) {
    last_gap_us_ = std::max<int64_t>(arrival - last_arrival_us_, 0);
  }
  last_arrival_us_ = arrival;
  stats_.last_gap_us = last_gap_us_;
}

// The window follows the arrival-time gap expressed in frame intervals. A gap
// above two intervals signals a stalled link that will deliver a burst, so the
// window widens by the observed skew; ordinary gaps decay the window back
// towards the configured base with an exponential moving average.
void ReorderBuffer::update_window() {
  if (!options_.adaptive_window || last_gap_us_ < 0) {
    effective_window_ = options_.base_window;
    stats_.effective_window = effective_window_;
    return;
  }
  const double nominal = static_cast<double>(options_.nominal_interval_us);
  const double gap_frames = static_cast<double>(last_gap_us_) / nominal;
  const double base = static_cast<double>(options_.base_window);
  const auto previous = effective_window_;
  if (gap_frames > 2.0) {
    jitter_frames_ = gap_frames;
    effective_window_ = static_cast<uint32_t>(
        std::clamp(std::ceil(base + gap_frames), base,
                   static_cast<double>(options_.max_window)));
  } else {
    jitter_frames_ = jitter_frames_ * 0.8 + gap_frames * 0.2;
    const auto decayed = static_cast<uint32_t>(
        std::clamp(std::ceil(base + jitter_frames_), base,
                   static_cast<double>(options_.max_window)));
    effective_window_ = std::min(effective_window_, decayed);
  }
  if (effective_window_ > previous) {
    ++stats_.adaptive_widenings;
  }
  stats_.effective_window = effective_window_;
}

void ReorderBuffer::note_released(uint64_t seq) {
  released_.insert(seq);
  while (released_.size() > options_.max_recent_sequences) {
    released_.erase(released_.begin());
  }
}

void ReorderBuffer::record_gap(uint64_t first, uint64_t last) {
  if (last < first) {
    return;
  }
  if (!gaps_.empty() && gaps_.back().last_seq + 1 == first) {
    gaps_.back().last_seq = last;
  } else {
    gaps_.push_back(SequenceGap{first, last});
  }
}

// Releases the contiguous run starting at next_seq_.
void ReorderBuffer::release_ready() {
  while (true) {
    auto it = pending_.find(next_seq_);
    if (it == pending_.end()) {
      break;
    }
    ready_.push_back(std::move(it->second));
    pending_.erase(it);
    note_released(next_seq_);
    ++next_seq_;
    ++stats_.released;
  }
}

void ReorderBuffer::force_release() {
  // Abandon the hole blocking progress and jump to the smallest buffered
  // sequence past it. The skipped range is recorded as a gap.
  if (!pending_.empty() && pending_.begin()->first > next_seq_) {
    const uint64_t first = next_seq_;
    next_seq_ = pending_.begin()->first;
    record_gap(first, next_seq_ - 1);
    stats_.abandoned += next_seq_ - first;
  }
  release_ready();
}

std::vector<Packet> ReorderBuffer::push(Packet packet) {
  ready_.clear();
  const uint64_t seq = packet.header.frame_seq;
  observe_arrival(packet);
  update_window();

  if (!started_) {
    started_ = true;
    next_seq_ = seq;
    highest_seq_ = seq;
  }
  highest_seq_ = std::max(highest_seq_, seq);

  if (seq < next_seq_ && released_.count(seq) > 0) {
    ++stats_.duplicates;
    return {};
  }
  if (seq < next_seq_) {
    // Before anything has been emitted, `next_seq_` can still recede so that
    // an earlier arrival extends the window backwards.
    if (stats_.released == 0) {
      next_seq_ = seq;
    } else {
      ++stats_.out_of_window;
      errors_.push_back(RepairError{ErrorType::OutOfWindow, packet.raw_offset,
                                    static_cast<uint32_t>(seq),
                                    "packet arrived after its sequence was passed"});
      return {};
    }
  }
  if (pending_.count(seq) > 0) {
    ++stats_.duplicates;
    return {};
  }

  ++stats_.accepted;
  pending_.emplace(seq, std::move(packet));
  stats_.buffered_peak = std::max(stats_.buffered_peak, pending_.size());

  // The very first packet is held back briefly: emitting it immediately would
  // classify any earlier sequence as late. Once a second packet arrives (or
  // the first one has waited a full window) the contiguous prefix is released.
  const bool window_exhausted =
      highest_seq_ >= next_seq_ && highest_seq_ - next_seq_ >= effective_window_;
  if (stats_.released > 0 || pending_.size() >= 2 || window_exhausted) {
    release_ready();
  }
  // A hole at the head that outlived a full window is abandoned so the stream
  // keeps moving; the skipped range is recorded as a gap.
  if (!pending_.empty() && highest_seq_ >= next_seq_ &&
      highest_seq_ - next_seq_ >= effective_window_) {
    force_release();
  }
  if (pending_.size() > options_.max_buffered_packets) {
    force_release();
  }
  return std::move(ready_);
}

std::vector<Packet> ReorderBuffer::flush() {
  ready_.clear();
  if (!pending_.empty() && pending_.begin()->first != next_seq_) {
    record_gap(next_seq_, pending_.begin()->first - 1);
  }
  for (auto& entry : pending_) {
    ready_.push_back(std::move(entry.second));
    note_released(entry.first);
    ++stats_.released;
  }
  pending_.clear();
  if (!ready_.empty()) {
    next_seq_ = ready_.back().header.frame_seq + 1;
  }
  return std::move(ready_);
}

}  // namespace mfs
