#pragma once

// Bounded jitter buffer for out-of-order packet delivery.
//
// A packet with sequence S is released once a packet at least `window`
// sequences ahead has arrived, so reordering up to the window depth is
// absorbed. A hole that blocks progress beyond the window is abandoned and
// recorded as a SequenceGap; packets arriving for an abandoned sequence are
// reported as OutOfWindow together with their raw byte offset. No packet is
// ever fabricated.
//
// The window adapts to observed perturbations: inter-arrival gaps are compared
// with the nominal frame interval, so a stalled link that later delivers a
// burst widens the buffer instead of dropping recoverable frames. Pending and
// recently-released bookkeeping are both bounded.

#include <cstdint>
#include <map>
#include <set>
#include <vector>

#include "media_types.hpp"

namespace mfs {

struct ReorderOptions {
  uint32_t base_window{kDefaultReorderWindow};
  uint32_t max_window{256};
  size_t max_buffered_packets{4096};
  size_t max_recent_sequences{8192};
  bool adaptive_window{true};
  int64_t nominal_interval_us{33333};
};

// A run of sequence numbers that will never be delivered. Timestamp estimates
// are filled in later by the repair stage, which sees neighbouring frames.

struct ReorderStats {
  uint64_t accepted{0};
  uint64_t duplicates{0};
  uint64_t out_of_window{0};
  uint64_t released{0};
  uint64_t abandoned{0};
  size_t buffered_peak{0};
  uint32_t effective_window{0};
  uint64_t adaptive_widenings{0};
  int64_t last_gap_us{-1};
};

class ReorderBuffer {
 public:
  explicit ReorderBuffer(ReorderOptions options = {});

  // Adds a packet in arrival order; returns newly released packets ordered by
  // sequence number.
  std::vector<Packet> push(Packet packet);

  // Releases everything still buffered in sequence order; used at end of
  // stream.
  std::vector<Packet> flush();

  const std::vector<RepairError>& errors() const { return errors_; }
  const std::vector<SequenceGap>& gaps() const { return gaps_; }
  const ReorderStats& stats() const { return stats_; }
  size_t buffered_count() const { return pending_.size(); }
  uint32_t effective_window() const { return effective_window_; }

 private:
  void observe_arrival(const Packet& packet);
  void update_window();
  void release_ready();
  void force_release();
  void record_gap(uint64_t first, uint64_t last);
  void note_released(uint64_t seq);

  ReorderOptions options_;
  std::map<uint64_t, Packet> pending_;
  std::set<uint64_t> released_;
  std::vector<RepairError> errors_;
  std::vector<SequenceGap> gaps_;
  ReorderStats stats_;
  std::vector<Packet> ready_;
  uint64_t next_seq_{0};
  uint64_t highest_seq_{0};
  bool started_{false};
  uint32_t effective_window_{0};
  double jitter_frames_{0.0};
  int64_t last_arrival_us_{-1};
  int64_t last_gap_us_{-1};
};

}  // namespace mfs
