#pragma once
// Timestamp repair on the shared media clock: 33 bit PTS wraparound,
// non-monotonic jump correction and configurable clock drift. Intervals
// that cannot be inferred uniquely are flagged uncertain and never
// silently rewritten.

#include <cstdint>
#include <vector>

#include "media_archive/types.h"

namespace media_archive {

struct TimestampConfig {
  uint64_t pts_modulo = kPtsModulo;       // 33 bit MPEG-TS style PTS
  double drift_ppm = 0.0;                 // sender clock drift to remove
  uint64_t nominal_frame_ticks = 0;       // 0 = auto-detect from cadence
  // A delta outside [1-tol, 1+tol] * nominal is an anomalous jump.
  double jump_tolerance = 0.25;
  // A jump adjacent to at least this many missing packets is ambiguous.
  uint32_t uncertainty_gap_threshold = 2;
};

struct TimestampResult {
  std::vector<RepairedPacket> packets;
  uint64_t wrap_count = 0;
  uint64_t corrected_jump_count = 0;
  uint64_t uncertain_count = 0;
  uint64_t nominal_frame_ticks = 0;
};

// Repair the PTS of sequence-ordered recovered packets. `gap_lengths`
// (same length semantics as the packet stream) may be empty; when present it
// maps each packet to the number of missing packets immediately before it.
TimestampResult repair_timestamps(
    const std::vector<TransportPacket>& ordered,
    const std::vector<uint32_t>& gap_lengths,
    const TimestampConfig& config = {});

}  // namespace media_archive
