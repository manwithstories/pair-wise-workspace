#pragma once
// Self-generated, fully deterministic transport trace: time-varying
// bandwidth, queueing latency, burst losses, duplicates and reordering.
// No external data and no network access — everything derives from a seed.

#include <cstdint>
#include <vector>

#include "media_archive/types.h"

namespace media_archive {

struct TraceConfig {
  uint64_t seed = 42;
  double frame_interval_ms = 33.3333;  // ~30 fps
  double mean_bandwidth_kbps = 4000.0; // average link capacity
  double burst_loss_prob = 0.012;      // per-packet chance to start a burst
  uint32_t max_burst_len = 6;          // longest burst loss run
  double duplicate_prob = 0.03;
  double reorder_prob = 0.05;
  // The normal sample must recover >= 90% of recoverable packets, so the
  // generated loss budget stays well below 10%.
};

struct TransportTrace {
  std::vector<TransportPacket> packets; // includes Gap and Duplicate rows
  std::vector<double> bandwidth_kbps;   // one sample per emitted frame
  std::vector<double> latencies_ms;     // one sample per delivered packet
  uint64_t generated_count = 0;         // unique frames before duplication
  uint64_t gap_packet_count = 0;
};

// Deterministic PRNG (xorshift64*) — same seed always yields the same trace,
// which keeps repeated runs bit-for-bit reproducible.
class DeterministicRng {
 public:
  explicit DeterministicRng(uint64_t seed);
  uint64_t next_u64();
  // Uniform double in [0, 1).
  double next_unit();
  // Uniform integer in [lo, hi].
  uint32_t uniform(uint32_t lo, uint32_t hi);

 private:
  uint64_t state_;
};

// Build a trace for `frame_count` frames of the given average encoded size.
TransportTrace build_transport_trace(uint32_t frame_count,
                                     const std::vector<uint32_t>& frame_sizes,
                                     const TraceConfig& config);

// Percentile (linear interpolation) of a latency sample set.
double percentile(std::vector<double> values, double p);

}  // namespace media_archive
