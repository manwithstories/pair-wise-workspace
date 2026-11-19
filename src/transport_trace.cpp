#include <algorithm>
#include <cmath>

#include "media_archive/transport_trace.h"

namespace media_archive {

DeterministicRng::DeterministicRng(uint64_t seed)
    : state_(seed == 0 ? 0x9E3779B97F4A7C15ull : seed) {}

uint64_t DeterministicRng::next_u64() {
  uint64_t x = state_;
  x ^= x >> 12;
  x ^= x << 25;
  x ^= x >> 27;
  state_ = x;
  return x * 0x2545F4914F6CDD1Dull;
}

double DeterministicRng::next_unit() {
  return static_cast<double>(next_u64() >> 11) / static_cast<double>(1ull << 53);
}

uint32_t DeterministicRng::uniform(uint32_t lo, uint32_t hi) {
  if (hi <= lo) return lo;
  return lo + static_cast<uint32_t>(next_unit() * (hi - lo + 1));
}

double percentile(std::vector<double> values, double p) {
  if (values.empty()) return 0.0;
  std::sort(values.begin(), values.end());
  const double rank = p * static_cast<double>(values.size() - 1);
  const size_t lo = static_cast<size_t>(std::floor(rank));
  const size_t hi = static_cast<size_t>(std::ceil(rank));
  if (lo == hi) return values[lo];
  const double frac = rank - static_cast<double>(lo);
  return values[lo] * (1.0 - frac) + values[hi] * frac;
}

namespace {
// Time-varying bandwidth: mean scaled by a slow sinusoid plus jitter.
double bandwidth_at(size_t frame, const TraceConfig& cfg,
                    DeterministicRng& rng) {
  const double wave =
      0.75 + 0.25 * std::sin(static_cast<double>(frame) * 0.35);
  const double jitter = 0.85 + 0.3 * rng.next_unit();
  return std::max(128.0, cfg.mean_bandwidth_kbps * wave * jitter);
}
}  // namespace

TransportTrace build_transport_trace(uint32_t frame_count,
                                     const std::vector<uint32_t>& frame_sizes,
                                     const TraceConfig& cfg) {
  TransportTrace trace;
  DeterministicRng rng(cfg.seed);
  trace.generated_count = frame_count;
  trace.bandwidth_kbps.reserve(frame_count);
  trace.packets.reserve(frame_count * 2);

  double link_clock_ms = 0.0;   // time at which the link becomes free
  uint64_t next_offset = 0;     // running byte offset in the received stream
  uint32_t burst_remaining = 0; // ongoing burst loss
  uint32_t seq_raw = 0;

  for (uint32_t f = 0; f < frame_count; ++f) {
    const uint32_t size = f < frame_sizes.size() ? frame_sizes[f] : 1024;
    const double bw_kbps = bandwidth_at(f, cfg, rng);
    trace.bandwidth_kbps.push_back(bw_kbps);

    const double send_ms = f * cfg.frame_interval_ms;
    // Serialization delay on the current (varying) bandwidth.
    const double serial_ms =
        static_cast<double>(size) * 8.0 / (bw_kbps * 1000.0);
    link_clock_ms = std::max(link_clock_ms, send_ms) + serial_ms;
    // Small queueing jitter proportional to link pressure.
    const double queue_ms = 0.5 + 2.5 * rng.next_unit();

    TransportPacket pkt;
    pkt.seq = seq_raw;
    pkt.pts_ticks = static_cast<uint64_t>(
        std::llround(send_ms * kTimebaseHz / 1000.0));
    pkt.size_bytes = size;
    pkt.send_ts_ms = send_ms;

    const bool in_burst = burst_remaining > 0;
    const bool starts_burst =
        !in_burst && rng.next_unit() < cfg.burst_loss_prob;
    if (starts_burst)
      burst_remaining = rng.uniform(2, std::max(2u, cfg.max_burst_len));

    if (in_burst || burst_remaining > 0) {
      pkt.flag = PacketFlag::Gap;
      pkt.source_offset = -1;
      pkt.recv_ts_ms = 0.0;
      trace.gap_packet_count++;
      if (burst_remaining > 0) --burst_remaining;
    } else {
      pkt.flag = rng.next_unit() < cfg.reorder_prob
                     ? PacketFlag::Reordered
                     : PacketFlag::Ok;
      pkt.source_offset = static_cast<int64_t>(next_offset);
      next_offset += size;
      pkt.recv_ts_ms = link_clock_ms + queue_ms;
      trace.latencies_ms.push_back(pkt.recv_ts_ms - pkt.send_ts_ms);
    }
    trace.packets.push_back(pkt);

    // Duplicates share the original's sequence number and byte range.
    if (pkt.flag != PacketFlag::Gap &&
        rng.next_unit() < cfg.duplicate_prob) {
      TransportPacket dup = pkt;
      dup.flag = PacketFlag::Duplicate;
      dup.recv_ts_ms = pkt.recv_ts_ms + 1.0 + rng.next_unit() * 2.0;
      trace.packets.push_back(dup);
    }
    seq_raw = static_cast<uint32_t>((seq_raw + 1u) % kSeqModulo);
  }

  // Apply reordering to the arrival view: swap a few neighbours. Kept
  // independent of the gap marking so sequence repair can prove it works.
  for (size_t i = 1; i + 1 < trace.packets.size(); ++i) {
    if (trace.packets[i].flag == PacketFlag::Reordered &&
        trace.packets[i - 1].flag != PacketFlag::Gap) {
      std::swap(trace.packets[i - 1], trace.packets[i]);
      ++i;
    }
  }
  return trace;
}

}  // namespace media_archive
