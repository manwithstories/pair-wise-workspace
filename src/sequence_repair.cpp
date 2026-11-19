#include <algorithm>
#include <unordered_map>

#include "media_archive/sequence_repair.h"

namespace media_archive {

int32_t modular_forward_distance(uint32_t a, uint32_t b, uint32_t modulo) {
  const uint32_t diff = (b - a) % modulo;
  // Values beyond half the space are interpreted as backwards (replays).
  return diff > modulo / 2 ? static_cast<int32_t>(diff) -
                                 static_cast<int32_t>(modulo)
                           : static_cast<int32_t>(diff);
}

uint64_t extend_sequence(uint32_t raw, uint64_t high_water, uint32_t modulo,
                         uint32_t reorder_window) {
  const uint64_t base_cycle = high_water / modulo;
  const uint64_t same_cycle = base_cycle * modulo + raw;
  const int32_t dist =
      modular_forward_distance(static_cast<uint32_t>(high_water % modulo),
                               raw, modulo);
  // Pick the cycle closest to the high-water mark in modular space.
  if (dist > 0 && same_cycle < high_water)
    return same_cycle + modulo;  // next cycle (e.g. 2 after 65535)
  if (dist < 0 && same_cycle > high_water)
    return same_cycle - modulo;  // previous cycle (late/replay packet)
  (void)reorder_window;
  return same_cycle;
}

SequenceResult repair_sequence(std::vector<TransportPacket> observed,
                               const SequenceConfig& cfg) {
  const uint32_t modulo = cfg.modulo;
  SequenceResult result;
  // Recovered (present) packets and known gaps are tracked separately so a
  // late arrival can never silently overwrite a gap record.
  std::unordered_map<uint64_t, TransportPacket> present;
  std::unordered_map<uint64_t, TransportPacket> gaps;
  present.reserve(observed.size());
  gaps.reserve(observed.size());

  uint64_t high_water = 0;
  bool have_anchor = false;

  for (const TransportPacket& p : observed) {
    if (!have_anchor) {
      high_water = p.seq;
      have_anchor = true;
    }
    const int32_t fwd =
        modular_forward_distance(static_cast<uint32_t>(high_water % modulo),
                                 p.seq, modulo);
    // Outside the reorder window in both directions and not a wraparound:
    // the packet belongs to an older generation, so it is a replay. This is
    // what separates 16 bit wraps (forward distance 1) from old replays
    // (forward distance thousands of packets away).
    if (fwd > static_cast<int32_t>(cfg.reorder_window) &&
        fwd < static_cast<int32_t>(modulo - cfg.reorder_window)) {
      result.replay_count++;
      continue;
    }
    const uint64_t ext =
        extend_sequence(p.seq, high_water, cfg.modulo, cfg.reorder_window);

    if (p.flag == PacketFlag::Gap) {
      if (ext > high_water) high_water = ext;
      gaps.emplace(ext, p);
      result.unrecovered_bytes += p.size_bytes;
      continue;
    }
    // Anything observed far behind the established high-water mark is an old
    // replay rather than a late packet, even if this exact sequence number
    // was never stored (e.g. it belonged to a previous generation).
    if (high_water > ext + cfg.reorder_window) {
      result.replay_count++;
      continue;
    }
    if (ext > high_water) high_water = ext;
    if (gaps.count(ext)) {
      // A packet whose sequence number is already recorded as lost: treat as
      // a replay of a missing slot, never as recovered content.
      result.replay_count++;
      continue;
    }
    auto [it, inserted] = present.emplace(ext, p);
    if (!inserted) {
      // Second copy: a far-behind copy is a replay, a close one a duplicate.
      const uint64_t behind = high_water - ext;
      if (behind > cfg.reorder_window)
        result.replay_count++;
      else
        result.duplicate_count++;
    }
  }

  // Expected unique sequence numbers = recovered packets + gap markers.
  result.expected_unique = present.size() + gaps.size();

  std::vector<uint64_t> keys;
  keys.reserve(result.expected_unique);
  for (const auto& [k, v] : present) keys.push_back(k);
  for (const auto& [k, v] : gaps) keys.push_back(k);
  std::sort(keys.begin(), keys.end());

  bool in_gap = false;
  GapInterval current{};
  uint32_t pending_gap_len = 0;
  for (uint64_t ext : keys) {
    auto gap_it = gaps.find(ext);
    if (gap_it != gaps.end()) {
      const TransportPacket& p = gap_it->second;
      if (!in_gap) {
        in_gap = true;
        current = {ext, ext, p.size_bytes};
        pending_gap_len = 1;
      } else {
        current.extended_seq_last = ext;
        current.missing_bytes += p.size_bytes;
        ++pending_gap_len;
      }
    } else {
      if (in_gap) {
        result.gaps.push_back(current);
        in_gap = false;
      }
      result.gap_before.push_back(pending_gap_len);
      pending_gap_len = 0;
      result.ordered.push_back(present.find(ext)->second);
    }
  }
  if (in_gap) result.gaps.push_back(current);
  return result;
}

}  // namespace media_archive
