#pragma once
// Sequence repair: idempotent deduplication, wraparound-aware reordering,
// replay discrimination and burst-gap accounting, all on the shared
// 16 bit transport sequence number space.

#include <vector>

#include "media_archive/types.h"

namespace media_archive {

struct SequenceConfig {
  uint32_t modulo = kSeqModulo;
  uint32_t reorder_window = kReorderWindow;
};

// Repair a manifest as observed on the wire (any order, duplicates, gaps).
// Gap rows stay in the manifest but are excluded from `ordered`; their bytes
// are accumulated into unrecovered_bytes and expanded into GapIntervals.
// The returned packets are sorted by extended (unwrapped) sequence number.
SequenceResult repair_sequence(std::vector<TransportPacket> observed,
                               const SequenceConfig& config = {});

// Convert a raw 16 bit sequence number to an extended one given the current
// high-water mark. Exposed for testing and for the timestamp stage.
uint64_t extend_sequence(uint32_t raw, uint64_t high_water, uint32_t modulo,
                         uint32_t reorder_window);

// Signed forward distance from a to b in the modular number space.
int32_t modular_forward_distance(uint32_t a, uint32_t b, uint32_t modulo);

}  // namespace media_archive
