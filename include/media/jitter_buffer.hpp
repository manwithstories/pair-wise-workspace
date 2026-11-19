// Adaptive jitter buffer: reorders wrap-safe sequences, suppresses duplicates,
// detects gaps without deadlocking on repeated timestamps, and adjusts its
// target depth from a sliding window of inter-arrival jitter and the current
// bandwidth estimate. Depth is always clamped to hard min/max bounds so a
// weak network cannot make wait time grow without limit.
#pragma once

#include <cstdint>
#include <memory>
#include <vector>

#include "media/network_replay.hpp"
#include "media/packet.hpp"

namespace media {

struct JitterConfig {
  std::int64_t min_depth_us = 40'000;    // hard lower bound
  std::int64_t max_depth_us = 200'000;   // hard upper bound
  std::uint32_t window_size = 32;        // sliding jitter window
  std::uint32_t max_buffered = kMaxBufferedPackets;
};

struct JitterStats {
  std::uint64_t pushed = 0;
  std::uint64_t emitted = 0;
  std::uint64_t duplicates_suppressed = 0;
  std::uint64_t gaps_detected = 0;
  std::int64_t target_depth_us = 0;
  std::int64_t jitter_p95_us = 0;
  std::vector<std::int64_t> latency_us;  // arrival -> playout per packet
};

// A sequence gap observed at playout time, handed to the recovery module.
struct Gap {
  std::uint16_t first_missing = 0;
  std::uint32_t count = 0;               // consecutive missing sequences
  std::uint64_t first_linear = 0;        // linear index of first missing packet
  std::int64_t detected_at_us = 0;
};

class JitterBuffer {
 public:
  explicit JitterBuffer(const JitterConfig& cfg = {});

  // Insert a packet together with its (possibly reordered) arrival timestamp.
  // Repeated timestamps and wrapped sequences never block insertion.
  void push(Packet p, std::int64_t arrival_us);

  // Update the bandwidth estimate (kbps) used by the depth adaptation rule.
  void update_bandwidth(std::uint32_t bandwidth_kbps);

  // Emit packets whose playout time has arrived. New gaps are appended to
  // *gaps so recovery can act on them; each gap is reported exactly once.
  std::vector<Packet> drain(std::int64_t now_us, std::vector<Gap>* gaps);

  // Flush everything still buffered (end of stream); remaining gaps reported.
  std::vector<Packet> flush(std::vector<Gap>* gaps);

  const JitterStats& stats() const noexcept { return stats_; }
  std::size_t buffered() const noexcept;

  ~JitterBuffer();

  // Public so the implementation's priority-queue comparator can name it.
  struct Entry {
    Packet packet;
    std::int64_t arrival_us;
  };

 private:
  struct Impl;

  JitterConfig cfg_;
  JitterStats stats_;
  std::unique_ptr<Impl> impl_;
  std::vector<std::int64_t> jitter_window_;
  std::int64_t last_arrival_us_ = 0;
  std::int64_t playout_clock_us_ = 0;
  std::uint64_t next_linear_ = 0;
  std::uint32_t bandwidth_kbps_ = 2000;
  bool have_next_ = false;

  void adapt_depth();
  void record_gap(std::uint64_t from_linear, std::uint32_t count,
                  std::int64_t now_us, std::vector<Gap>* gaps);
};

} // namespace media
