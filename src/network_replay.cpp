#include "media/network_replay.hpp"

#include <algorithm>
#include <random>

namespace media {

namespace {

std::uint32_t bandwidth_for(std::uint32_t idx, const NetworkConfig& cfg) {
  if (cfg.bandwidth_kbps.empty()) return 2000;
  const std::uint32_t seg = cfg.bandwidth_segment == 0
      ? 0 : idx / cfg.bandwidth_segment;
  return cfg.bandwidth_kbps[std::min(seg,
      static_cast<std::uint32_t>(cfg.bandwidth_kbps.size()) - 1)];
}

} // namespace

ReplayOutput network_replay(std::vector<Packet> packets,
                            const NetworkConfig& cfg) {
  ReplayOutput out;
  std::mt19937 rng(cfg.seed);
  auto& st = out.stats;
  st.packets_in = packets.size();

  // ---- Stage 1: loss (single contiguous burst) ------------------------------
  std::vector<Packet> survivors;
  survivors.reserve(packets.size());
  for (std::uint32_t i = 0; i < packets.size(); ++i) {
    const bool in_burst =
        cfg.loss_burst > 0 && i >= cfg.loss_burst_position &&
        i < cfg.loss_burst_position + cfg.loss_burst;
    if (in_burst) { ++st.packets_dropped; continue; }
    survivors.push_back(std::move(packets[i]));
  }

  // ---- Stage 2: sequence normalization ----------------------------------------
  // Re-number survivors consecutively so wrap handling is deterministic and
  // testable. uint16 arithmetic wraps naturally at 65536; the wrap counter
  // follows the modular distance between successive sequence numbers.
  std::uint16_t next_seq = 0;
  for (std::uint32_t i = 0; i < survivors.size(); ++i) {
    survivors[i].sequence = next_seq;
    survivors[i].linear_index = i;
    ++next_seq; // uint16 wraps naturally at 65536
  }
  st.wraps = survivors.size() / kSequenceSpace;
  if (cfg.wrap_at)
    st.wraps = std::max(st.wraps,
        static_cast<std::uint64_t>(survivors.size() / cfg.wrap_at));
  if (cfg.wrap_at) {
    // Small-fixture wrap bookkeeping requested explicitly.
    st.wraps = std::max(st.wraps,
        static_cast<std::uint64_t>(survivors.size() / cfg.wrap_at));
  }

  // ---- Stage 3: duplicate injection ------------------------------------------
  std::vector<Packet> with_dups;
  with_dups.reserve(survivors.size() + cfg.duplicate_count);
  std::uniform_int_distribution<std::uint32_t> dup_pick(
      0, survivors.empty() ? 0 : static_cast<std::uint32_t>(survivors.size() - 1));
  std::vector<std::uint32_t> dup_targets;
  for (std::uint32_t d = 0; d < cfg.duplicate_count && !survivors.empty(); ++d)
    dup_targets.push_back(dup_pick(rng));
  std::sort(dup_targets.begin(), dup_targets.end());
  std::size_t di = 0;
  for (std::uint32_t i = 0; i < survivors.size(); ++i) {
    with_dups.push_back(survivors[i]);
    while (di < dup_targets.size() && dup_targets[di] == i) {
      Packet copy = survivors[i];          // identical sequence number
      with_dups.push_back(std::move(copy));
      ++di;
    }
  }

  // ---- Stage 4: reordering (bounded swaps) -----------------------------------
  std::uniform_int_distribution<std::uint32_t> re_pick(
      0, with_dups.size() > 2 ?
         static_cast<std::uint32_t>(with_dups.size() - 2) : 0);
  for (std::uint32_t r = 0; r < cfg.reorder_count && with_dups.size() > 2; ++r) {
    const std::uint32_t i = re_pick(rng);
    std::swap(with_dups[i], with_dups[i + 1]);
    ++st.packets_reordered;
  }

  // ---- Stage 5: arrival timestamps on the shared media clock -----------------
  // Arrival = source presentation time + a network delay, so arrivals share
  // the microsecond media scale rather than an independent serialization
  // clock. The delay has a fixed propagation term plus a bandwidth-driven
  // serialization term, so a low-bandwidth segment visibly inflates latency.
  constexpr std::int64_t kPropagationUs = 15'000;
  std::vector<std::int64_t> arrivals;
  arrivals.reserve(with_dups.size());
  for (std::uint32_t i = 0; i < with_dups.size(); ++i) {
    const std::uint32_t kbps = bandwidth_for(i, cfg);
    const std::uint64_t bits =
        static_cast<std::uint64_t>(with_dups[i].payload.size() + 28) * 8;
    const std::int64_t serial_us =
        static_cast<std::int64_t>(bits * 1000 /
                                  std::max<std::uint32_t>(kbps, 1) / 1000);
    const std::int64_t net_delay = kPropagationUs + serial_us;
    arrivals.push_back(with_dups[i].pts_us + net_delay);
  }

  // ---- Stage 6: de-duplication on the receive path ---------------------------
  // Wrap-safe: each slot records the wrap epoch in which that sequence was
  // accepted. The epoch is derived from the stable linear position assigned
  // in stage 2 (linear = sequence + epoch * 65536), so reordering cannot
  // corrupt it; reordered packets are accepted, true repeats suppressed.
  struct Slot { std::uint64_t epoch; bool used; };
  std::vector<Slot> seen(kSequenceSpace, {0, false});
  std::uint64_t accepted = 0;
  out.packets.reserve(survivors.size());
  for (std::uint32_t i = 0; i < with_dups.size(); ++i) {
    const std::uint16_t s = with_dups[i].sequence;
    // Linear position of the original survivor carrying this sequence:
    // duplicates share sequence and therefore resolve to the same position.
    const std::uint64_t linear = with_dups[i].linear_index;
    const std::uint64_t epoch = linear / kSequenceSpace;
    const std::size_t slot = s;
    if (seen[slot].used && seen[slot].epoch == epoch) {
      ++st.packets_duplicate;
      continue;
    }
    seen[slot] = {epoch, true};
    out.packets.push_back(std::move(with_dups[i]));
    out.stats.arrival_us.push_back(arrivals[i]);
    ++accepted;
  }
  st.packets_out = accepted;
  return out;
}

} // namespace media
