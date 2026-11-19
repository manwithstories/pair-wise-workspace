#include <algorithm>
#include <cmath>

#include "media_archive/timestamp_repair.h"

namespace media_archive {

namespace {
uint64_t median_nominal(const std::vector<TransportPacket>& packets,
                        uint64_t modulo) {
  std::vector<uint64_t> deltas;
  for (size_t i = 1; i < packets.size() && i <= 32; ++i) {
    const uint64_t prev = packets[i - 1].pts_ticks % modulo;
    const uint64_t cur = packets[i].pts_ticks % modulo;
    if (cur > prev) deltas.push_back(cur - prev);
  }
  if (deltas.empty()) return kTimebaseHz / 30;  // 30 fps fallback
  std::sort(deltas.begin(), deltas.end());
  return deltas[deltas.size() / 2];
}
}  // namespace

TimestampResult repair_timestamps(
    const std::vector<TransportPacket>& ordered,
    const std::vector<uint32_t>& gap_lengths,
    const TimestampConfig& cfg) {
  TimestampResult out;
  out.packets.reserve(ordered.size());
  if (ordered.empty()) return out;

  const uint64_t modulo = cfg.pts_modulo;
  const uint64_t nominal = cfg.nominal_frame_ticks != 0
                              ? cfg.nominal_frame_ticks
                              : median_nominal(ordered, modulo);
  out.nominal_frame_ticks = nominal;
  const double drift_scale = 1.0 + cfg.drift_ppm / 1'000'000.0;

  // Stage 1: unwrap the modular PTS, keeping a cumulative wrap counter.
  std::vector<uint64_t> unwrapped;
  unwrapped.reserve(ordered.size());
  uint64_t wraps = 0;
  uint64_t prev_raw = ordered.front().pts_ticks % modulo;
  for (const auto& p : ordered) {
    const uint64_t raw = p.pts_ticks % modulo;
    if (!unwrapped.empty() && prev_raw > raw &&
        (prev_raw - raw) > modulo / 2) {
      ++wraps;
      ++out.wrap_count;
    }
    unwrapped.push_back(raw + wraps * modulo);
    prev_raw = raw;
  }

  // Stage 2: remove clock drift while anchoring the first timestamp.
  const uint64_t anchor = unwrapped.front();
  std::vector<uint64_t> drifted;
  drifted.reserve(unwrapped.size());
  for (uint64_t u : unwrapped) {
    const double delta = static_cast<double>(u - anchor) / drift_scale;
    drifted.push_back(anchor + static_cast<uint64_t>(std::llround(delta)));
  }

  // Stage 3: detect anomalous jumps; correct inferable ones, flag the rest.
  RepairedPacket first;
  first.packet = ordered.front();
  first.repaired_pts = drifted.front();
  out.packets.push_back(first);

  for (size_t i = 1; i < ordered.size(); ++i) {
    RepairedPacket rp;
    rp.packet = ordered[i];
    const uint32_t gap_before =
        i < gap_lengths.size() ? gap_lengths[i] : 0;
    const uint64_t expected = nominal * (1u + gap_before);
    const int64_t actual =
        static_cast<int64_t>(drifted[i]) -
        static_cast<int64_t>(out.packets.back().repaired_pts);
    const bool anomalous =
        actual < 0 ||
        static_cast<uint64_t>(std::abs(
            static_cast<double>(actual) - static_cast<double>(expected))) >
            static_cast<uint64_t>(
                std::llround(cfg.jump_tolerance *
                              static_cast<double>(expected)));

    rp.repaired_pts = drifted[i];
    if (anomalous) {
      if (gap_before >= cfg.uncertainty_gap_threshold) {
        // Missing content makes the true elapsed time unknowable; keep the
        // received value and mark it instead of inventing a timestamp.
        rp.pts_uncertain = true;
        ++out.uncertain_count;
      } else {
        rp.repaired_pts =
            out.packets.back().repaired_pts + expected;
        ++out.corrected_jump_count;
      }
    }
    if (i > 0) {
      const uint64_t prev_raw2 = ordered[i - 1].pts_ticks % modulo;
      const uint64_t cur_raw = ordered[i].pts_ticks % modulo;
      rp.pts_wrapped = prev_raw2 > cur_raw &&
                       (prev_raw2 - cur_raw) > modulo / 2;
    }
    out.packets.push_back(rp);
  }
  return out;
}

}  // namespace media_archive
