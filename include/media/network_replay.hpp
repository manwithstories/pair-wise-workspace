// Deterministic network trace replay: duplicate injection/de-duplication,
// reordering, burst loss and bandwidth-driven delay. All randomness comes from
// a fixed-seed mt19937 so identical input + trace always yields identical
// output. Sequence numbers are 16-bit and may wrap.
#pragma once

#include <cstdint>
#include <vector>

#include "media/packet.hpp"

namespace media {

struct NetworkConfig {
  std::uint32_t seed = 41;
  std::uint32_t loss_burst = 0;        // packets dropped in one burst
  std::uint32_t loss_burst_position = 60; // sequence where the burst starts
  std::uint32_t duplicate_count = 8;   // extra copies injected
  std::uint32_t reorder_count = 6;     // packets delayed past their successor
  std::uint32_t wrap_at = 0;           // force a 16-bit wrap at this seq count
  std::vector<std::uint32_t> bandwidth_kbps; // piecewise-constant trace
  std::uint32_t bandwidth_segment = 60;// packets per bandwidth segment
};

struct ReplayStats {
  std::uint64_t packets_in = 0;
  std::uint64_t packets_out = 0;
  std::uint64_t packets_dropped = 0;
  std::uint64_t packets_duplicate = 0; // duplicates detected and suppressed
  std::uint64_t packets_reordered = 0;
  std::uint64_t wraps = 0;
  std::vector<std::int64_t> arrival_us; // per-output arrival on media clock
};

// Replays packets through the trace in order. The returned vector preserves
// every surviving packet exactly once; arrival_us carries the (possibly
// reordered) receive timestamp. Input packets must already carry payloads.
struct ReplayOutput {
  std::vector<Packet> packets;
  ReplayStats stats;
};

ReplayOutput network_replay(std::vector<Packet> packets,
                            const NetworkConfig& cfg);

} // namespace media
